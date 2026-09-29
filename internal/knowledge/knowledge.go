// Package knowledge 把"题库"变成可检索的参考知识库。
//
// 它承担三件事, 而且这三件事必须由同一份数据驱动:
//
//  1. **提问**: 按阶段从题目里抽题, 组成一场面试;
//  2. **追问**: 把参考答案拆成要点建索引, 检索出"候选人没说到、但参考答案
//     强调"的那一点作为追问方向;
//  3. **解释**: 检索结果可以被接口直接查看 —— 面试官能看到"AI 为什么
//     挑了这个方向追问", 这是把 RAG 从黑盒变成可复核的关键一步。
//
// 如果这三者各用一份数据, 报告里的评分依据迟早会和实际问的问题对不上。
package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// Hit 是一条检索结果。
//
// 它带着"这条结果是怎么被选出来的": 类型(题目还是参考要点)、属于哪道题、
// 哪一段检索链路产出。面试官质疑"AI 为什么追问这个"时, 能直接看到答案。
type Hit struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"` // reference_point / question
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	QuestionID string  `json:"question_id"`
	PointKey   string  `json:"point_key,omitempty"`
	Stage      string  `json:"stage,omitempty"`
	Competency string  `json:"competency,omitempty"`
}

// Stats 描述当前知识库的构成。
type Stats struct {
	Questions int       `json:"questions"`
	Points    int       `json:"points"`
	Documents int       `json:"documents"`
	Embedder  string    `json:"embedder"`
	Reranker  string    `json:"reranker"`
	Source    string    `json:"source"`
	BuiltAt   time.Time `json:"built_at"`
	BuildMS   int64     `json:"build_ms"`
	Pipeline  string    `json:"pipeline"`
}

// Corpus 是一份可检索的知识库。
//
// 读写用 RWMutex 而不是"启动时构建一次就不再变": 题库是管理界面里
// 可以随时改的数据, 改完必须能立刻生效, 否则面试官会以为"发布没成功"。
type Corpus struct {
	mu        sync.RWMutex
	bank      *orchestrator.Bank
	refs      *orchestrator.ReferenceBank
	retriever *rag.Retriever
	embedder  rag.Embedder
	reranker  rag.Reranker
	stats     Stats
}

// Build 从题目集合构造知识库。
//
// tenantItems 是租户在题库管理里维护的题目; 内置题目永远参与构建,
// 因为开场、反问、收尾这几个"流程骨架"阶段如果没有可用题目,
// 面试会直接跳过整段流程 —— 而使用者只会看到"面试好像少了点什么"。
func Build(ctx context.Context, tenantItems []store.QuestionItem, emb rag.Embedder, reranker rag.Reranker) (*Corpus, error) {
	start := time.Now()
	if emb == nil {
		emb = rag.NewHashingEmbedder(256)
	}
	if reranker == nil {
		reranker = rag.NewLocalReranker()
	}

	items, source := mergeItems(tenantItems, SeedItems())
	questions := make([]orchestrator.Question, 0, len(items))
	answers := make([]orchestrator.ReferenceAnswer, 0, len(items))
	retriever := rag.NewRetriever(emb, reranker, rag.WithTopK(5), rag.WithCandidates(20))

	points := 0
	docs := 0
	for _, it := range items {
		questions = append(questions, toQuestion(it))

		ref := orchestrator.ReferenceAnswer{QuestionID: it.ID, Summary: it.Text}
		for _, p := range it.ReferencePoints {
			key := strings.TrimSpace(p.Key)
			text := strings.TrimSpace(p.Text)
			if key == "" || text == "" {
				continue
			}
			ref.Points = append(ref.Points, orchestrator.ReferencePoint{Key: key, Text: text})
			if err := retriever.AddDoc(ctx, rag.Doc{
				ID:   it.ID + "::" + key,
				Text: text,
				Meta: map[string]string{
					"kind": "reference_point", "question_id": it.ID, "point_key": key,
					"stage": it.Stage, "competency": it.Competency,
				},
			}); err != nil {
				return nil, fmt.Errorf("knowledge: 索引参考要点失败: %w", err)
			}
			points++
			docs++
		}
		answers = append(answers, ref)

		// 题目正文也进索引: 面试官在检索框里搜"幂等"时, 期望看到的是
		// 相关题目, 而不只是孤立的要点片段。
		if err := retriever.AddDoc(ctx, rag.Doc{
			ID:   it.ID,
			Text: it.Text,
			Meta: map[string]string{
				"kind": "question", "question_id": it.ID,
				"stage": it.Stage, "competency": it.Competency,
			},
		}); err != nil {
			return nil, fmt.Errorf("knowledge: 索引题目失败: %w", err)
		}
		docs++
	}

	c := &Corpus{
		bank:      orchestrator.NewBank(questions...),
		refs:      orchestrator.NewReferenceBank(answers...),
		retriever: retriever,
		embedder:  emb,
		reranker:  reranker,
	}
	c.stats = Stats{
		Questions: len(items),
		Points:    points,
		Documents: docs,
		Embedder:  emb.Name(),
		Reranker:  reranker.Name(),
		Source:    source,
		BuiltAt:   time.Now().UTC(),
		BuildMS:   time.Since(start).Milliseconds(),
		Pipeline:  "BM25 + 向量 + RRF(k=60) + 精排",
	}
	return c, nil
}

// mergeItems 把租户题目与内置题目合并。
//
// 排序规则有意为之: 租户题目排在前面。题库管理里"发布了一道题"是一个
// 有意图的动作, 抽题时它应该优先于内置演示题; 而内置题只负责补齐
// 租户没覆盖到的阶段。
func mergeItems(tenant, builtin []store.QuestionItem) ([]store.QuestionItem, string) {
	seen := make(map[string]bool, len(tenant)+len(builtin))
	out := make([]store.QuestionItem, 0, len(tenant)+len(builtin))

	tenantCount := 0
	for _, it := range tenant {
		if !usable(it) || seen[it.ID] {
			continue
		}
		it.Status = "published"
		seen[it.ID] = true
		out = append(out, it)
		tenantCount++
	}
	builtinCount := 0
	for _, it := range builtin {
		if !usable(it) || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
		builtinCount++
	}

	source := fmt.Sprintf("内置 %d 题", builtinCount)
	if tenantCount > 0 {
		source = fmt.Sprintf("租户 %d 题 + %s", tenantCount, source)
	}
	return out, source
}

// usable 判断一道题是否可以进入面试。
func usable(it store.QuestionItem) bool {
	if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Text) == "" {
		return false
	}
	if it.Stage == "" {
		return false
	}
	switch it.Status {
	case "draft", "retired":
		return false
	}
	return true
}

// toQuestion 把题库条目转成引擎的题目结构。
func toQuestion(it store.QuestionItem) orchestrator.Question {
	maxProbe := it.MaxProbe
	if maxProbe < 0 {
		maxProbe = 0
	}
	if maxProbe == 0 && it.Importance == "high" {
		maxProbe = 2
	}
	return orchestrator.Question{
		ID:           it.ID,
		Stage:        orchestrator.Stage(it.Stage),
		Competency:   it.Competency,
		Text:         it.Text,
		Keywords:     append([]string(nil), it.Keywords...),
		AntiPatterns: append([]string(nil), it.AntiPatterns...),
		Importance:   parseImportance(it.Importance),
		MaxProbe:     maxProbe,
	}
}

func parseImportance(v string) orchestrator.Importance {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "high":
		return orchestrator.ImportanceHigh
	case "low":
		return orchestrator.ImportanceLow
	default:
		return orchestrator.ImportanceMedium
	}
}

// Bank 返回用于抽题的题库。
func (c *Corpus) Bank() *orchestrator.Bank {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bank
}

// ReferenceBank 返回参考答案库。
func (c *Corpus) ReferenceBank() *orchestrator.ReferenceBank {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refs
}

// Planner 返回检索驱动的追问规划器。
func (c *Corpus) Planner() orchestrator.ProbePlanner {
	return orchestrator.NewRAGProbePlanner(c.refs, c.retriever)
}

// Stats 返回知识库构成。
func (c *Corpus) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// Search 执行混合检索, 返回带来源信息的结果。
//
// 这是"RAG 到底有没有在工作"最直接的证据: 同一个索引既用于追问决策,
// 也用于这个接口。如果这个接口返回的结果不相关, 那追问也不会好。
func (c *Corpus) Search(ctx context.Context, query string, topK int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("knowledge: 检索词不能为空")
	}
	if topK <= 0 {
		topK = 5
	}
	if topK > 20 {
		topK = 20
	}

	c.mu.RLock()
	retriever := c.retriever
	c.mu.RUnlock()

	scored, err := retriever.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(scored))
	for _, s := range scored {
		hit := Hit{
			ID:         s.ID,
			Text:       s.Doc.Text,
			Score:      s.Score,
			QuestionID: s.Doc.Meta["question_id"],
			PointKey:   s.Doc.Meta["point_key"],
			Stage:      s.Doc.Meta["stage"],
			Competency: s.Doc.Meta["competency"],
			Kind:       s.Doc.Meta["kind"],
		}
		if hit.Kind == "" {
			hit.Kind = "reference_point"
		}
		out = append(out, hit)
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// Stages 返回知识库覆盖到的阶段(按面试流程顺序)。
func (c *Corpus) Stages() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	present := make(map[string]bool)
	for _, q := range c.bank.All() {
		present[string(q.Stage)] = true
	}
	out := make([]string, 0, len(present))
	for _, st := range orchestrator.StageOrder {
		if present[string(st)] {
			out = append(out, string(st))
		}
	}
	return out
}

// MissingStructuralStages 返回缺少题目的流程骨架阶段。
//
// 它是给管理界面用的自检: 题库被改坏(比如把开场题全下线)时,
// 应该在题库页面上直接报警, 而不是等到面试跑完才发现整个过程不自然。
func (c *Corpus) MissingStructuralStages() []string {
	required := []orchestrator.Stage{
		orchestrator.StageGreeting, orchestrator.StageResumeDeepDive,
		orchestrator.StageTechFundamental, orchestrator.StageCandidateQA,
		orchestrator.StageWrapUp,
	}
	have := make(map[string]bool)
	for _, s := range c.Stages() {
		have[s] = true
	}
	var missing []string
	for _, st := range required {
		if !have[string(st)] {
			missing = append(missing, string(st))
		}
	}
	sort.Strings(missing)
	return missing
}
