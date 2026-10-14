package scoring

import (
	"os"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

// TestLLMScorerAgainstRealModel 用真实厂商模型验证"AI 面试官真的会评分"。
//
// 与 MySQL / Redis 集成测试同款约定: 未配置密钥就跳过, 绝不让 CI
// 因为没有外部依赖而红。真实环境里这样跑:
//
//	LLM_BASE_URL=... LLM_API_KEY=... LLM_MODEL=... LLM_API_MODE=chat \
//	  go test ./internal/scoring/ -run RealModel -v
func TestLLMScorerAgainstRealModel(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("未设置 LLM_API_KEY, 跳过真实模型评分集成测试")
	}

	s := NewLLMScorer(llm.NewClient(llm.FromEnv()))
	got := s.Score(sampleAnswer())

	if !got.Valid() {
		t.Fatalf("真实模型评分应有效, 实际: %+v", got)
	}
	if got.Rationale == "" {
		t.Error("真实模型应给出评分理由")
	}
	if got.DegradedFrom != "" {
		t.Errorf("不应降级到规则评分, 实际降级自 %s", got.DegradedFrom)
	}
	t.Logf("模型=%s 等级=%s 置信度=%.2f 理由=%s",
		got.Model, got.LevelName, got.Confidence, got.Rationale)
}
