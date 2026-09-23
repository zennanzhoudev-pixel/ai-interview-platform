package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore 是内存实现, 默认后端。
//
// 它的存在让整条链路在没有 MySQL 的机器上也能完整跑起来 ——
// 这对演示、CI 和新人上手都很重要。但不要把当它当"临时方案":
// 行为契约测试保证了它与 MySQL 实现语义一致。
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	turns    map[string][]Turn
	reports  map[string]Report
	consents map[string][]Consent
	now      func() time.Time
}

// NewMemoryStore 构造内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]Session),
		turns:    make(map[string][]Turn),
		reports:  make(map[string]Report),
		consents: make(map[string][]Consent),
		now:      time.Now,
	}
}

func (m *MemoryStore) CreateSession(_ context.Context, s Session) error {
	if s.ID == "" {
		return fmt.Errorf("store: 会话 ID 不能为空")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return fmt.Errorf("store: 会话 %s 已存在", s.ID)
	}
	now := m.now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.Status == "" {
		s.Status = StatusRunning
	}
	s.UpdatedAt = now
	m.sessions[s.ID] = s
	return nil
}

func (m *MemoryStore) UpdateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.sessions[s.ID]
	if !ok {
		return ErrNotFound
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = old.CreatedAt
	}
	s.UpdatedAt = m.now()
	m.sessions[s.ID] = s
	return nil
}

func (m *MemoryStore) GetSession(_ context.Context, id string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryStore) ListSessions(_ context.Context, tenantID string, limit int) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if tenantID != "" && s.TenantID != tenantID {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) AppendTurn(_ context.Context, t Turn) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := m.turns[t.SessionID]
	for _, existing := range list {
		if existing.Index == t.Index {
			// 消息队列至少一次投递: 重复的那条直接忽略, 保证幂等。
			// 与 MySQL 实现的 ON DUPLICATE KEY UPDATE 语义保持一致。
			return nil
		}
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = m.now()
	}
	m.turns[t.SessionID] = append(list, t)
	return nil
}

func (m *MemoryStore) ListTurns(_ context.Context, sessionID string) ([]Turn, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.turns[sessionID]
	out := make([]Turn, len(list))
	copy(out, list)
	// 按序号排序, 与 MySQL 的 "ORDER BY turn_index" 对齐:
	// 上层的报告生成依赖问答顺序, 这个顺序不能依赖写入时序。
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func (m *MemoryStore) SaveReport(_ context.Context, r Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now()
	}
	m.reports[r.SessionID] = r
	return nil
}

func (m *MemoryStore) GetReport(_ context.Context, sessionID string) (Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.reports[sessionID]
	if !ok {
		return Report{}, ErrNotFound
	}
	return r, nil
}

func (m *MemoryStore) SaveConsent(_ context.Context, c Consent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.AgreedAt.IsZero() {
		c.AgreedAt = m.now()
	}
	for _, existing := range m.consents[c.SessionID] {
		if existing.Scope == c.Scope {
			return nil // 同一范围重复授权: 保留最早那条, 时间的先后有法律意义
		}
	}
	m.consents[c.SessionID] = append(m.consents[c.SessionID], c)
	return nil
}

func (m *MemoryStore) ListConsents(_ context.Context, sessionID string) ([]Consent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.consents[sessionID]
	out := make([]Consent, len(list))
	copy(out, list)
	return out, nil
}

func (m *MemoryStore) Close() error { return nil }
