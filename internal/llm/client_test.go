package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientChatSendsExpectedRequest(t *testing.T) {
	var (
		gotAuth string
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"}}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-test"})
	resp, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("路径错误: %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头错误: %q", gotAuth)
	}
	if gotBody["model"] != "gpt-test" {
		t.Errorf("model 字段错误: %v", gotBody["model"])
	}
	if gotBody["response_format"] == nil {
		t.Error("默认应开启 JSON 模式, 避免模型输出解释性文字")
	}
	if resp.TotalTokens != 15 || resp.PromptTokens != 10 || resp.CompletionTokens != 5 {
		t.Errorf("token 统计错误: %+v", resp)
	}
	if resp.Attempts != 1 {
		t.Errorf("首次调用成功时 Attempts 应为 1, 实际 %d", resp.Attempts)
	}
}

func TestClientRetriesRateLimitThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 2})
	resp, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("限流后重试应当成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应调用 2 次, 实际 %d", calls)
	}
	if resp.Attempts != 2 || resp.Text != "ok" {
		t.Fatalf("响应异常: %+v", resp)
	}
}

func TestClientRetriesServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "upstream busy", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 3})
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("5xx 应当被重试直到成功: %v", err)
	}
	if calls != 3 {
		t.Fatalf("应调用 3 次, 实际 %d", calls)
	}
}

// 400 这类错误说明请求本身有问题, 重试只会浪费时间和额度。
func TestClientDoesNotRetryBadRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":"invalid model"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 3})
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("400 应返回错误")
	}
	if calls != 1 {
		t.Fatalf("400 不应重试, 实际调用 %d 次", calls)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("应返回带状态码的 APIError, 实际 %v", err)
	}
	if apiErr.Retryable() {
		t.Fatal("400 不应被标记为可重试")
	}
}

func TestClientDoesNotStartWhenContextCanceled(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 3})
	if _, err := c.Chat(ctx, []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("已取消的 context 应直接返回错误")
	}
	if calls != 0 {
		t.Fatalf("context 已取消时不应发起请求, 实际 %d 次", calls)
	}
}

// 单次请求超时必须生效, 否则一个卡住的上游会把整条评分链路拖死。
func TestClientAppliesPerAttemptTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Timeout: 50 * time.Millisecond, MaxRetries: 0,
	})
	start := time.Now()
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("上游超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时未生效, 耗时 %v", elapsed)
	}
}

func TestClientRequiresAPIKey(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://127.0.0.1:1", Model: "m"})
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("缺少 API Key 应直接报错")
	}
}

func TestFromEnvReadsOpenAICompatibleConfig(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://example.com/v1/")
	t.Setenv("LLM_API_KEY", "sk-env")
	t.Setenv("LLM_MODEL", "deepseek-chat")

	cfg := FromEnv()
	if cfg.BaseURL != "https://example.com/v1/" || cfg.APIKey != "sk-env" || cfg.Model != "deepseek-chat" {
		t.Fatalf("环境变量读取错误: %+v", cfg)
	}
	if !cfg.Enabled() {
		t.Fatal("配置完整时应为可用状态")
	}
}

func TestFromEnvFallsBackToOpenAIKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	if got := FromEnv().APIKey; got != "sk-openai" {
		t.Fatalf("应兼容 OPENAI_API_KEY, 实际 %q", got)
	}
}

// 上游返回的错误正文可能很长, 必须截断后再往上抛,
// 否则一条错误日志就能把 4xx 的整个响应体写进日志系统。
func TestAPIErrorBodyIsTruncated(t *testing.T) {
	long := strings.Repeat("x", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, long, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 0})
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 APIError, 实际 %v", err)
	}
	if len(apiErr.Body) > 400 {
		t.Fatalf("错误正文应被截断, 实际长度 %d", len(apiErr.Body))
	}
}
