package practice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// memOps 是一个符合 Redis 语义的内存实现, 只覆盖 RedisManager 用到的那几条命令。
//
// 为什么不是"随手写的假对象": 它必须复刻这几个会直接影响正确性的行为 ——
//   - ZPopMin 取分数最小者, 分数相同时按成员字典序(Redis 的真实规则);
//   - ZRemRangeByScore 是闭区间;
//   - 每个 key 有独立 TTL, 到点即视为不存在(惰性过期);
//   - 字符串与哈希是两套结构, 同一个 key 不会混用。
//
// 有了它, 跨实例配对与并发配对这两件最难用肉眼确认的事, 才真的被跑过。
// 它与真实 Redis 的差别(网络错误、集群、命令级原子性细节)由
// TestRedisManagerAgainstRealRedis 用真实实例覆盖(需要 REDIS_ADDR)。
type memOps struct {
	mu      sync.Mutex
	now     func() time.Time
	strs    map[string]string
	hashes  map[string]map[string]string
	zsets   map[string]map[string]float64
	expires map[string]time.Time
}

func newMemOps(now func() time.Time) *memOps {
	return &memOps{
		now: now, strs: map[string]string{}, hashes: map[string]map[string]string{},
		zsets: map[string]map[string]float64{}, expires: map[string]time.Time{},
	}
}

// expiredLocked 检查并清除已到期的 key(惰性过期, 与 Redis 一致)。
func (m *memOps) expiredLocked(key string) bool {
	at, ok := m.expires[key]
	if ok && !at.IsZero() && m.now().After(at) {
		delete(m.strs, key)
		delete(m.hashes, key)
		delete(m.zsets, key)
		delete(m.expires, key)
	}
	_, stillThere := m.strs[key]
	if !stillThere {
		if _, ok := m.hashes[key]; ok {
			stillThere = true
		}
	}
	if !stillThere {
		if _, ok := m.zsets[key]; ok {
			stillThere = true
		}
	}
	return !stillThere
}

func (m *memOps) SetNX(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.expiredLocked(key) {
		return false, nil
	}
	m.strs[key] = value
	m.setTTLLocked(key, ttl)
	return true, nil
}

func (m *memOps) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		return "", false, nil
	}
	v, ok := m.strs[key]
	return v, ok, nil
}

func (m *memOps) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strs[key] = value
	m.setTTLLocked(key, ttl)
	return nil
}

func (m *memOps) Del(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.strs, k)
		delete(m.hashes, k)
		delete(m.zsets, k)
		delete(m.expires, k)
	}
	return nil
}

// DelIfValue 实现"值匹配才删除": 解锁必须用它, 否则会把别人刚拿到的锁删掉。
func (m *memOps) DelIfValue(_ context.Context, key, value string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		return false, nil
	}
	if m.strs[key] != value {
		return false, nil
	}
	delete(m.strs, key)
	delete(m.expires, key)
	return true, nil
}

func (m *memOps) HSet(_ context.Context, key string, values map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		m.hashes[key] = map[string]string{}
	}
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	for f, v := range values {
		// Redis 的值是**二进制安全**的: []byte 会原样存进去, 而 fmt.Sprint
		// 会把它渲染成 "[123 34 100 ...]" 这样的数字列表 —— 用它当替身会
		// 把"房间数据损坏"这种假故障造出来(这个坑我踩过一次)。
		switch b := v.(type) {
		case []byte:
			m.hashes[key][f] = string(b)
		case string:
			m.hashes[key][f] = b
		default:
			m.hashes[key][f] = fmt.Sprint(v)
		}
	}
	return nil
}

func (m *memOps) HGet(_ context.Context, key, field string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		return "", false, nil
	}
	v, ok := m.hashes[key][field]
	return v, ok, nil
}

func (m *memOps) Expire(_ context.Context, key string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setTTLLocked(key, ttl)
	return nil
}

func (m *memOps) ZAdd(_ context.Context, key string, score float64, member string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		m.zsets[key] = map[string]float64{}
	}
	if m.zsets[key] == nil {
		m.zsets[key] = map[string]float64{}
	}
	m.zsets[key][member] = score
	return nil
}

// ZPopMin 弹出分数最小者; 分数相同时取字典序最小者 —— 与 Redis 一致。
func (m *memOps) ZPopMin(_ context.Context, key string) (string, float64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) || len(m.zsets[key]) == 0 {
		return "", 0, false, nil
	}
	type entry struct {
		member string
		score  float64
	}
	list := make([]entry, 0, len(m.zsets[key]))
	for member, score := range m.zsets[key] {
		list = append(list, entry{member, score})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score == list[j].score {
			return list[i].member < list[j].member
		}
		return list[i].score < list[j].score
	})
	best := list[0]
	delete(m.zsets[key], best.member)
	return best.member, best.score, true, nil
}

func (m *memOps) ZRemRangeByScore(_ context.Context, key string, min, max float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		return nil
	}
	for member, score := range m.zsets[key] {
		if score >= min && score <= max {
			delete(m.zsets[key], member)
		}
	}
	return nil
}

func (m *memOps) ZRem(_ context.Context, key string, members ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, m2 := range members {
		delete(m.zsets[key], m2)
	}
	return nil
}

func (m *memOps) ZRevRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(key) {
		return nil, nil
	}
	type entry struct {
		member string
		score  float64
	}
	list := make([]entry, 0, len(m.zsets[key]))
	for member, score := range m.zsets[key] {
		list = append(list, entry{member, score})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score == list[j].score {
			return list[i].member > list[j].member
		}
		return list[i].score > list[j].score
	})
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.member)
	}
	if start < 0 {
		start = 0
	}
	if stop >= int64(len(out)) {
		stop = int64(len(out)) - 1
	}
	if start > stop || len(out) == 0 {
		return nil, nil
	}
	return out[start : stop+1], nil
}

func (m *memOps) setTTLLocked(key string, ttl time.Duration) {
	if ttl <= 0 {
		delete(m.expires, key)
		return
	}
	m.expires[key] = m.now().Add(ttl)
}

// twoInstances 模拟"两个进程共用一个 Redis": 两个 RedisManager 实例,
// 一个共享的命令实现。这正是多副本部署的情形。
func twoInstances(now func() time.Time) (*RedisManager, *RedisManager, *memOps) {
	ops := newMemOps(now)
	cfg := Config{WaitTTL: 15 * time.Minute, PairedTTL: time.Hour, Now: now}
	return NewRedisManagerWithOps(ops, "t:", cfg), NewRedisManagerWithOps(ops, "t:", cfg), ops
}

// TestRedisPairingWorksAcrossInstances 是这次改动的核心用例: 两个人分别在
// 两个实例上排队, 必须被配进同一间房。
func TestRedisPairingWorksAcrossInstances(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a, b, _ := twoInstances(clock)

	// 实例 A 上的候选人先排队。
	roomA, roleA, err := a.Join("tenant-a", "u_1", "陈雨", "后端一面", RoleInterviewee)
	if err != nil {
		t.Fatalf("实例 A 排队失败: %v", err)
	}
	if roomA.State != StateWaiting || roleA != RoleInterviewee {
		t.Fatalf("第一个人应当等待: %v / %s", roomA.State, roleA)
	}
	// 实例 B 上的面试官进来 —— 他看不到 A 的内存, 只能靠 Redis。
	roomB, roleB, err := b.Join("tenant-a", "u_2", "王琳", "后端一面", RoleInterviewer)
	if err != nil {
		t.Fatalf("实例 B 配对失败: %v", err)
	}
	if roomB.ID != roomA.ID {
		t.Fatalf("跨实例必须配进同一间房: %s vs %s", roomB.ID, roomA.ID)
	}
	if roomB.State != StatePaired || roleB != RoleInterviewer {
		t.Fatalf("应配对成功: %v / %s", roomB.State, roleB)
	}
	if len(roomB.Participants) != 2 {
		t.Fatalf("房间里应有两名参与者: %+v", roomB.Participants)
	}
	// 两个实例读到的房间必须一致(状态在 Redis 里, 不在各自内存里)。
	for name, mgr := range map[string]*RedisManager{"A": a, "B": b} {
		got, err := mgr.Get("tenant-a", roomA.ID)
		if err != nil {
			t.Fatalf("实例 %s 读取房间失败: %v", name, err)
		}
		if got.State != StatePaired || len(got.Participants) != 2 {
			t.Fatalf("实例 %s 看到的房间不对: %+v", name, got)
		}
	}
	if a.Backend() != "redis" || b.Backend() != "redis" {
		t.Fatal("自检必须如实报告用的是 Redis 后端")
	}
}

// TestRedisPairingUnderConcurrency 用并发压出"三个人挤进两间房"这类错误。
func TestRedisPairingUnderConcurrency(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	a, b, _ := twoInstances(func() time.Time { return now })
	instances := []*RedisManager{a, b}

	const pairs = 12
	var wg sync.WaitGroup
	results := make([][2]string, pairs*2) // [roomID, role]
	errCh := make(chan error, pairs*2)

	for i := 0; i < pairs*2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 一半扮演候选人, 一半扮演面试官; 轮流用两个实例, 逼近真实分布。
			want := RoleInterviewee
			if i%2 == 1 {
				want = RoleInterviewer
			}
			mgr := instances[i%2]
			room, role, err := mgr.Join("tenant-a", fmt.Sprintf("u_%02d", i), "参与者", "并发对练", want)
			if err != nil {
				errCh <- err
				return
			}
			results[i] = [2]string{room.ID, string(role)}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发加入失败: %v", err)
	}

	// 每个房间最多两个人: 这是"认领必须原子"的直接后果。
	rooms := a.List("tenant-a")
	occupancy := map[string]int{}
	for _, room := range rooms {
		occupancy[room.ID] = len(room.Participants)
		if len(room.Participants) > 2 {
			t.Fatalf("房间 %s 里挤进了 %d 个人", room.ID, len(room.Participants))
		}
	}
	// 同一个人不能出现在两个房间里。
	seen := map[string]string{}
	for _, room := range rooms {
		for _, p := range room.Participants {
			if prev, dup := seen[p.UserID]; dup {
				t.Fatalf("参与者 %s 同时出现在 %s 与 %s", p.UserID, prev, room.ID)
			}
			seen[p.UserID] = room.ID
		}
	}
	// 并发下的正确结果: 12 对全部配上, 没有落单。
	stats := a.Stats("tenant-a")
	if stats["paired"] != pairs {
		t.Fatalf("应配成 %d 间房, 实际 %+v", pairs, stats)
	}
	if stats["waiting"] != 0 {
		t.Fatalf("不应留下等待中的房间: %+v", stats)
	}
}

func TestRedisPairingRulesMatchMemoryImplementation(t *testing.T) {
	// 用**递进**的时钟, 而不是冻结的时钟。
	//
	// 冻结时钟下所有房间的入队分数完全相同, 于是 ZPopMin 的并列规则
	// (按成员字典序) 决定了先配谁 —— 而那个顺序取决于随机房间号的字母序,
	// 与"先来先配"无关。真实场景里时间会前进, 因此这里也用前进的时钟,
	// 否则测的是随机性而不是规则。
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	step := time.Duration(0)
	clock := func() time.Time {
		step += time.Second
		return base.Add(step)
	}
	a, b, _ := twoInstances(clock)

	// 同角色不配对: 两个面试官各自等待。
	first, _, _ := a.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	second, role, _ := b.Join("tenant-a", "u_2", "乙", "", RoleInterviewer)
	if second.ID == first.ID || role != RoleInterviewer || second.State != StateWaiting {
		t.Fatalf("同角色不应配对: %+v / %s", second.State, role)
	}
	// any 补齐对方空缺: 甲在等面试官, 丙说"都可以" -> 丙当候选人。
	third, role3, _ := a.Join("tenant-a", "u_3", "丙", "", RoleAny)
	if third.ID != first.ID || role3 != RoleInterviewee || third.State != StatePaired {
		t.Fatalf("any 应补齐空缺角色: %s / %s", role3, third.State)
	}
	// 跨租户不配对。
	other, _, _ := b.Join("tenant-b", "u_9", "丁", "", RoleInterviewee)
	if other.State != StateWaiting {
		t.Fatalf("跨租户不应配对: %v", other.State)
	}
	// 重复 join 幂等: 回到原房间, 不新建。
	again, roleAgain, _ := b.Join("tenant-a", "u_1", "甲", "", RoleAny)
	if again.ID != first.ID || roleAgain != RoleInterviewer {
		t.Fatalf("重复 join 应回到原房间: %s / %s", again.ID, roleAgain)
	}
	if stats := a.Stats("tenant-a"); stats["waiting"] != 1 {
		t.Fatalf("只应剩一间等待房(乙): %+v", stats)
	}
}

func TestRedisLeaveEndsRoomAndClearsQueue(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	a, b, _ := twoInstances(func() time.Time { return now })

	room, _, _ := a.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	paired, _, _ := b.Join("tenant-a", "u_2", "乙", "", RoleInterviewee)

	updated, ended, err := b.Leave("tenant-a", paired.ID, "u_2")
	if err != nil || !ended || updated.State != StateEnded {
		t.Fatalf("离开应结束整间房: %v %v %v", err, ended, updated.State)
	}
	// 另一方在**另一个实例**上也必须立刻看到已结束。
	got, err := a.Get("tenant-a", room.ID)
	if err != nil || got.State != StateEnded {
		t.Fatalf("另一个实例应看到已结束: %v %v", got.State, err)
	}
	// 结束后两人的指针被清掉, 因此可以立刻重新排队(而不是被判为"你还在房间里")。
	fresh, _, err := a.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	if err != nil {
		t.Fatalf("结束后应能重新排队: %v", err)
	}
	if fresh.ID == room.ID || fresh.State != StateWaiting {
		t.Fatalf("应开一间新的等待房: %+v", fresh)
	}
	if _, _, err := b.Leave("tenant-a", room.ID, "u_99"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("非参与者离开应 ErrNotMember, 实际 %v", err)
	}
}

func TestRedisWaitingRoomsExpireByTTL(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ops := newMemOps(clock)
	cfg := Config{WaitTTL: 10 * time.Minute, PairedTTL: time.Hour, Now: clock}
	a := NewRedisManagerWithOps(ops, "t:", cfg)
	b := NewRedisManagerWithOps(ops, "t:", cfg)

	room, _, _ := a.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	// 超时之后房间应随 TTL 消失, 而不是留在队列里等人配。
	now = now.Add(20 * time.Minute)
	if _, err := b.Get("tenant-a", room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("过期房间应读不到, 实际 %v", err)
	}
	// 新来的人不会撞上幽灵房间。
	fresh, _, err := b.Join("tenant-a", "u_2", "乙", "", RoleInterviewer)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if fresh.ID == room.ID || fresh.State != StateWaiting {
		t.Fatalf("应是一间全新的等待房: %+v", fresh)
	}
	if stats := a.Stats("tenant-a"); stats["waiting"] != 1 {
		t.Fatalf("只应有一间等待房: %+v", stats)
	}
}

func TestRedisClaimedButDeadRoomIsSkipped(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	a, b, ops := twoInstances(func() time.Time { return now })

	room, _, _ := a.Join("tenant-a", "u_1", "甲", "", RoleInterviewee)
	// 模拟"认领之后房间坏了": 直接把房间数据删掉, 但队列里还留着它。
	_ = ops.Del(context.Background(), a.roomKey(room.ID))

	// 配对方必须跳过这个坏房间, 并给自己开一间新的等待房, 而不是把
	// 用户配进一个不存在的房间。
	got, role, err := b.Join("tenant-a", "u_2", "乙", "", RoleInterviewer)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if got.ID == room.ID {
		t.Fatalf("不应配进已损坏的房间: %+v", got)
	}
	if role != RoleInterviewer || got.State != StateWaiting {
		t.Fatalf("应变成自己等待: %s / %v", role, got.State)
	}
}
