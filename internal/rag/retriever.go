package rag

import (
	"context"
	"fmt"
)

// Retriever 组合 BM25 + 向量 + RRF + 精排, 是混合检索的统一入口。
//
// 调用方只需要 AddDoc / Search, 不必关心内部是几路召回、怎么融合。
// 这层抽象的意义在于: 以后把向量索引换成向量数据库、把精排换成
// 专用模型, 上层(追问规划)一行都不用改。
type Retriever struct {
	bm25       *BM25
	vec        *VectorIndex
	reranker   Reranker
	rrfK       int
	candidateN int // 每路召回取多少, 送入融合
	topK       int // 精排后返回多少
}

// RetrieverOption 配置检索器。
type RetrieverOption func(*Retriever)

// WithTopK 设置最终返回条数。
func WithTopK(n int) RetrieverOption {
	return func(r *Retriever) { r.topK = n }
}

// WithCandidates 设置每路召回的候选数。
func WithCandidates(n int) RetrieverOption {
	return func(r *Retriever) { r.candidateN = n }
}

// NewRetriever 构造混合检索器。
func NewRetriever(emb Embedder, reranker Reranker, opts ...RetrieverOption) *Retriever {
	if reranker == nil {
		reranker = NewLocalReranker()
	}
	r := &Retriever{
		bm25:       NewBM25(1.5, 0.75),
		vec:        NewVectorIndex(emb),
		reranker:   reranker,
		rrfK:       60,
		candidateN: 20,
		topK:       5,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AddDoc 把一篇文档同时加入 BM25 与向量索引。
func (r *Retriever) AddDoc(ctx context.Context, doc Doc) error {
	if doc.ID == "" || doc.Text == "" {
		return fmt.Errorf("rag: 文档 ID 与正文不能为空")
	}
	r.bm25.Add(doc)
	return r.vec.Add(ctx, doc)
}

// Search 返回与 query 最相关的 top-K 篇文档。
func (r *Retriever) Search(ctx context.Context, query string) ([]Scored, error) {
	bm := r.bm25.Search(query, r.candidateN)
	vv := r.vec.Search(ctx, query, r.candidateN)

	fused := RRF(r.rrfK, bm, vv)
	if len(fused) == 0 {
		return nil, nil
	}

	// 融合后取 top-K 送入精排; 精排数量太多会让本地精排变慢、模型精排变贵。
	rerankN := r.topK * 2
	if len(fused) < rerankN {
		rerankN = len(fused)
	}
	candidates := make([]Doc, 0, rerankN)
	for _, f := range fused[:rerankN] {
		candidates = append(candidates, f.Doc)
	}

	reranked, err := r.reranker.Rerank(ctx, query, candidates)
	if err != nil {
		return nil, err
	}
	if len(reranked) > r.topK {
		reranked = reranked[:r.topK]
	}
	return reranked, nil
}

// DocCount 返回索引中的文档总数。
func (r *Retriever) DocCount() int { return r.bm25.DocCount() }
