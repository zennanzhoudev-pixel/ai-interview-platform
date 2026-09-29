package practice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Pairer 是配对能力的统一接口: 单实例用内存实现, 多副本用 Redis 实现。
//
// 上层只看这个接口, 因此"要不要多副本"是部署决定, 而不是一次代码改造。
type Pairer interface {
	Join(tenantID, userID, name, topic string, want Role) (Room, Role, error)
	Leave(tenantID, roomID, userID string) (Room, bool, error)
	Get(tenantID, roomID string) (Room, error)
	List(tenantID string) []Room
	Stats(tenantID string) map[string]int
	// Backend 返回实现名, 供自检如实说明"配对状态存在哪"。
	Backend() string
}

// Backend 返回内存实现的名字。
func (m *Manager) Backend() string { return "memory" }

// redisOps 是 RedisManager 需要的**全部** Redis 能力。
//
// 收敛成一个窄接口有两个直接好处:
//  1. 生产用 go-redis 实现, 验证时可以用一个符合 Redis 语义的内存实现 ——
//     配对算法(谁和谁配、什么时候算过期)因此能被真正跑到, 而不是只留在纸上;
//  2. "这个模块依赖 Redis 的哪些语义"变成一句可读的话, 而不是散落在代码里。
//
// 刻意只用**单条原子命令**完成配对, 不使用 Lua 脚本, 也不使用分布式锁:
// 认领等待者靠 ZPopMin(原子弹出), 两个人不可能拿到同一个等待者。少一个
// 锁就少一类"锁超时后被别人删掉"的故障, 也少一层难以测试的间接。
type redisOps interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	// DelIfValue 只在 key 的当前值等于 value 时删除它(解锁的标准写法)。
	// 直接 Del 会把"已经超时、并被别人重新获取"的锁误删。
	DelIfValue(ctx context.Context, key, value string) (bool, error)
	HSet(ctx context.Context, key string, values map[string]any) error
	HGet(ctx context.Context, key, field string) (string, bool, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
	ZAdd(ctx context.Context, key string, score float64, member string) error
	ZPopMin(ctx context.Context, key string) (string, float64, bool, error)
	ZRemRangeByScore(ctx context.Context, key string, min, max float64) error
	ZRem(ctx context.Context, key string, members ...string) error
	ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error)
}

// RedisManager 把配对状态放进 Redis, 让多副本部署也能正确配对。
//
// 为什么内存实现不能直接用于多副本: 两个人在不同实例上排队时, 各自的内存
// 里都以为自己"唯一在等", 于是永远不会被配上 —— 而故障表现是"一直卡在
// 等待页", 既不报错也没有日志。
type RedisManager struct {
	ops       redisOps
	prefix    string
	waitTTL   time.Duration
	pairedTTL time.Duration
	now       func() time.Time
}

// Config 参数见 Manager; 这里复用同一份配置结构。
func NewRedisManager(client *redis.Client, prefix string, cfg Config) *RedisManager {
	return NewRedisManagerWithOps(newGoRedisOps(client), prefix, cfg)
}

// NewRedisManagerWithOps 允许注入自定义的命令实现(测试用)。
func NewRedisManagerWithOps(ops redisOps, prefix string, cfg Config) *RedisManager {
	if prefix == "" {
		prefix = "practice:"
	}
	if cfg.WaitTTL <= 0 {
		cfg.WaitTTL = 15 * time.Minute
	}
	if cfg.PairedTTL <= 0 {
		cfg.PairedTTL = 3 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RedisManager{
		ops: ops, prefix: prefix,
		waitTTL: cfg.WaitTTL, pairedTTL: cfg.PairedTTL, now: cfg.Now,
	}
}

// Backend 返回实现名。
func (m *RedisManager) Backend() string { return "redis" }

func (m *RedisManager) roomKey(id string) string { return m.prefix + "room:" + id }

func (m *RedisManager) waitKey(tenant string, role Role) string {
	return m.prefix + "wait:" + tenant + ":" + string(role)
}

func (m *RedisManager) userKey(tenant, userID string) string {
	return m.prefix + "user:" + tenant + ":" + userID
}

func (m *RedisManager) tenantKey(tenant string) string { return m.prefix + "tenant:" + tenant }

// claimKey 是"正在被认领"的等待房间; 见 Join 里的说明。
func (m *RedisManager) claimKey(tenant string) string { return m.prefix + "claim:" + tenant }

// lockKey 是租户级配对锁。
func (m *RedisManager) lockKey(tenant string) string { return m.prefix + "lock:" + tenant }

// lockTenant 获取配对锁, 返回解锁函数。
//
// 用 SET NX + TTL 实现租约锁: 持锁者崩溃时锁会自己过期, 不会把整个租户的
// 配对永久卡死。自旋上限 2 秒 —— 超过就告诉调用方"稍后重试", 而不是无限等。
func (m *RedisManager) lockTenant(ctx context.Context, tenantID string) (func(), error) {
	key := m.lockKey(tenantID)
	token := fmt.Sprintf("%d-%d", m.now().UnixNano(), randInt63())
	deadline := m.now().Add(2 * time.Second)
	for {
		ok, err := m.ops.SetNX(ctx, key, token, 5*time.Second)
		if err != nil {
			return func() {}, err
		}
		if ok {
			return func() {
				unlockCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, _ = m.ops.DelIfValue(unlockCtx, key, token)
			}, nil
		}
		if m.now().After(deadline) {
			return func() {}, fmt.Errorf("practice: 配对繁忙, 请稍后重试")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func randInt63() int64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return time.Now().UnixNano()
	}
	var v int64
	for _, b := range buf {
		v = v<<8 | int64(b)
	}
	if v < 0 {
		v = -v
	}
	return v
}

// wantsOf 返回"我该去哪个等待队列里找对方"。
//
// 注意取的是**补集**: 我想当面试官, 就要找正在等面试官的候选人。
func wantsOf(want Role) []Role {
	switch want {
	case RoleInterviewer:
		return []Role{RoleInterviewee}
	case RoleInterviewee:
		return []Role{RoleInterviewer}
	default:
		// any: 先补"想练提问"的人, 再补"想被面试"的人。
		return []Role{RoleInterviewer, RoleInterviewee}
	}
}

// Join 加入配对。规则与内存实现完全一致(见 Manager.Join)。
func (m *RedisManager) Join(tenantID, userID, name, topic string, want Role) (Room, Role, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(tenantID) == "" {
		return Room{}, "", fmt.Errorf("%w: 缺少账号或租户", ErrNotMember)
	}
	switch want {
	case RoleInterviewer, RoleInterviewee, RoleAny:
	default:
		return Room{}, "", ErrInvalidRole
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1) 已经在房间里就直接返回(刷新页面会重复 join)。
	if room, role, ok := m.existing(ctx, tenantID, userID); ok {
		return room, role, nil
	}

	// "认领或入队"必须是一个原子操作, 这里用一把租户级短锁保护它。
	//
	// 没有锁时存在一个真实竞态: 两个互相兼容的人同时到达, 各自的等待队列
	// 在同一瞬间都是空的 —— 于是双方都开了等待房, 谁也没配上。表现就是
	// "两个人都卡在等待页"。这个 bug 是并发测试跑出来的(12 对里只配成 11 对)。
	//
	// 锁的粒度是租户: 不同公司的配对不对互相排队; 持有时间是毫秒级,
	// TTL 只是"持锁者崩了"时的兜底。
	unlock, err := m.lockTenant(ctx, tenantID)
	if err != nil {
		return Room{}, "", err
	}
	defer unlock()

	// 拿到锁之后再确认一次: 临界区之外的等待里, 别人可能已经把我配掉了。
	if room, role, ok := m.existing(ctx, tenantID, userID); ok {
		return room, role, nil
	}

	// 2) 认领一个等待者: 按"我该补哪个角色"的顺序试。
	for _, occupantRole := range wantsOf(want) {
		if room, role, ok := m.claimAndPair(ctx, tenantID, occupantRole, userID, name); ok {
			return room, role, nil
		}
	}

	// 3) 没人可配: 自己开一间等待房。
	role := want
	if role == RoleAny {
		role = RoleInterviewee
	}
	now := m.now().UTC()
	room := Room{
		ID:       newRoomID(now),
		TenantID: tenantID,
		Topic:    defaultTopic(topic),
		State:    StateWaiting,
		Participants: []Participant{{
			UserID: userID, Name: displayName(name), Role: role, JoinedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := m.saveRoom(ctx, room, m.waitTTL); err != nil {
		return Room{}, "", err
	}
	if err := m.ops.ZAdd(ctx, m.waitKey(tenantID, role), float64(now.UnixMilli()), room.ID); err != nil {
		return Room{}, "", err
	}
	if err := m.ops.ZAdd(ctx, m.tenantKey(tenantID), float64(now.UnixMilli()), room.ID); err != nil {
		return Room{}, "", err
	}
	if err := m.ops.Set(ctx, m.userKey(tenantID, userID), room.ID, m.waitTTL); err != nil {
		return Room{}, "", err
	}
	return room, role, nil
}

// claimAndPair 认领一个等待中的房间并与自己配对。
//
// 关键点: **认领是原子的**(ZPopMin), 因此两个人不可能认领到同一个等待者。
//
// 为什么还要一个 claim 标记: 认领成功与"写回房间"之间存在极短的窗口,
// 如果进程正好在这里崩掉, 那个等待者的房间就既不在排队队列里、也没有被
// 配对 —— 他会一直等到房间 TTL 到期(最多 15 分钟)。用一个短期标记
// (claim:<tenant> -> roomID) 记下"这个房间正在被认领", 让他在轮询时
// 能被告知"正在为你配对", 而不是看上去毫无动静。
func (m *RedisManager) claimAndPair(ctx context.Context, tenantID string, occupant Role, userID, name string) (Room, Role, bool) {
	key := m.waitKey(tenantID, occupant)
	// 先清掉超时的等待者: 它们的房间已经过期, 配上就是一间空房。
	cutoff := float64(m.now().Add(-m.waitTTL).UnixMilli())
	if err := m.ops.ZRemRangeByScore(ctx, key, -1<<62, cutoff); err != nil {
		return Room{}, "", false
	}
	roomID, _, ok, err := m.ops.ZPopMin(ctx, key)
	if err != nil || !ok || roomID == "" {
		return Room{}, "", false
	}
	// 认领期间打个标记: 万一这台实例随后失联, 对面至少能看出"有人在配"。
	_ = m.ops.Set(ctx, m.claimKey(tenantID), roomID, 30*time.Second)
	defer func() { _ = m.ops.Del(context.Background(), m.claimKey(tenantID)) }()

	room, err := m.Get(tenantID, roomID)
	if err != nil || room.State != StateWaiting || room.Full() {
		// 房间已经失效: 把成员从队列里清掉, 让调用方继续尝试下一个。
		_ = m.ops.ZRem(ctx, m.tenantKey(tenantID), roomID)
		return Room{}, "", false
	}

	role := complementaryRole(occupant, RoleAny)
	now := m.now().UTC()
	room.Participants = append(room.Participants, Participant{
		UserID: userID, Name: displayName(name), Role: role, JoinedAt: now,
	})
	room.State = StatePaired
	room.UpdatedAt = now
	if err := m.saveRoom(ctx, room, m.pairedTTL); err != nil {
		// 写回失败: 把等待者放回队列, 否则他会永远等不到人。
		_ = m.ops.ZAdd(ctx, key, float64(now.UnixMilli()), roomID)
		return Room{}, "", false
	}
	for _, p := range room.Participants {
		if err := m.ops.Set(ctx, m.userKey(tenantID, p.UserID), room.ID, m.pairedTTL); err != nil {
			return Room{}, "", false
		}
	}
	// 配对成功后房间仍在租户索引里(List 与 Stats 要用)。
	_ = m.ops.ZAdd(ctx, m.tenantKey(tenantID), float64(now.UnixMilli()), room.ID)
	return room, role, true
}

// existing 查这个人是否已经在一间未结束的房里。
func (m *RedisManager) existing(ctx context.Context, tenantID, userID string) (Room, Role, bool) {
	roomID, ok, err := m.ops.Get(ctx, m.userKey(tenantID, userID))
	if err != nil || !ok || roomID == "" {
		return Room{}, "", false
	}
	room, err := m.Get(tenantID, roomID)
	if err != nil || room.State == StateEnded {
		return Room{}, "", false
	}
	if p, ok := room.Participant(userID); ok {
		return room, p.Role, true
	}
	return Room{}, "", false
}

// Leave 离开房间: 一方离开则整间房结束。
func (m *RedisManager) Leave(tenantID, roomID, userID string) (Room, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	room, err := m.Get(tenantID, roomID)
	if err != nil {
		return Room{}, false, err
	}
	if !room.Has(userID) {
		return room, false, ErrNotMember
	}
	if room.State == StateEnded {
		return room, true, nil
	}
	room.State = StateEnded
	room.EndedAt = m.now().UTC()
	room.UpdatedAt = room.EndedAt
	if err := m.saveRoom(ctx, room, 5*time.Minute); err != nil {
		return Room{}, false, err
	}
	keys := make([]string, 0, len(room.Participants))
	for _, p := range room.Participants {
		keys = append(keys, m.userKey(tenantID, p.UserID))
	}
	if err := m.ops.Del(ctx, keys...); err != nil {
		return room, true, err
	}
	// 等待中的房间被取消时, 也要从排队队列里摘掉。
	for _, role := range []Role{RoleInterviewer, RoleInterviewee} {
		_ = m.ops.ZRem(ctx, m.waitKey(tenantID, role), roomID)
	}
	return room, true, nil
}

// Get 读取房间。
func (m *RedisManager) Get(tenantID, roomID string) (Room, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, ok, err := m.ops.HGet(ctx, m.roomKey(roomID), "data")
	if err != nil {
		return Room{}, err
	}
	if !ok {
		return Room{}, ErrRoomNotFound
	}
	var room Room
	if err := json.Unmarshal([]byte(raw), &room); err != nil {
		return Room{}, fmt.Errorf("%w: 房间数据损坏", ErrRoomNotFound)
	}
	if room.TenantID != tenantID {
		// 跨租户一律当作不存在: 不要泄露"这个 ID 存在但不属于你"。
		return Room{}, ErrRoomNotFound
	}
	return room, nil
}

// List 列出本租户的房间。
func (m *RedisManager) List(tenantID string) []Room {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ids, err := m.ops.ZRevRange(ctx, m.tenantKey(tenantID), 0, 99)
	if err != nil {
		return nil
	}
	out := make([]Room, 0, len(ids))
	for _, id := range ids {
		if room, err := m.Get(tenantID, id); err == nil {
			out = append(out, room)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Stats 返回本租户的配对概况。
func (m *RedisManager) Stats(tenantID string) map[string]int {
	out := map[string]int{"waiting": 0, "paired": 0, "ended": 0}
	for _, room := range m.List(tenantID) {
		out[string(room.State)]++
	}
	return out
}

// saveRoom 写入房间并按状态设置 TTL。
//
// TTL 是这里的关键: 等待中的房间到点自己消失, 不需要任何清理任务 ——
// 而"靠定时任务清理"在实例重启、任务没跑起来时会留下幽灵房间。
func (m *RedisManager) saveRoom(ctx context.Context, room Room, ttl time.Duration) error {
	raw, err := json.Marshal(room)
	if err != nil {
		return err
	}
	if err := m.ops.HSet(ctx, m.roomKey(room.ID), map[string]any{
		"data": raw, "tenant": room.TenantID, "state": string(room.State),
	}); err != nil {
		return err
	}
	return m.ops.Expire(ctx, m.roomKey(room.ID), ttl)
}

// newRoomID 生成房间号。
//
// 必须带随机部分, 不能只用时间戳: 我曾用过"秒 + 纳秒低位", 结果在
// **时钟不前进**时(测试里的固定时钟、或同纳秒内的连续调用)会生成完全
// 相同的 ID, 两个房间于是互相覆盖 —— 表现是"两个人被配对到同一个房间里,
// 但房间里只有一个人"。这类 ID 碰撞在低流量时几乎不会出现, 一上并发就暴露。
func newRoomID(now time.Time) string {
	var buf [5]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 退无可退时才退回时间戳: 它不保证唯一, 但至少不比没有强。
		return fmt.Sprintf("pr_%d_%04d", now.Unix(), now.Nanosecond()%10000)
	}
	return fmt.Sprintf("pr_%d_%s", now.Unix(), hex.EncodeToString(buf[:]))
}

var errNilClient = errors.New("practice: 未提供 Redis 客户端")

/* ---------------- go-redis 适配层 ---------------- */

type goRedisOps struct {
	client *redis.Client
}

func newGoRedisOps(client *redis.Client) *goRedisOps {
	return &goRedisOps{client: client}
}

func (o *goRedisOps) down() error {
	if o == nil || o.client == nil {
		return errNilClient
	}
	return nil
}

func (o *goRedisOps) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if err := o.down(); err != nil {
		return false, err
	}
	return o.client.SetNX(ctx, key, value, ttl).Result()
}

func (o *goRedisOps) Get(ctx context.Context, key string) (string, bool, error) {
	if err := o.down(); err != nil {
		return "", false, err
	}
	v, err := o.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (o *goRedisOps) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := o.down(); err != nil {
		return err
	}
	return o.client.Set(ctx, key, value, ttl).Err()
}

func (o *goRedisOps) Del(ctx context.Context, keys ...string) error {
	if err := o.down(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	return o.client.Del(ctx, keys...).Err()
}

// unlockScript 是解锁的标准写法: 只有值仍然是我写进去的那个才删除。
// 这一小段 Lua 只做"比较再删除", 不参与配对逻辑 —— 配对算法本身
// 完全由可独立测试的 Go 代码完成。
var unlockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0`)

func (o *goRedisOps) DelIfValue(ctx context.Context, key, value string) (bool, error) {
	if err := o.down(); err != nil {
		return false, err
	}
	n, err := unlockScript.Run(ctx, o.client, []string{key}, value).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (o *goRedisOps) HSet(ctx context.Context, key string, values map[string]any) error {
	if err := o.down(); err != nil {
		return err
	}
	return o.client.HSet(ctx, key, values).Err()
}

func (o *goRedisOps) HGet(ctx context.Context, key, field string) (string, bool, error) {
	if err := o.down(); err != nil {
		return "", false, err
	}
	v, err := o.client.HGet(ctx, key, field).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (o *goRedisOps) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if err := o.down(); err != nil {
		return err
	}
	return o.client.Expire(ctx, key, ttl).Err()
}

func (o *goRedisOps) ZAdd(ctx context.Context, key string, score float64, member string) error {
	if err := o.down(); err != nil {
		return err
	}
	return o.client.ZAdd(ctx, key, redis.Z{Score: score, Member: member}).Err()
}

func (o *goRedisOps) ZPopMin(ctx context.Context, key string) (string, float64, bool, error) {
	if err := o.down(); err != nil {
		return "", 0, false, err
	}
	res, err := o.client.ZPopMin(ctx, key, 1).Result()
	if err != nil {
		return "", 0, false, err
	}
	if len(res) == 0 {
		return "", 0, false, nil
	}
	return fmt.Sprint(res[0].Member), res[0].Score, true, nil
}

func (o *goRedisOps) ZRemRangeByScore(ctx context.Context, key string, min, max float64) error {
	if err := o.down(); err != nil {
		return err
	}
	return o.client.ZRemRangeByScore(ctx, key, fmt.Sprintf("%f", min), fmt.Sprintf("%f", max)).Err()
}

func (o *goRedisOps) ZRem(ctx context.Context, key string, members ...string) error {
	if err := o.down(); err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	args := make([]any, 0, len(members))
	for _, m := range members {
		args = append(args, m)
	}
	return o.client.ZRem(ctx, key, args...).Err()
}

func (o *goRedisOps) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	if err := o.down(); err != nil {
		return nil, err
	}
	return o.client.ZRevRange(ctx, key, start, stop).Result()
}
