package llm

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"time"
)

// 熔断与费用预算。
//
// 这两件事和"重试"是正交的: 重试解决"偶尔抖动一次",
// 熔断解决"上游持续不可用", 配额解决"总量失控、被刷爆"。
// 三者在调用链路上分工明确, 谁也别替代谁。

var (
	// ErrCircuitOpen 表示熔断器打开, 本次调用被快速拒绝 —— 没有发出任何请求。
	ErrCircuitOpen = errors.New("llm: 熔断器打开, 已快速失败(未发起请求)")
	// ErrBudgetExceeded 表示已超过本进程的 token 费用预算。
	ErrBudgetExceeded = errors.New("llm: 已超过 token 费用预算")
)

type circuitState uint8

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// circuitBreaker 是简单的三态熔断器。
//
// 触发: 连续 threshold 次可重试失败(5xx/429/网络错误)后打开;
// 打开后冷却 cooldown, 再放行一个"探针请求"进入半开态 ——
// 探针成功则闭合, 失败则重新打开。这样既不会在上游故障时反复空耗,
// 也能在上游恢复后自动重新放量。
type circuitBreaker struct {
	mu                  sync.Mutex
	state               circuitState
	consecutiveFailures int
	openedAt            time.Time
	threshold           int
	cooldown            time.Duration
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &circuitBreaker{state: circuitClosed, threshold: threshold, cooldown: cooldown}
}

// allow 决定本次调用是否放行。打开且在冷却期内的调用被直接拒绝。
func (cb *circuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case circuitClosed:
		return true
	case circuitHalfOpen:
		// 半开态一次只放行一个探针, 其余调用仍被拒绝。
		return false
	case circuitOpen:
		if time.Since(cb.openedAt) >= cb.cooldown {
			cb.state = circuitHalfOpen
			return true
		}
		return false
	}
	return false
}

// recordSuccess 在调用成功后闭合熔断器并清零失败计数。
func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = circuitClosed
	cb.consecutiveFailures = 0
}

// recordFailure 在调用最终失败后累计失败次数; 达到阈值即打开熔断器。
func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFailures++
	if cb.state == circuitHalfOpen {
		// 探针失败, 立即重新打开, 并重置计数基准。
		cb.state = circuitOpen
		cb.openedAt = time.Now()
		cb.consecutiveFailures = 1
		return
	}
	if cb.consecutiveFailures >= cb.threshold {
		cb.state = circuitOpen
		cb.openedAt = time.Now()
	}
}

// isOpen 报告熔断器当前是否处于打开状态。
func (cb *circuitBreaker) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state == circuitOpen
}

// tokenBudget 是"滑动窗口"式的 token 预算(简化实现: 按窗口起点重算)。
// limit<=0 表示不限。
type tokenBudget struct {
	mu          sync.Mutex
	windowStart time.Time
	used        int
	limit       int
	window      time.Duration
}

func newTokenBudget(limit int, window time.Duration) *tokenBudget {
	if window <= 0 {
		window = time.Minute
	}
	return &tokenBudget{windowStart: time.Now(), limit: limit, window: window}
}

// exceeded 判断当前窗口是否已用尽。limit<=0 时永不为真。
func (b *tokenBudget) exceeded() bool {
	if b.limit <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.used >= b.limit
}

// consume 记入 n 个 token。
func (b *tokenBudget) consume(n int) {
	if b.limit <= 0 || n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	b.used += n
}

// roll 在窗口过期时重置(调用方必须持锁)。
func (b *tokenBudget) roll() {
	if time.Since(b.windowStart) >= b.window {
		b.windowStart = time.Now()
		b.used = 0
	}
}

// envInt / envDuration 从环境变量解析配置; 解析失败返回 0,
// 由调用方决定是否回落默认值 —— 避免一行写错的配置把整个服务带崩。
func envInt(name string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func envDuration(name string) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 0
}
