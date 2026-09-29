package practice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 这条用例是"真的在 Redis 上跑一遍"。
//
// 上面那些用例用的是符合 Redis 语义的内存替身, 它们验证的是**算法**
// (谁和谁配、什么时候算过期); 这一条验证的是**接线**: go-redis 的命令
// 用法、ZPopMin 的真实返回、TTL 的真实行为、以及两个进程共用一个 Redis
// 时到底会不会配对。
//
// 跑法(项目里已经有 docker compose 的 redis 服务):
//
//	make docker-up
//	REDIS_ADDR=127.0.0.1:6379 go test ./internal/practice/ -run RealRedis -v
//
// 未设置 REDIS_ADDR 时跳过而不是失败: 单元测试必须能在没有外部依赖的
// 机器上跑通, 否则 CI 会因为环境差异一片红, 最后没人再认真看结果。
func TestRedisManagerAgainstRealRedis(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("未设置 REDIS_ADDR, 跳过真实 Redis 集成测试")
	}
	client := store.NewRedisClient(addr, os.Getenv("REDIS_PASSWORD"), 0)
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("连接 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// 用独立前缀, 避免影响同一个库里的其它数据。
	prefix := fmt.Sprintf("practice:selftest:%d:", time.Now().UnixNano())
	t.Cleanup(func() { cleanupPrefix(t, client, prefix) })

	cfg := Config{WaitTTL: 2 * time.Second, PairedTTL: time.Minute}
	a := NewRedisManager(client, prefix, cfg)
	b := NewRedisManager(client, prefix, cfg)

	t.Run("跨实例配对", func(t *testing.T) {
		roomA, _, err := a.Join("tenant-a", "u_1", "陈雨", "后端一面", RoleInterviewee)
		if err != nil {
			t.Fatalf("实例 A 排队失败: %v", err)
		}
		roomB, role, err := b.Join("tenant-a", "u_2", "王琳", "后端一面", RoleInterviewer)
		if err != nil {
			t.Fatalf("实例 B 配对失败: %v", err)
		}
		if roomB.ID != roomA.ID || roomB.State != StatePaired || role != RoleInterviewer {
			t.Fatalf("跨实例未配上: %s/%s vs %s", roomB.ID, roomB.State, roomA.ID)
		}
		if len(roomB.Participants) != 2 {
			t.Fatalf("房间里应有两名参与者: %+v", roomB.Participants)
		}
	})

	t.Run("并发配对不产生半间房", func(t *testing.T) {
		const pairs = 8
		var wg sync.WaitGroup
		errCh := make(chan error, pairs*2)
		for i := 0; i < pairs*2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				want := RoleInterviewee
				if i%2 == 1 {
					want = RoleInterviewer
				}
				mgr := a
				if i%2 == 0 {
					mgr = b
				}
				if _, _, err := mgr.Join("tenant-a", fmt.Sprintf("c_%02d", i), "并发", "并发对练", want); err != nil {
					errCh <- err
				}
			}(i)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("并发加入失败: %v", err)
		}
		stats := a.Stats("tenant-a")
		if stats["waiting"] != 0 {
			t.Fatalf("并发配对后不应有人落单: %+v", stats)
		}
		for _, room := range a.List("tenant-a") {
			if len(room.Participants) > 2 {
				t.Fatalf("房间 %s 挤进了 %d 个人", room.ID, len(room.Participants))
			}
		}
	})

	t.Run("真实 TTL 会让等待房自己消失", func(t *testing.T) {
		room, _, err := b.Join("tenant-b", "u_9", "等待者", "", RoleInterviewer)
		if err != nil {
			t.Fatalf("加入失败: %v", err)
		}
		// WaitTTL 配成 2 秒: 到点之后房间应随 TTL 消失 —— 这条只有真实
		// Redis 能验证(替身里的 TTL 是我自己实现的, 不能证明 Redis 的行为)。
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := a.Get("tenant-b", room.ID); errors.Is(err, ErrRoomNotFound) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("等待房到期后仍然存在: TTL 没有生效")
	})
}

// cleanupPrefix 删除测试写入的 key。
func cleanupPrefix(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}
