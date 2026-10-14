package llm

import (
	"testing"
	"time"
)

func TestCircuitOpensAfterConsecutiveFailures(t *testing.T) {
	cb := newCircuitBreaker(3, time.Hour)
	for i := 0; i < 2; i++ {
		if !cb.allow() {
			t.Fatalf("第 %d 次失败前应放行", i+1)
		}
		cb.recordFailure()
	}
	if !cb.allow() {
		t.Fatal("第三次前仍应放行")
	}
	cb.recordFailure()
	if !cb.isOpen() {
		t.Fatal("连续 3 次失败后应打开熔断器")
	}
	if cb.allow() {
		t.Fatal("打开后冷却期内应拒绝")
	}
}

func TestCircuitHalfOpenProbeSuccessCloses(t *testing.T) {
	cb := newCircuitBreaker(1, time.Hour)
	if !cb.allow() {
		t.Fatal("初始应放行")
	}
	cb.recordFailure()
	if !cb.isOpen() {
		t.Fatal("一次失败即应打开")
	}
	cb.openedAt = time.Now().Add(-2 * time.Hour) // 让冷却过期
	if !cb.allow() {
		t.Fatal("冷却到期应放行探针(进入半开)")
	}
	if cb.allow() {
		t.Fatal("半开态只应放行一个探针")
	}
	cb.recordSuccess()
	if cb.isOpen() {
		t.Fatal("探针成功后应闭合")
	}
	if !cb.allow() {
		t.Fatal("闭合后应继续放行")
	}
}

func TestCircuitHalfOpenProbeFailureReopens(t *testing.T) {
	cb := newCircuitBreaker(1, time.Hour)
	cb.allow()
	cb.recordFailure()
	cb.openedAt = time.Now().Add(-2 * time.Hour)
	if !cb.allow() {
		t.Fatal("冷却到期应放行探针")
	}
	cb.recordFailure()
	if !cb.isOpen() {
		t.Fatal("探针失败后应重新打开")
	}
}

func TestTokenBudgetWindow(t *testing.T) {
	b := newTokenBudget(10, time.Minute)
	if b.exceeded() {
		t.Fatal("初始不应超限")
	}
	b.consume(6)
	if b.exceeded() {
		t.Fatal("消耗 6 不应超限")
	}
	b.consume(4)
	if !b.exceeded() {
		t.Fatal("消耗到 10 应达到上限")
	}
}

func TestTokenBudgetUnlimited(t *testing.T) {
	b := newTokenBudget(0, time.Minute)
	b.consume(1_000_000)
	if b.exceeded() {
		t.Fatal("limit<=0 应视为不限")
	}
}
