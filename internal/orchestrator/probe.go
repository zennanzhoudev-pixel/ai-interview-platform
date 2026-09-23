package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
)

// Probe 描述一次追问的靶子。
type Probe struct {
	Focus     string // 追问的要点名
	Reference string // 参考答案里的表述, 用于生成贴合原文的追问
}

// ProbePlanner 决定"追问什么"。
//
// 默认行为只看回答里缺失的判定要点; RAG 实现会检索参考答案,
// 挑出候选人既没覆盖、又被参考答案强调的要点来追问。两者都实现同一接口,
// 引擎不关心具体是哪种 —— 这是把"追问策略"从"面试流程"里拆出来。
type ProbePlanner interface {
	Name() string
	PlanProbe(parent Question, answer string, res scoring.Result) (Probe, bool)
}

// RAGProbePlanner 用混合检索驱动追问。
type RAGProbePlanner struct {
	bank      *ReferenceBank
	retriever *rag.Retriever
	timeout   time.Duration
}

// NewRAGProbePlanner 构造检索驱动的追问规划器。
func NewRAGProbePlanner(bank *ReferenceBank, retriever *rag.Retriever) *RAGProbePlanner {
	return &RAGProbePlanner{bank: bank, retriever: retriever, timeout: 5 * time.Second}
}

func (p *RAGProbePlanner) Name() string { return "rag" }

// PlanProbe 从参考答案里挑一个"候选人还没覆盖"的要点。
func (p *RAGProbePlanner) PlanProbe(parent Question, answer string, _ scoring.Result) (Probe, bool) {
	ref, ok := p.bank.ByQuestionID(parent.ID)
	if !ok || len(ref.Points) == 0 {
		return Probe{}, false
	}

	var missed []ReferencePoint
	for _, pt := range ref.Points {
		if !pointCovered(pt, answer) {
			missed = append(missed, pt)
		}
	}
	if len(missed) == 0 {
		return Probe{}, false
	}

	best := p.pickMostRelevant(parent, answer, missed)
	return Probe{Focus: best.Key, Reference: best.Text}, true
}

// pointCovered 判断回答是否覆盖了某个要点: 要点文本的 token 在回答里
// 出现的比例足够高即认为覆盖。阈值取 0.5 —— 说对一半以上算"说到了"。
func pointCovered(pt ReferencePoint, answer string) bool {
	tokens := rag.Tokenize(pt.Text)
	if len(tokens) == 0 {
		return false
	}
	lower := strings.ToLower(answer)
	hit := 0
	for _, t := range tokens {
		if strings.Contains(lower, t) {
			hit++
		}
	}
	return float64(hit)/float64(len(tokens)) >= 0.5
}

// pickMostRelevant 用检索按相关性给"缺失的要点"排序, 取最该问的那个。
//
// 检索查询是"题目 + 回答", 让相关性贴合当前上下文; 返回的文档是参考答案
// 要点, 再过滤出其中属于"缺失"的那部分。这样追问既踩在缺口上,
// 又由参考答案的检索排序决定优先问哪个。
func (p *RAGProbePlanner) pickMostRelevant(parent Question, answer string, missed []ReferencePoint) ReferencePoint {
	if p.retriever == nil {
		return missed[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	results, err := p.retriever.Search(ctx, parent.Text+" "+answer)
	if err != nil || len(results) == 0 {
		return missed[0]
	}

	missedByKey := make(map[string]ReferencePoint, len(missed))
	for _, m := range missed {
		missedByKey[m.Key] = m
	}
	for _, r := range results {
		if key := r.Doc.Meta["point_key"]; key != "" {
			if m, ok := missedByKey[key]; ok {
				return m
			}
		}
	}
	return missed[0]
}
