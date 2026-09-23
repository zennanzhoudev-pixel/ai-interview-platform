package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis 起一个进程内的 Redis(miniredis), 让 Redis 路径
// 也能在没有外部依赖的情况下被真实执行 —— 而不是只写个 mock 假装通过了。
func newTestRedis(t *testing.T) (*RedisCheckpointStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisCheckpointStore(client, "test:ckpt:"), mr
}

func TestRedisCheckpointRoundTrip(t *testing.T) {
	s, _ := newTestRedis(t)
	ctx := context.Background()

	payload := []byte(`{"stage":"RESUME_DEEP_DIVE","asked":3,"remaining_ms":900000}`)
	if err := s.Save(ctx, "s1", payload, time.Hour); err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}

	got, err := s.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("快照内容不一致: %s", got)
	}

	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatalf("删除快照失败: %v", err)
	}
	if _, err := s.Load(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应返回 ErrNotFound, 实际 %v", err)
	}
}

func TestRedisCheckpointMissingIsNotFound(t *testing.T) {
	s, _ := newTestRedis(t)
	if _, err := s.Load(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的快照应返回 ErrNotFound, 实际 %v", err)
	}
}

// 快照必须会过期。候选人弃面的会话如果永久留在 Redis 里,
// 峰值期会把实例悄悄撑爆 —— 而且是没有任何告警的那种撑爆。
func TestRedisCheckpointExpires(t *testing.T) {
	s, mr := newTestRedis(t)
	ctx := context.Background()

	if err := s.Save(ctx, "s2", []byte("{}"), 30*time.Minute); err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}
	mr.FastForward(29 * time.Minute)
	if _, err := s.Load(ctx, "s2"); err != nil {
		t.Fatalf("29 分钟时快照应仍然存在: %v", err)
	}

	mr.FastForward(2 * time.Minute)
	if _, err := s.Load(ctx, "s2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TTL 到期后应自动清理, 实际 %v", err)
	}
}

func TestRedisCheckpointDefaultTTL(t *testing.T) {
	s, mr := newTestRedis(t)
	ctx := context.Background()

	// 传 0 时应落到 24 小时默认值, 而不是永久不过期。
	if err := s.Save(ctx, "s3", []byte("{}"), 0); err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}
	mr.FastForward(23 * time.Hour)
	if _, err := s.Load(ctx, "s3"); err != nil {
		t.Fatalf("23 小时内快照应仍然存在: %v", err)
	}
	mr.FastForward(2 * time.Hour)
	if _, err := s.Load(ctx, "s3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("默认 TTL 应为 24 小时, 实际 %v", err)
	}
}

func TestRedisCheckpointRejectsEmptySessionID(t *testing.T) {
	s, _ := newTestRedis(t)
	if err := s.Save(context.Background(), "", []byte("{}"), time.Hour); err == nil {
		t.Fatal("空会话 ID 应被拒绝")
	}
}

// 多租户隔离: 不同租户的快照必须落在不同的 key 空间里。
func TestRedisCheckpointPrefixIsolatesTenants(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	a := NewRedisCheckpointStore(client, "t1:")
	b := NewRedisCheckpointStore(client, "t2:")
	if err := a.Save(ctx, "same-id", []byte("from-t1"), time.Hour); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := b.Load(ctx, "same-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不同租户的快照不能互相可见, 实际 %v", err)
	}
}
