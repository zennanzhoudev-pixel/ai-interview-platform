package store

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCheckpointStore 用 Redis 保存面试会话快照。
//
// 为什么快照放 Redis 而不是 MySQL: 面试进行中每 30 秒写一次快照,
// 一场面试就是 60 多次写入。这些数据在面试结束后只需要归档一次,
// 让它们全部打到主库是纯粹的浪费 —— 而且是在秋招峰值最不该浪费的时候。
// 用 Redis 承载热路径、结束后异步归档到 MySQL, 是这个场景的标准分工。
type RedisCheckpointStore struct {
	client *redis.Client
	prefix string
}

// NewRedisCheckpointStore 构造快照存储。
// prefix 用于多租户隔离, 空值时使用默认前缀。
func NewRedisCheckpointStore(client *redis.Client, prefix string) *RedisCheckpointStore {
	if prefix == "" {
		prefix = "interview:checkpoint:"
	}
	return &RedisCheckpointStore{client: client, prefix: prefix}
}

// NewRedisClient 构造 Redis 客户端。
func NewRedisClient(addr, password string, db int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}

func (s *RedisCheckpointStore) key(sessionID string) string { return s.prefix + sessionID }

// Save 写入快照。
//
// TTL 必须存在且不能太长: 候选人弃面的会话如果永久留在 Redis 里,
// 峰值期会把实例悄悄撑爆。默认 24 小时 —— 足够覆盖"面试中途断开,
// 第二天找回链接"这种真实情况, 又不至于无限堆积。
func (s *RedisCheckpointStore) Save(ctx context.Context, sessionID string, payload []byte, ttl time.Duration) error {
	if sessionID == "" {
		return errors.New("store: 会话 ID 不能为空")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return s.client.Set(ctx, s.key(sessionID), payload, ttl).Err()
}

// Load 读取快照。不存在返回 ErrNotFound, 便于上层区分"没有快照"和"读失败"。
func (s *RedisCheckpointStore) Load(ctx context.Context, sessionID string) ([]byte, error) {
	data, err := s.client.Get(ctx, s.key(sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Delete 删除快照(面试正常结束后调用)。
func (s *RedisCheckpointStore) Delete(ctx context.Context, sessionID string) error {
	return s.client.Del(ctx, s.key(sessionID)).Err()
}
