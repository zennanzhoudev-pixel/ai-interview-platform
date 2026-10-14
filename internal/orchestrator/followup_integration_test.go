package orchestrator

import (
	"os"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

// TestLLMFollowUpPhraserAgainstRealModel 用真实厂商模型验证追问措辞。
//
// 未配置密钥就跳过(与 MySQL/Redis 集成测试同款约定)。真实环境里:
//
//	LLM_BASE_URL=... LLM_API_KEY=... LLM_MODEL=... LLM_API_MODE=chat \
//	  go test ./internal/orchestrator/ -run RealModel -v
func TestLLMFollowUpPhraserAgainstRealModel(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("未设置 LLM_API_KEY, 跳过真实模型追问集成测试")
	}

	const reference = "内存增长、过期清理与大 key 分片"
	p := NewLLMFollowUpPhraser(llm.NewClient(llm.FromEnv()))
	q, err := p.Phrase(FollowUpContext{
		ParentQuestion:  "ZSet 索引是怎么做的?",
		CandidateAnswer: "我把排行榜拆成 ZSet 索引, 用 score 存权重, 查询 RT 降了 70%。",
		Focus:           "内存",
		Reference:       reference,
	})
	if err != nil {
		t.Fatalf("真实模型追问生成失败: %v", err)
	}
	if q == "" {
		t.Fatal("真实模型应生成非空追问")
	}
	if !followUpAnchored(reference, q) {
		t.Fatalf("真实模型追问偏离参考答案: %q", q)
	}
	t.Logf("追问 = %q", q)
}
