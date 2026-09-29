// Package practice 实现"真人双向对练"的配对。
//
// 与 AI 面试的区别在于谁提问: AI 面试里流程由状态机驱动, 两个人各自面对
// 一套界面即可; 对练是两个真人连进同一间房, 因此必须先解决"谁和谁连、
// 谁扮演什么角色"——这就是本包的全部职责。
//
// 三条设计决定:
//  1. 配对状态放内存, 对练记录走审计。等待队列是秒级临时状态, 放进数据库
//     只会留下"进程崩溃后一堆永远配不上的房间"; 而"谁和谁对练过"需要长期留存。
//  2. 角色要显式表态。两人对练时谁扮面试官不能靠猜, 否则会出现两边都在提问
//     或者两边都在等的死局。
//  3. 等待房间必须有超时, 否则用户关掉页面就会留下一个空房间, 把后来的
//     面试官配进去。
package practice

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Role 是参与者在一次对练里扮演的角色。
type Role string

const (
	// RoleInterviewer 提问方(模拟面试官)。
	RoleInterviewer Role = "interviewer"
	// RoleInterviewee 作答方(模拟候选人)。
	RoleInterviewee Role = "interviewee"
	// RoleAny 都可以 —— 由系统按对方的需要补齐角色。
	RoleAny Role = "any"
)

// State 是房间状态。
type State string

const (
	// StateWaiting 等待配对。
	StateWaiting State = "waiting"
	// StatePaired 已配对。
	StatePaired State = "paired"
	// StateEnded 已结束。
	StateEnded State = "ended"
)

var (
	// ErrRoomNotFound 房间不存在或已过期。
	ErrRoomNotFound = errors.New("practice: 房间不存在或已过期")
	// ErrNotMember 不是该房间的参与者。
	ErrNotMember = errors.New("practice: 你不是这个房间的参与者")
	// ErrInvalidRole 角色取值不合法。
	ErrInvalidRole = errors.New("practice: 角色不合法")
)

// Participant 是一位参与者。
type Participant struct {
	UserID   string    `json:"user_id"`
	Name     string    `json:"name"`
	Role     Role      `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// Room 是一次对练。
type Room struct {
	ID           string        `json:"room_id"`
	TenantID     string        `json:"tenant_id"`
	Topic        string        `json:"topic"`
	State        State         `json:"state"`
	Participants []Participant `json:"participants"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	EndedAt      time.Time     `json:"ended_at,omitempty"`
}

// Full 表示房间已经有两个参与者。
func (r Room) Full() bool { return len(r.Participants) >= 2 }

// Participant 按用户 ID 取参与者。
func (r Room) Participant(userID string) (Participant, bool) {
	for _, p := range r.Participants {
		if p.UserID == userID {
			return p, true
		}
	}
	return Participant{}, false
}

// Has 判断某人是否在房间里。
func (r Room) Has(userID string) bool {
	_, ok := r.Participant(userID)
	return ok
}

// Peer 返回对方的参与者信息(对练房只有两个人)。
//
// 先确认调用者确实是本房间的人: 否则"取对方"会对任何陌生 ID 都返回
// 第一个参与者的信息 —— 在接口层就等于泄露了房间里的人是谁。
// 这个漏洞是写测试时发现的: 传入一个不在房间里的 ID, 它照样返回了对方。
func (r Room) Peer(userID string) (Participant, bool) {
	if !r.Has(userID) {
		return Participant{}, false
	}
	for _, p := range r.Participants {
		if p.UserID != userID {
			return p, true
		}
	}
	return Participant{}, false
}

// Config 配置配对管理器。
type Config struct {
	// WaitTTL 等待房间存活时间, 默认 15 分钟。
	WaitTTL time.Duration
	// PairedTTL 已配对房间存活时间, 默认 3 小时。
	PairedTTL time.Duration
	Now       func() time.Time
}

// Manager 维护等待队列与房间。
//
// 单实例内存实现。配对是秒级且要求强一致的操作: 多副本部署时需要把等待队列
// 放到 Redis 并加锁, 否则两个人可能被配进不同的房间。接口留成方法而不是
// 直接暴露 map, 就是为了将来换实现时上层不用改。
type Manager struct {
	mu     sync.Mutex
	rooms  map[string]*Room
	config Config
	seq    int64
}

// NewManager 构造配对管理器。
func NewManager(cfg Config) *Manager {
	if cfg.WaitTTL <= 0 {
		cfg.WaitTTL = 15 * time.Minute
	}
	if cfg.PairedTTL <= 0 {
		cfg.PairedTTL = 3 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{rooms: make(map[string]*Room), config: cfg}
}

// Join 加入配对, 返回房间与"我这次扮演的角色"。
//
// 规则全部显式, 不靠猜:
//   - 声明 interviewer: 找正在等 interviewee 的房间; 找不到就自己等;
//   - 声明 interviewee: 找正在等 interviewer 的房间; 找不到就自己等;
//   - 声明 any: 有等待房间就补齐对方的空缺, 否则按 interviewee 等待
//     (想练的人通常是想被面试的那一方)。
func (m *Manager) Join(tenantID, userID, name, topic string, want Role) (Room, Role, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(tenantID) == "" {
		return Room{}, "", fmt.Errorf("%w: 缺少账号或租户", ErrNotMember)
	}
	switch want {
	case RoleInterviewer, RoleInterviewee, RoleAny:
	default:
		return Room{}, "", ErrInvalidRole
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()

	// 已经在房间里就直接返回原房间: 刷新页面后重复 join 是常态。
	for _, room := range m.rooms {
		if room.TenantID == tenantID && room.State != StateEnded && room.Has(userID) {
			p, _ := room.Participant(userID)
			return *room, p.Role, nil
		}
	}

	if seat := m.findWaitingLocked(tenantID, want); seat != nil {
		role := complementaryRole(seat.need, want)
		now := m.config.Now()
		seat.room.Participants = append(seat.room.Participants, Participant{
			UserID: userID, Name: displayName(name), Role: role, JoinedAt: now,
		})
		seat.room.State = StatePaired
		seat.room.UpdatedAt = now
		if strings.TrimSpace(seat.room.Topic) == "" {
			seat.room.Topic = defaultTopic(topic)
		}
		return *seat.room, role, nil
	}

	role := want
	if role == RoleAny {
		role = RoleInterviewee
	}
	now := m.config.Now()
	room := &Room{
		ID:       m.nextRoomIDLocked(),
		TenantID: tenantID,
		Topic:    defaultTopic(topic),
		State:    StateWaiting,
		Participants: []Participant{{
			UserID: userID, Name: displayName(name), Role: role, JoinedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.rooms[room.ID] = room
	return *room, role, nil
}

// waitingSeat 表示"某个等待房间在等什么角色"。
type waitingSeat struct {
	room *Room
	need Role
}

// findWaitingLocked 找一个能配上的等待房间。
// 选最早创建的: 先来的人先配对, 否则等得久的人会被反复插队。
func (m *Manager) findWaitingLocked(tenantID string, want Role) *waitingSeat {
	var best *waitingSeat
	for _, room := range m.rooms {
		if room.TenantID != tenantID || room.State != StateWaiting || room.Full() {
			continue
		}
		if len(room.Participants) == 0 {
			continue
		}
		occupant := room.Participants[0]
		if !canPair(want, occupant.Role) {
			continue
		}
		if best == nil || room.CreatedAt.Before(best.room.CreatedAt) {
			best = &waitingSeat{room: room, need: occupant.Role}
		}
	}
	return best
}

// canPair 判断"我想扮演 want"能否补进房间里 occupant 的位置。
func canPair(want, occupant Role) bool {
	if want == RoleAny {
		return true
	}
	// 两人对练必须一正一反: 同角色会变成两个面试官面面相觑。
	return occupant != want
}

// complementaryRole 决定第二个人的角色, 一定与房间里的人相反。
func complementaryRole(occupant, want Role) Role {
	if want != RoleAny {
		return want
	}
	if occupant == RoleInterviewer {
		return RoleInterviewee
	}
	return RoleInterviewer
}

// Leave 离开房间, 返回更新后的房间以及"房间是否已结束"。
func (m *Manager) Leave(tenantID, roomID, userID string) (Room, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	room, ok := m.rooms[roomID]
	if !ok || room.TenantID != tenantID {
		return Room{}, false, ErrRoomNotFound
	}
	if !room.Has(userID) {
		return *room, false, ErrNotMember
	}
	// 对练是双向的: 一方离开整间房就结束。否则页面会显示一个已经没人的房间。
	room.State = StateEnded
	room.EndedAt = m.config.Now()
	room.UpdatedAt = m.config.Now()
	return *room, true, nil
}

// Get 读取房间(顺带做超时清理)。
func (m *Manager) Get(tenantID, roomID string) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	room, ok := m.rooms[roomID]
	if !ok || room.TenantID != tenantID {
		return Room{}, ErrRoomNotFound
	}
	return *room, nil
}

// List 列出本租户当前的对练房(工作台用)。
func (m *Manager) List(tenantID string) []Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	out := make([]Room, 0, len(m.rooms))
	for _, room := range m.rooms {
		if room.TenantID == tenantID {
			out = append(out, *room)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Stats 返回本租户的配对概况(等人 / 对练中 / 已结束)。
func (m *Manager) Stats(tenantID string) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	out := map[string]int{"waiting": 0, "paired": 0, "ended": 0}
	for _, room := range m.rooms {
		if room.TenantID == tenantID {
			out[string(room.State)]++
		}
	}
	return out
}

// sweepLocked 清理超时房间。
//
// 必须真的删除等待中的房间: 配对靠遍历房间完成, 留下大量过期房间会让每次
// Join 都变慢; 而"已结束"保留几分钟是为了让双方页面能看到结束状态。
func (m *Manager) sweepLocked() {
	now := m.config.Now()
	for id, room := range m.rooms {
		switch room.State {
		case StateWaiting:
			if now.Sub(room.UpdatedAt) > m.config.WaitTTL {
				delete(m.rooms, id)
			}
		case StatePaired:
			if now.Sub(room.UpdatedAt) > m.config.PairedTTL {
				room.State = StateEnded
				room.EndedAt = now
				room.UpdatedAt = now
			}
		case StateEnded:
			if now.Sub(room.UpdatedAt) > 5*time.Minute {
				delete(m.rooms, id)
			}
		}
	}
}

func (m *Manager) nextRoomIDLocked() string {
	m.seq++
	return fmt.Sprintf("pr_%d_%02d", m.config.Now().Unix(), m.seq%100)
}

func displayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "参与者"
	}
	return name
}

func defaultTopic(topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return "自由对练"
	}
	return topic
}
