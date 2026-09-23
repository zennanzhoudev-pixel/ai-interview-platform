package scoring

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

// mockLLM 是一个 OpenAI 兼容的假服务: 按顺序返回预设的"模型输出"。
// 用它可以把"模型不听话"的各种情形(越界、编造证据、上游挂掉)
// 变成可重复的测试用例, 而不是靠运气在生产环境里偶遇。
type mockLLM struct {
	srv       *httptest.Server
	responses []string
	status    int

	mu     sync.Mutex
	calls  int
	bodies []string
}

func newMockLLM(responses ...string) *mockLLM {
	m := &mockLLM{responses: responses, status: http.StatusOK}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		idx := m.calls
		m.calls++
		m.bodies = append(m.bodies, string(body))
		status := m.status
		responses := m.responses
		m.mu.Unlock()

		if status != http.StatusOK {
			http.Error(w, `{"error":"upstream down"}`, status)
			return
		}
		if len(responses) == 0 {
			http.Error(w, "no response configured", http.StatusInternalServerError)
			return
		}
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"content": responses[idx]},
			}},
			"usage": map[string]int{
				"prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	return m
}

func (m *mockLLM) client() *llm.Client {
	// MaxRetries 设为 0: 传输层重试由 llm 包的测试覆盖,
	// 这里要精确观察"schema 重试"的次数。
	return llm.NewClient(llm.Config{
		BaseURL: m.srv.URL, APIKey: "k", Model: "mock-model", MaxRetries: 0,
	})
}

func (m *mockLLM) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockLLM) lastBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[len(m.bodies)-1]
}

func (m *mockLLM) close() { m.srv.Close() }

func sampleAnswer() Answer {
	return Answer{
		QuestionID: "q_resume_zset",
		Competency: "distributed_system",
		Question:   "ZSet 索引是怎么做的?",
		Text:       "我们把排行榜拆成 ZSet 索引, 用 score 存权重, 查询 RT 降了 70%。",
		Keywords:   []string{"ZSet", "score", "70%"},
	}
}

func TestLLMScorerScoresWithVerifiableEvidence(t *testing.T) {
	m := newMockLLM(`{"level":4,"matched":["ZSet","score"],"missing":["内存"],` +
		`"evidence":[{"quote":"排行榜拆成 ZSet 索引","matched":"ZSet"}],` +
		`"confidence":0.8,"rationale":"能说明结构选择与收益"}`)
	defer m.close()

	s := NewLLMScorer(m.client())
	got := s.Score(sampleAnswer())

	if !got.Valid() {
		t.Fatalf("合规输出应产出有效评分, 实际 %+v", got)
	}
	if got.Level != LevelAdvanced {
		t.Fatalf("level 4 应映射为 L4 精通, 实际 %s", got.Level)
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("应保留 1 条可核实的证据, 实际 %d 条", len(got.Evidence))
	}
	if got.Evidence[0].Quote != "排行榜拆成 ZSet 索引" {
		t.Fatalf("证据原话错误: %q", got.Evidence[0].Quote)
	}
	if got.Evidence[0].QuestionID != "q_resume_zset" || got.Evidence[0].Kind != EvidenceSupport {
		t.Fatalf("证据应绑定题目且标记为支持性证据: %+v", got.Evidence[0])
	}

	usage := s.Usage()
	if usage.Calls != 1 || usage.PromptTokens != 100 || usage.CompletionTokens != 40 {
		t.Fatalf("消耗统计错误: %+v", usage)
	}
}

// 这是抗幻觉机制的核心用例: 模型给出的"证据"在候选人回答里根本不存在。
func TestLLMScorerRejectsFabricatedEvidenceAndFeedsBack(t *testing.T) {
	fabricated := `{"level":5,"matched":["ZSet"],` +
		`"evidence":[{"quote":"我用了 Redis Cluster 做主从切换","matched":"ZSet"}],` +
		`"confidence":0.9,"rationale":"候选人水平很高"}`
	m := newMockLLM(fabricated)
	defer m.close()

	s := NewLLMScorer(m.client(), WithSchemaRetries(1))
	got := s.Score(sampleAnswer())

	if got.Valid() {
		t.Fatalf("编造证据的评分不应生效, 实际 %+v", got)
	}
	if m.callCount() != 2 {
		t.Fatalf("应在发现编造证据后再问一次, 实际调用 %d 次", m.callCount())
	}
	if !strings.Contains(m.lastBody(), "编造") {
		t.Fatalf("重试时应把具体违规点回灌给模型, 实际请求体里没有说明: %s", m.lastBody())
	}
}

func TestLLMScorerRetriesOnSchemaViolationThenSucceeds(t *testing.T) {
	m := newMockLLM(
		`{"level":9,"rationale":"等级越界"}`,
		`{"level":3,"matched":["ZSet"],`+
			`"evidence":[{"quote":"用 score 存权重","matched":"score"}],`+
			`"confidence":0.7,"rationale":"部分命中"}`,
	)
	defer m.close()

	s := NewLLMScorer(m.client(), WithSchemaRetries(1))
	got := s.Score(sampleAnswer())

	if !got.Valid() {
		t.Fatalf("第二次输出合规时应产出有效评分, 实际 %+v", got)
	}
	if got.Level != LevelProficient {
		t.Fatalf("level 3 应映射为 L3 熟练, 实际 %s", got.Level)
	}
	if m.callCount() != 2 {
		t.Fatalf("应重试一次, 实际调用 %d 次", m.callCount())
	}
	if !strings.Contains(m.lastBody(), "应在 [1, 5] 范围内") {
		t.Fatalf("重试时应说明具体的 schema 违规点: %s", m.lastBody())
	}
}

func TestLLMScorerTreatsMissingEvidenceAsInvalid(t *testing.T) {
	m := newMockLLM(`{"level":5,"matched":["ZSet"],"evidence":[],"confidence":0.9,"rationale":"很强"}`)
	defer m.close()

	s := NewLLMScorer(m.client(), WithSchemaRetries(1))
	got := s.Score(sampleAnswer())

	if got.Valid() {
		t.Fatal("没有任何证据的评分必须作废")
	}
	if m.callCount() != 2 {
		t.Fatalf("应重试一次, 实际 %d 次", m.callCount())
	}
}

// 反例由本地再核对一遍: 不指望模型每次都记得"命中反例要降级"。
func TestLLMScorerForcesDowngradeOnAntiPattern(t *testing.T) {
	a := sampleAnswer()
	a.AntiPatterns = []string{"不用管重复消费"}
	a.Text = "MQ 重复消费不用管重复消费, 业务侧自己兜就行, 我们一直是这么做的。"

	m := newMockLLM(`{"level":5,"matched":["ZSet"],` +
		`"evidence":[{"quote":"业务侧自己兜就行","matched":"ZSet"}],` +
		`"confidence":0.95,"rationale":"表达清晰, 经验丰富"}`)
	defer m.close()

	s := NewLLMScorer(m.client())
	got := s.Score(a)

	if got.Level != LevelNone {
		t.Fatalf("命中断言反例必须降到最低等级, 实际 %s", got.Level)
	}
	if len(got.AntiHits) != 1 {
		t.Fatalf("应记录命中的反例, 实际 %v", got.AntiHits)
	}
}

func TestLLMScorerAcceptsQuoteWithDifferentSpacing(t *testing.T) {
	a := sampleAnswer()
	m := newMockLLM(`{"level":4,"matched":["ZSet"],` +
		`"evidence":[{"quote":"排行榜 拆成 ZSet 索引","matched":"ZSet"}],` +
		`"confidence":0.8,"rationale":"ok"}`)
	defer m.close()

	got := NewLLMScorer(m.client()).Score(a)
	if !got.Valid() {
		t.Fatal("仅空白差异不应被判定为编造证据")
	}
}

func TestLLMScorerHalvesConfidenceOnPartialFabrication(t *testing.T) {
	m := newMockLLM(`{"level":4,"matched":["ZSet","score"],` +
		`"evidence":[{"quote":"用 score 存权重","matched":"score"},` +
		`{"quote":"我们还做了多机房容灾","matched":"容灾"}],` +
		`"confidence":0.8,"rationale":"ok"}`)
	defer m.close()

	got := NewLLMScorer(m.client()).Score(sampleAnswer())

	if !got.Valid() {
		t.Fatal("只要还有可核实证据, 评分就应保留")
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("不可核实的证据应被丢弃, 实际保留 %d 条", len(got.Evidence))
	}
	if got.Confidence != 0.4 {
		t.Fatalf("存在被丢弃证据时置信度应减半, 实际 %v", got.Confidence)
	}
	if !strings.Contains(got.Rationale, "丢弃不可核实证据") {
		t.Fatalf("理由里应说明证据被打折: %q", got.Rationale)
	}
}

func TestLLMScorerReturnsInvalidWhenUpstreamFails(t *testing.T) {
	m := newMockLLM("ignored")
	m.status = http.StatusInternalServerError
	defer m.close()

	got := NewLLMScorer(m.client(), WithSchemaRetries(1)).Score(sampleAnswer())
	if got.Valid() {
		t.Fatal("上游不可用时应产出无效结果, 交由降级链处理")
	}
	if !strings.Contains(got.Rationale, "大模型评分失败") {
		t.Fatalf("理由应说明失败原因: %q", got.Rationale)
	}
	// 传输层重试由 llm.Client 负责, 评分器不应再叠一层。
	if m.callCount() != 1 {
		t.Fatalf("传输层失败不应触发 schema 重试, 实际调用 %d 次", m.callCount())
	}
}

func TestLLMScorerWithoutClientIsInvalid(t *testing.T) {
	got := NewLLMScorer(nil).Score(sampleAnswer())
	if got.Valid() {
		t.Fatal("未配置客户端时应产出无效结果")
	}
}

func TestChainScorerFallsBackToRuleScorer(t *testing.T) {
	var from, to, cause string
	chain := NewChainScorer(
		stubScorer{name: "llm", level: LevelExpert, hasProof: false},
		NewKeywordScorer("rule", 0),
	)
	chain.OnFallback = func(p, f, c string) { from, to, cause = p, f, c }

	got := chain.Score(Answer{
		QuestionID: "q1",
		Text:       "用了 ZSet 和 score",
		Keywords:   []string{"ZSet", "score"},
	})

	if !got.Valid() {
		t.Fatalf("主评分器失效时应由规则评分器兜底, 实际 %+v", got)
	}
	if got.DegradedFrom == "" {
		t.Fatal("降级必须被记录下来, 否则业务方不会知道评分标准变了")
	}
	if from != "llm" || to != "rule" {
		t.Fatalf("降级回调参数错误: from=%q to=%q", from, to)
	}
	if !strings.Contains(cause, "未产出有效评分") {
		t.Fatalf("降级原因应可读: %q", cause)
	}
	if !strings.Contains(got.Rationale, "已降级") {
		t.Fatalf("结果理由里应体现降级: %q", got.Rationale)
	}
}

func TestChainScorerUsesPrimaryWhenValid(t *testing.T) {
	chain := NewChainScorer(
		stubScorer{name: "llm", level: LevelProficient, hasProof: true},
		NewKeywordScorer("rule", 0),
	)
	got := chain.Score(sampleAnswer())

	if got.Level != LevelProficient {
		t.Fatalf("主评分器可用时应直接采用, 实际 %s", got.Level)
	}
	if got.DegradedFrom != "" {
		t.Fatalf("未发生降级时不应留下降级标记: %q", got.DegradedFrom)
	}
}

func TestChainScorerReturnsInvalidWhenAllScorersFail(t *testing.T) {
	chain := NewChainScorer(
		stubScorer{name: "a", level: LevelExpert, hasProof: false},
		stubScorer{name: "b", level: LevelExpert, hasProof: false},
	)
	got := chain.Score(sampleAnswer())

	if got.Valid() {
		t.Fatal("全部失败时必须产出无效结果, 让上层看到'这条没有分'")
	}
	if !strings.Contains(got.Rationale, "所有评分器") {
		t.Fatalf("理由应说明全部失败: %q", got.Rationale)
	}
}
