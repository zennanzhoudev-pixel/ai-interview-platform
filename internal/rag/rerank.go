package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Reranker 对候选做精排。
//
// 召回阶段用宽松、快速的 BM25 与向量, 精排阶段用更贵但更准的方式
// 对 top-N 重排。这个两段式结构让"准"和"快"不必二选一。
type Reranker interface {
	Name() string
	Rerank(ctx context.Context, query string, docs []Doc) ([]Scored, error)
}

// LocalReranker 是确定性的本地精排: 词面命中率 + 术语精确命中加权。
//
// 它对"查询里的词在文档里出现得多不多、是不是整词命中"打分。
// 这是精排的基线, 不是语义精排; 需要更强的相关性判断时换 LLMReranker。
type LocalReranker struct{}

func NewLocalReranker() *LocalReranker { return &LocalReranker{} }
func (r *LocalReranker) Name() string  { return "local" }

func (r *LocalReranker) Rerank(_ context.Context, query string, docs []Doc) ([]Scored, error) {
	qt := Tokenize(query)
	out := make([]Scored, 0, len(docs))
	for _, d := range docs {
		docTokens := d.Tokens
		if len(docTokens) == 0 {
			docTokens = Tokenize(d.Text)
		}
		docSet := make(map[string]bool, len(docTokens))
		for _, t := range docTokens {
			docSet[t] = true
		}

		hits := 0
		exact := 0
		lowerText := strings.ToLower(d.Text)
		for _, q := range qt {
			if docSet[q] {
				hits++
			}
			if strings.Contains(lowerText, q) {
				exact++
			}
		}
		score := 0.0
		if len(qt) > 0 {
			score = float64(hits) / float64(len(qt))
		}
		score += float64(exact) * 0.1 // 整词命中再加一点
		out = append(out, Scored{ID: d.ID, Doc: d, Score: score})
	}
	sortScored(out)
	return out, nil
}

// LLMReranker 用大模型做精排(交叉编码器的廉价替代)。
//
// 它把候选文档编号后交给模型, 让模型只返回"最相关的序号"。
// 这是可行的工程近似: 参考题库很小, top-N 通常只有 5 到 10 篇,
// 一次调用就能精排, 而不必为每对(query, doc)单独打分。
type LLMReranker struct {
	// Rank 是具体实现: 输入 query 与候选列表, 输出按相关性排序后的 ID。
	Rank func(ctx context.Context, query string, docs []Doc) ([]string, error)
}

func (r *LLMReranker) Name() string { return "llm" }

func (r *LLMReranker) Rerank(ctx context.Context, query string, docs []Doc) ([]Scored, error) {
	if r.Rank == nil {
		return nil, fmt.Errorf("LLMReranker 未配置 Rank 实现")
	}
	ordered, err := r.Rank(ctx, query, docs)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Doc, len(docs))
	for _, d := range docs {
		byID[d.ID] = d
	}
	out := make([]Scored, 0, len(ordered))
	for rank, id := range ordered {
		if d, ok := byID[id]; ok {
			out = append(out, Scored{ID: id, Doc: d, Score: float64(len(ordered) - rank)})
		}
	}
	return out, nil
}

func sortScored(list []Scored) {
	sort.Slice(list, func(a, b int) bool {
		if list[a].Score == list[b].Score {
			return list[a].ID < list[b].ID
		}
		return list[a].Score > list[b].Score
	})
}
