package api

import (
	"sync"
	"time"
)

// limiter 是单进程内的令牌桶限流器。
//
// 为什么服务自身还需要限流: 前面有网关不代表这个进程可以无限接受请求。
// 创建会话会写库、WebSocket 握手会占住长连接, 某个客户端一旦异常重试,
// 整个实例的可用性都会被拖下去 —— 而它拖垮的是**其他候选人正在进行的面试**。
// 单机限流是最后一道兜底。
//
// 生产环境应该换成 Redis 做集群级限流(多实例共享配额), 但接口保持一致,
// 替换时上层无感。
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // 每秒补充的令牌数
	burst   float64 // 桶容量(允许的瞬时突发)
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newLimiter(rate, burst float64) *limiter {
	if rate <= 0 {
		rate = 20
	}
	if burst <= 0 {
		burst = 60
	}
	return &limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// allow 判断某个 key(通常是客户端 IP)此刻是否允许通过。
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
		// 顺手清理空闲桶。不做这件事的话, 这个 map 会被扫描源或
		// 伪造源 IP 的请求撑爆 —— 限流器本身变成内存泄漏点,
		// 是这类组件最经典的自伤方式。
		if len(l.buckets) > 4096 {
			l.sweep(now)
		}
	}

	b.tokens = minFloat(l.burst, b.tokens+now.Sub(b.lastSeen).Seconds()*l.rate)
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep 清理超过 10 分钟没有请求的桶。调用方需持有锁。
func (l *limiter) sweep(now time.Time) {
	const idleTTL = 10 * time.Minute
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > idleTTL {
			delete(l.buckets, key)
		}
	}
}

// size 返回当前追踪的 key 数量, 仅用于测试与观测。
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
