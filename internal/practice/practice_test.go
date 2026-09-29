package practice

import (
	"errors"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return NewManager(Config{
		WaitTTL:   10 * time.Minute,
		PairedTTL: time.Hour,
		Now:       func() time.Time { return now },
	})
}

func TestFirstJoinerWaitsSecondGetsPaired(t *testing.T) {
	m := newTestManager(t)
	room, role, err := m.Join("tenant-a", "u_1", "陈雨", "后端一面", RoleInterviewer)
	if err != nil {
		t.Fatalf("第一个加入失败: %v", err)
	}
	if room.State != StateWaiting || role != RoleInterviewer {
		t.Fatalf("第一个人应当等待并扮演面试官: %+v / %s", room.State, role)
	}
	if room.Topic != "后端一面" {
		t.Fatalf("主题应被保留: %q", room.Topic)
	}

	paired, role2, err := m.Join("tenant-a", "u_2", "王琳", "另一个主题", RoleInterviewee)
	if err != nil {
		t.Fatalf("第二个加入失败: %v", err)
	}
	if paired.ID != room.ID {
		t.Fatalf("两个人必须在同一间房: %s vs %s", paired.ID, room.ID)
	}
	if paired.State != StatePaired || len(paired.Participants) != 2 {
		t.Fatalf("应当配对成功: %+v", paired)
	}
	if role2 != RoleInterviewee {
		t.Fatalf("第二个人应是候选人, 实际 %s", role2)
	}
	// 第一间房的主题不能被后加入者改写: 两人必须看到同一句话。
	if paired.Topic != "后端一面" {
		t.Fatalf("主题被覆盖成了 %q", paired.Topic)
	}
}

func TestAnyRoleFillsOppositeSeat(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer); err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	// 第二个人"都可以": 应当补上候选人位置, 而不是也当面试官。
	room, role, err := m.Join("tenant-a", "u_2", "乙", "", RoleAny)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if role != RoleInterviewee {
		t.Fatalf("any 应补齐对方的空缺角色, 实际 %s", role)
	}
	if room.State != StatePaired {
		t.Fatalf("应配对成功: %v", room.State)
	}
	// 没有人等待时, any 默认按候选人等待(想练的人通常是被面试的一方)。
	if room2, role2, _ := m.Join("tenant-b", "u_3", "丙", "", RoleAny); role2 != RoleInterviewee || room2.State != StateWaiting {
		t.Fatalf("无人可配时应按候选人等待: %v / %s", room2.State, role2)
	}
}

func TestSameRoleDoesNotPair(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer); err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	// 第二个也想当面试官: 不能和第一个凑成一对(会变成两个面试官面面相觑),
	// 而应该自己开一间等待房。
	room, role, err := m.Join("tenant-a", "u_2", "乙", "", RoleInterviewer)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if room.State != StateWaiting || role != RoleInterviewer {
		t.Fatalf("同角色不应配对, 实际 %v / %s", room.State, role)
	}
	if stats := m.Stats("tenant-a"); stats["waiting"] != 2 {
		t.Fatalf("应有两间等待房, 实际 %+v", stats)
	}
}

func TestPairingIsTenantScoped(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer); err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	// 另一个租户的人不能被配进来 —— 那会让两家公司的人进同一间房。
	room, _, err := m.Join("tenant-b", "u_2", "乙", "", RoleInterviewee)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if room.State != StateWaiting {
		t.Fatalf("跨租户不应配对, 实际 %v", room.State)
	}
}

func TestJoinIsIdempotentForExistingParticipant(t *testing.T) {
	m := newTestManager(t)
	first, _, _ := m.Join("tenant-a", "u_1", "甲", "主题", RoleInterviewer)
	// 刷新页面会重复 join: 必须回到原房间, 而不是又开一间。
	again, role, err := m.Join("tenant-a", "u_1", "甲", "主题", RoleAny)
	if err != nil {
		t.Fatalf("重复加入失败: %v", err)
	}
	if again.ID != first.ID || role != RoleInterviewer {
		t.Fatalf("重复加入应回到原房间并保持原角色: %s / %s", again.ID, role)
	}
	if stats := m.Stats("tenant-a"); stats["waiting"] != 1 {
		t.Fatalf("不应新增房间: %+v", stats)
	}
}

func TestLeaveEndsRoomForBothSides(t *testing.T) {
	m := newTestManager(t)
	room, _, _ := m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	paired, _, _ := m.Join("tenant-a", "u_2", "乙", "", RoleInterviewee)

	updated, ended, err := m.Leave("tenant-a", paired.ID, "u_1")
	if err != nil {
		t.Fatalf("离开失败: %v", err)
	}
	if !ended || updated.State != StateEnded {
		t.Fatalf("一方离开应结束整间房: %v / %v", ended, updated.State)
	}
	// 另一方读到的也是已结束 —— 页面上不该停留在一个"还有人"的房间。
	if got, err := m.Get("tenant-a", room.ID); err != nil || got.State != StateEnded {
		t.Fatalf("另一方应看到已结束: %v %v", got.State, err)
	}
	// 外人不能离开别人的房间。
	if _, _, err := m.Leave("tenant-a", paired.ID, "u_3"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("非参与者离开应 ErrNotMember, 实际 %v", err)
	}
	if _, _, err := m.Leave("tenant-b", paired.ID, "u_1"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("跨租户应 ErrRoomNotFound, 实际 %v", err)
	}
}

func TestWaitingRoomsExpire(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	m := NewManager(Config{WaitTTL: time.Minute, Now: func() time.Time { return now }})
	room, _, _ := m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)

	// 注意这里只有一个人加入: 一旦第二个人进来就会配对成功, 房间状态
	// 变成 paired, 那走的就是 PairedTTL 而不是 WaitTTL 了(测试第一版
	// 就是这么写错的 —— 它验证的其实是另一条规则)。
	if room.State != StateWaiting {
		t.Fatalf("第一个人应当处于等待状态: %v", room.State)
	}
	now = now.Add(2 * time.Minute)
	if _, err := m.Get("tenant-a", room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("过期房间应被清理, 实际 %v", err)
	}
	if stats := m.Stats("tenant-a"); stats["waiting"] != 0 {
		t.Fatalf("过期后不应还有等待房: %+v", stats)
	}
	// 清掉之后, 新来的人应该能正常开一间新房, 而不是被"幽灵房间"干扰。
	fresh, _, err := m.Join("tenant-a", "u_9", "丁", "", RoleInterviewer)
	if err != nil {
		t.Fatalf("加入失败: %v", err)
	}
	if fresh.ID == room.ID {
		t.Fatal("过期房间不应被复用")
	}
}

func TestPairedRoomExpiresIntoEnded(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	m := NewManager(Config{PairedTTL: 30 * time.Minute, Now: func() time.Time { return now }})
	_, _, _ = m.Join("tenant-a", "u_1", "甲", "", RoleInterviewer)
	room, _, _ := m.Join("tenant-a", "u_2", "乙", "", RoleInterviewee)

	now = now.Add(time.Hour)
	got, err := m.Get("tenant-a", room.ID)
	if err != nil {
		t.Fatalf("读取房间失败: %v", err)
	}
	if got.State != StateEnded {
		t.Fatalf("超时的对练房应转为已结束, 实际 %v", got.State)
	}
}

func TestJoinRejectsBadInput(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.Join("tenant-a", "u_1", "甲", "", Role("boss")); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("非法角色应被拒绝, 实际 %v", err)
	}
	if _, _, err := m.Join("tenant-a", "", "甲", "", RoleAny); err == nil {
		t.Fatal("缺少账号应被拒绝")
	}
}

func TestPeerAndTopicDefaults(t *testing.T) {
	m := newTestManager(t)
	_, _, _ = m.Join("tenant-a", "u_1", "甲", "  ", RoleInterviewer)
	room, _, _ := m.Join("tenant-a", "u_2", "乙", "", RoleInterviewee)
	if room.Topic != "自由对练" {
		t.Fatalf("空主题应有默认值, 实际 %q", room.Topic)
	}
	peer, ok := room.Peer("u_1")
	if !ok || peer.UserID != "u_2" || peer.Name != "乙" {
		t.Fatalf("对练房应能取到对方: %+v", peer)
	}
	if _, ok := room.Peer("u_3"); ok {
		t.Fatal("房间外的人不应取到对方")
	}
}
