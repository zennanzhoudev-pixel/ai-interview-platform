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
// 这对演示、CI 和新人上手都很重要。但不要把它当"临时方案":
// 行为契约测试保证了它与 MySQL 实现语义一致, 包括租户隔离。
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	turns    map[string][]Turn
	reports  map[string]Report
	consents map[string][]Consent
	audit    []AuditEntry
	nextID   int64
	now      func() time.Time

	// 业务域(面试之外)的集合。与上面分开命名, 是为了让"热路径"与
	// "管理路径"在代码里也保持视觉上的分离。
	jobs         map[string]Job
	candidates   map[string]Candidate
	applications map[string]Application
	questions    map[string]QuestionItem
	schedules    map[string]Schedule
	recordings   map[string]Recording
}

// NewMemoryStore 构造内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:     make(map[string]Session),
		turns:        make(map[string][]Turn),
		reports:      make(map[string]Report),
		consents:     make(map[string][]Consent),
		jobs:         make(map[string]Job),
		candidates:   make(map[string]Candidate),
		applications: make(map[string]Application),
		questions:    make(map[string]QuestionItem),
		schedules:    make(map[string]Schedule),
		recordings:   make(map[string]Recording),
		now:          time.Now,
	}
}

func (m *MemoryStore) CreateSession(_ context.Context, s Session) error {
	if s.ID == "" {
		return fmt.Errorf("store: 会话 ID 不能为空")
	}
	if s.TenantID == "" {
		return fmt.Errorf("store: 租户 ID 不能为空")
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
	if !ok || old.TenantID != s.TenantID {
		return ErrNotFound
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = old.CreatedAt
	}
	s.UpdatedAt = m.now()
	m.sessions[s.ID] = s
	return nil
}

// GetSession 按租户读取会话。跨租户一律返回 ErrNotFound。
func (m *MemoryStore) GetSession(_ context.Context, tenantID, sessionID string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok || s.TenantID != tenantID {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryStore) ListSessions(_ context.Context, tenantID string, limit int) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.TenantID != tenantID {
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

	sess, ok := m.sessions[t.SessionID]
	if !ok || sess.TenantID != t.TenantID {
		return ErrNotFound
	}
	list := m.turns[t.SessionID]
	for _, existing := range list {
		if existing.Index == t.Index {
			// 消息队列至少一次投递: 重复的那条直接忽略, 保证幂等。
			return nil
		}
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = m.now()
	}
	m.turns[t.SessionID] = append(list, t)
	return nil
}

func (m *MemoryStore) ListTurns(_ context.Context, tenantID, sessionID string) ([]Turn, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if s, ok := m.sessions[sessionID]; !ok || s.TenantID != tenantID {
		return nil, ErrNotFound
	}
	list := m.turns[sessionID]
	out := make([]Turn, len(list))
	copy(out, list)
	// 按序号排序, 与 MySQL 的 "ORDER BY turn_index" 对齐。
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func (m *MemoryStore) SaveReport(_ context.Context, r Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[r.SessionID]; !ok || s.TenantID != r.TenantID {
		return ErrNotFound
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now()
	}
	m.reports[r.SessionID] = r
	return nil
}

func (m *MemoryStore) GetReport(_ context.Context, tenantID, sessionID string) (Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[sessionID]; !ok || s.TenantID != tenantID {
		return Report{}, ErrNotFound
	}
	r, ok := m.reports[sessionID]
	if !ok {
		return Report{}, ErrNotFound
	}
	return r, nil
}

func (m *MemoryStore) SaveConsent(_ context.Context, c Consent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[c.SessionID]; !ok || s.TenantID != c.TenantID {
		return ErrNotFound
	}
	if c.AgreedAt.IsZero() {
		c.AgreedAt = m.now()
	}
	for _, existing := range m.consents[c.SessionID] {
		if existing.Scope == c.Scope {
			// 同一范围重复授权: 保留最早那条, 时间的先后有法律意义。
			return nil
		}
	}
	m.consents[c.SessionID] = append(m.consents[c.SessionID], c)
	return nil
}

func (m *MemoryStore) ListConsents(_ context.Context, tenantID, sessionID string) ([]Consent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[sessionID]; !ok || s.TenantID != tenantID {
		return nil, ErrNotFound
	}
	list := m.consents[sessionID]
	out := make([]Consent, len(list))
	copy(out, list)
	return out, nil
}

func (m *MemoryStore) AppendAudit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	e.ID = m.nextID
	if e.CreatedAt.IsZero() {
		e.CreatedAt = m.now()
	}
	m.audit = append(m.audit, e)
	return nil
}

func (m *MemoryStore) ListAudit(_ context.Context, tenantID string, limit int) ([]AuditEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AuditEntry, 0, len(m.audit))
	for i := len(m.audit) - 1; i >= 0; i-- { // 最新的在前
		if m.audit[i].TenantID != tenantID {
			continue
		}
		out = append(out, m.audit[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ExportCandidate 导出某候选人全部数据(可携带权)。
func (m *MemoryStore) ExportCandidate(_ context.Context, tenantID, candidateRef string) (CandidateExport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bundle := CandidateExport{
		CandidateRef: candidateRef,
		TenantID:     tenantID,
		ExportedAt:   m.now(),
	}
	for _, s := range m.sessions {
		if s.TenantID != tenantID || s.CandidateRef != candidateRef {
			continue
		}
		bundle.Sessions = append(bundle.Sessions, s)
		bundle.Turns = append(bundle.Turns, m.turns[s.ID]...)
		if r, ok := m.reports[s.ID]; ok {
			bundle.Reports = append(bundle.Reports, r)
		}
		bundle.Consents = append(bundle.Consents, m.consents[s.ID]...)
	}
	if len(bundle.Sessions) == 0 {
		return CandidateExport{}, ErrNotFound
	}
	return bundle, nil
}

// EraseCandidate 删除某候选人的全部数据(删除权), 返回删除的会话数。
//
// 审计日志**不删除**: 它记录的是"系统发生过什么", 属于平台自身的合规证据,
// 且不包含候选人内容。真实系统里这一点需要在隐私政策里写明。
func (m *MemoryStore) EraseCandidate(_ context.Context, tenantID, candidateRef string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var erased int
	for id, s := range m.sessions {
		if s.TenantID != tenantID || s.CandidateRef != candidateRef {
			continue
		}
		delete(m.sessions, id)
		delete(m.turns, id)
		delete(m.reports, id)
		delete(m.consents, id)
		erased++
	}
	return erased, nil
}

func (m *MemoryStore) Close() error { return nil }

// Analytics 在内存实现里就是一次遍历 —— 与 MySQL 的聚合 SQL 语义一致。
func (m *MemoryStore) Analytics(_ context.Context, tenantID string) (Analytics, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := Analytics{
		ByRecommendation: map[string]int{},
		ByStatus:         map[string]int{},
		ByCompetency:     map[string]map[string]int{},
	}
	var scoreSum float64
	var scoredSessions int

	for id, s := range m.sessions {
		if s.TenantID != tenantID {
			continue
		}
		out.Sessions++
		out.ByStatus[string(s.Status)]++
		switch s.Status {
		case StatusFinished:
			out.Finished++
			out.ByRecommendation[s.Recommendation]++
		case StatusRunning:
			out.Running++
		}
		if r, ok := m.reports[id]; ok {
			scoreSum += float64(r.Score)
			scoredSessions++
		}
		for _, t := range m.turns[id] {
			out.TotalTurns++
			if t.Scored {
				out.ScoredTurns++
			}
			if t.DegradedFrom != "" {
				out.DegradedTurns++
			}
			if t.IsProbe {
				out.ProbeTurns++
			}
			if t.Competency == "" || t.Level == "" {
				continue
			}
			if out.ByCompetency[t.Competency] == nil {
				out.ByCompetency[t.Competency] = map[string]int{}
			}
			out.ByCompetency[t.Competency][t.Level]++
		}
	}
	if scoredSessions > 0 {
		out.AvgScore = scoreSum / float64(scoredSessions)
	}
	return out, nil
}

// CreateSessionWithConsents 在内存实现里模拟事务: 先校验再一次性提交。
func (m *MemoryStore) CreateSessionWithConsents(ctx context.Context, s Session, consents []Consent) error {
	m.mu.Lock()
	if _, ok := m.sessions[s.ID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("store: 会话 %s 已存在", s.ID)
	}
	m.mu.Unlock()

	if err := m.CreateSession(ctx, s); err != nil {
		return err
	}
	for _, c := range consents {
		if err := m.SaveConsent(ctx, c); err != nil {
			// 真实实现里这里会 rollback; 内存实现直接清理, 保证语义一致。
			m.mu.Lock()
			delete(m.sessions, s.ID)
			delete(m.consents, s.ID)
			m.mu.Unlock()
			return err
		}
	}
	return nil
}

// FinishSession 保存报告并把会话标记为结束。
func (m *MemoryStore) FinishSession(_ context.Context, s Session, r Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	old, ok := m.sessions[s.ID]
	if !ok || old.TenantID != s.TenantID {
		return ErrNotFound
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now()
	}
	s.CreatedAt = old.CreatedAt
	s.UpdatedAt = m.now()
	s.Status = StatusFinished
	m.reports[s.ID] = r
	m.sessions[s.ID] = s
	return nil
}
