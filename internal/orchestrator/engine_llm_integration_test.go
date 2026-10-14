package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
)

// TestEngineUsesRealLLMEndToEnd 用真实模型把"面试官提问 → 评分 → 追问"
// 整条链路跑一遍, 证明配了密钥后引擎确实用 LLM 在思考和追问, 而不是规则兜底。
//
// 未配置密钥就跳过(与其它集成测试同款约定)。
func TestEngineUsesRealLLMEndToEnd(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("未设置 LLM_API_KEY, 跳过真实模型端到端测试")
	}

	ctx := context.Background()
	client := llm.NewClient(llm.FromEnv())

	refs := DefaultReferenceBank()
	retriever, err := refs.BuildRetriever(ctx, rag.NewHashingEmbedder(256), rag.NewLocalReranker())
	if err != nil {
		t.Fatalf("构建检索器失败: %v", err)
	}

	primary := scoring.NewChainScorer(
		scoring.NewLLMScorer(client),
		scoring.NewKeywordScorer("rule-fallback", 0),
	)
	secondary := scoring.NewKeywordScorer("rule-reviewer", 0)
	arbiter := scoring.NewKeywordScorer("rule-arbiter", 0)

	eng := NewEngine(DefaultPlan(time.Minute), DefaultBank(), time.Minute,
		WithScorers(primary, secondary, arbiter, 1),
		WithProbePlanner(NewRAGProbePlanner(refs, retriever)),
		WithFollowUpPhraser(NewLLMFollowUpPhraser(client)),
	)

	// 刻意给一个"没覆盖任何要点"的回答, 让追问必然被触发。
	const answer = "这个我了解一些，主要是照着文档和示例做的，具体细节记不太清了。"
	llmScored := false
	var probes []string

	d := eng.Start()
	for i := 0; i < 4 && d.Action != ActionFinish; i++ {
		if d.IsProbe {
			probes = append(probes, d.Question)
		}
		next, err := eng.Submit(answer, 30*time.Second)
		if err != nil {
			t.Fatalf("提交回答失败: %v", err)
		}
		if next.Turn != nil && next.Turn.Verdict != nil {
			v := next.Turn.Verdict.Primary
			if strings.HasPrefix(v.Model, "llm:") {
				llmScored = true
				t.Logf("第 %d 轮 | 评分来源=%s | 等级=%s | 理由=%s",
					next.Turn.Index, v.Model, v.LevelName, v.Rationale)
			} else {
				t.Logf("第 %d 轮 | 评分来源=%s(规则兜底)", next.Turn.Index, v.Model)
			}
		}
		d = next
	}

	if !llmScored {
		t.Fatal("端到端面试中没有任何一轮评分来自真实大模型, 全部规则兜底")
	}
	for _, p := range probes {
		t.Logf("追问措辞 = %s", p)
	}
	t.Logf("LLM 追问轮数 = %d", len(probes))
}
