package resume

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

const sampleResume = `陈雨
高级后端工程师

技能: Go, Redis, Python

项目经历
- IM 对话平台 2023.06 - 2024.03, 负责 Redis 存储改造与 ZSet 索引优化
- 个人项目 2024.06 - 2023.03, 时间区间倒挂的示例
`

func entitiesOf(r *Resume, kind EntityKind) []Entity {
	var out []Entity
	for _, e := range r.Entities {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestParseTracksRuneOffsets(t *testing.T) {
	r := Parse(sampleResume)
	if len(r.Blocks) == 0 {
		t.Fatal("应切出若干块")
	}
	// 第一块是姓名
	if r.Blocks[0].Text != "陈雨" {
		t.Fatalf("第一块应为姓名, 实际 %q", r.Blocks[0].Text)
	}
}

// 这是"原文定位"的核心: 抽取结果必须能精确回到原文的原话。
func TestRuleExtractorLocatesEntitiesInSource(t *testing.T) {
	r := NewRuleExtractor().Extract(sampleResume)

	var zset *Entity
	for _, e := range entitiesOf(r, KindSkill) {
		if e.Value == "ZSet" {
			zset = &e
			break
		}
	}
	if zset == nil {
		t.Fatal("应抽到技能 ZSet")
	}
	if got := r.Quote(*zset); got != "ZSet" {
		t.Fatalf("ZSet 的原文定位错误: %q", got)
	}

	// 时间线的偏移必须做字节到 rune 的换算, 否则定位会错到别的字上。
	var timeline *Entity
	for _, e := range entitiesOf(r, KindTimeline) {
		if e.Value == "2023.06 - 2024.03" {
			timeline = &e
			break
		}
	}
	if timeline == nil {
		t.Fatal("应抽到时间区间 2023.06 - 2024.03")
	}
	if got := r.Quote(*timeline); got != "2023.06 - 2024.03" {
		t.Fatalf("时间线原文定位错误: %q", got)
	}

	if len(entitiesOf(r, KindProject)) < 2 {
		t.Fatalf("应抽到至少 2 个项目段落, 实际 %d", len(entitiesOf(r, KindProject)))
	}
}

func TestCrossValidationFlagsSkillsAndReversedDates(t *testing.T) {
	r := NewRuleExtractor().Extract(sampleResume)

	var warnsAboutPython, warnsReversed bool
	for _, w := range r.Warnings {
		if strings.Contains(w, "Python") {
			warnsAboutPython = true
		}
		if strings.Contains(w, "倒挂") {
			warnsReversed = true
		}
	}
	if !warnsAboutPython {
		t.Fatalf("技能 Python 没有出现在任何项目里, 应产生警告: %v", r.Warnings)
	}
	if !warnsReversed {
		t.Fatalf("时间区间倒挂应产生警告: %v", r.Warnings)
	}
}

// mockChat 是一个按顺序返回预设输出的 OpenAI 兼容假服务。
func mockChat(t *testing.T, responses ...string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		idx := int(n) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": responses[idx]}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(payload))
	}))
	return srv, &calls
}

func llmClientFor(t *testing.T, srv *httptest.Server) *llm.Client {
	t.Helper()
	return llm.NewClient(llm.Config{
		BaseURL: srv.URL, APIKey: "k", Model: "mock", MaxRetries: 0,
	})
}

// 模型抽取的实体必须能回原文定位; 编造的会被丢弃而不是照单全收。
func TestLLMExtractorLocatesAndRejectsFabricated(t *testing.T) {
	resp := `{"skills":["Redis","Kubernetes"],"projects":[{"name":"IM 对话平台","description":"负责存储改造","time_range":"2023.06 - 2024.03","tech":["ZSet"]}]}`
	srv, _ := mockChat(t, resp)
	defer srv.Close()

	r, err := NewLLMExtractor(llmClientFor(t, srv)).Extract(context.Background(), sampleResume)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}

	var hasRedis, hasK8s, hasIM, hasZSet bool
	for _, e := range r.Entities {
		switch e.Value {
		case "Redis":
			hasRedis = true
		case "Kubernetes":
			hasK8s = true
		case "IM 对话平台":
			hasIM = true
		case "ZSet":
			hasZSet = true
		}
	}
	if !hasRedis || !hasIM || !hasZSet {
		t.Fatalf("原文里真实存在的实体都应被抽取并定位: redis=%v im=%v zset=%v",
			hasRedis, hasIM, hasZSet)
	}
	if hasK8s {
		t.Fatal("原文没有 Kubernetes, 模型编造的内容不应被接受")
	}

	var warned bool
	for _, w := range r.Warnings {
		if strings.Contains(w, "Kubernetes") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("编造内容应产生告警: %v", r.Warnings)
	}
}

func TestLLMExtractorRetriesOnSchemaViolation(t *testing.T) {
	srv, calls := mockChat(t,
		`{"skills":"not-an-array","projects":[]}`,
		`{"skills":["Go"],"projects":[]}`,
	)
	defer srv.Close()

	r, err := NewLLMExtractor(llmClientFor(t, srv)).Extract(context.Background(), sampleResume)
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Fatalf("schema 不合规应重试一次, 实际调用 %d 次", atomic.LoadInt32(calls))
	}
	if len(entitiesOf(r, KindSkill)) == 0 {
		t.Fatal("第二次合规输出应被采用")
	}
}

func TestRuleExtractorIsIdempotentAndDeterministic(t *testing.T) {
	a := NewRuleExtractor().Extract(sampleResume)
	b := NewRuleExtractor().Extract(sampleResume)
	if len(a.Entities) != len(b.Entities) {
		t.Fatalf("规则抽取必须确定性: %d vs %d", len(a.Entities), len(b.Entities))
	}
}
