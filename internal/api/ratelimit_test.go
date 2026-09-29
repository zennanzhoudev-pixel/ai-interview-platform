package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

func TestLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := newLimiter(1, 3)
	l.now = func() time.Time { return time.Unix(0, 0) } // 冻结时间, 排除补充令牌的干扰

	for i := 1; i <= 3; i++ {
		if !l.allow("client-a") {
			t.Fatalf("突发额度内第 %d 次应通过", i)
		}
	}
	if l.allow("client-a") {
		t.Fatal("超出突发额度后应被拒绝")
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(10, 1) // 每秒 10 个令牌
	l.now = func() time.Time { return now }

	if !l.allow("client-a") {
		t.Fatal("首次请求应通过")
	}
	if l.allow("client-a") {
		t.Fatal("额度耗尽后应立即被拒绝")
	}

	now = now.Add(200 * time.Millisecond) // 补回 2 个令牌
	if !l.allow("client-a") {
		t.Fatal("补充令牌后应重新放行")
	}
}

func TestLimiterIsolatesClients(t *testing.T) {
	l := newLimiter(1, 1)
	l.now = func() time.Time { return time.Unix(0, 0) }

	if !l.allow("client-a") {
		t.Fatal("A 的首次请求应通过")
	}
	if l.allow("client-a") {
		t.Fatal("A 的第二次请求应被拒绝")
	}
	if !l.allow("client-b") {
		t.Fatal("限流必须按来源隔离, B 不应被 A 拖累")
	}
}

// 限流器自身不能变成内存泄漏点: 被扫描或伪造的源 IP 会不断制造新 key。
func TestLimiterSweepsIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(100, 1)
	l.now = func() time.Time { return now }

	for i := 0; i < 5000; i++ {
		l.allow(fmt.Sprintf("client-%d", i))
	}
	if l.size() > 5000 {
		t.Fatalf("清理没有生效, 桶数量 %d", l.size())
	}

	// 让所有桶进入空闲态, 再触发一次清理。
	// 注意只新增一个 key: 清理后剩下的应该是"新来的那个", 而不是继续累积。
	now = now.Add(11 * time.Minute)
	l.allow("fresh-0")
	if got := l.size(); got > 100 {
		t.Fatalf("空闲桶应被清理, 当前桶数量 %d", got)
	}
}

func TestRateLimitMiddlewareReturns429(t *testing.T) {
	srv := NewServer(Config{
		Store:              store.NewMemoryStore(),
		RateLimitPerSecond: 1,
		RateLimitBurst:     1,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	first, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("首次请求应通过, 实际 %d", first.StatusCode)
	}

	second, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("第二次请求失败: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("超出额度应返回 429, 实际 %d", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After, 便于客户端退避")
	}
}
