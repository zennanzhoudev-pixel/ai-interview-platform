package store

import (
	"context"
	"sort"
	"time"
)

// 内存实现的业务域方法。
//
// 与 memory.go 一样, 这里的价值是"零依赖可跑 + 契约测试复用":
// 它必须和 MySQL 实现语义一致, 包括租户隔离与排序稳定性。
// 管理界面依赖排序稳定(看板卡片顺序), 因此不能靠 map 迭代顺序返回。

/* ---------------- 职位 ---------------- */

// CreateJob 创建职位。
func (m *MemoryStore) CreateJob(_ context.Context, j Job) error {
	if j.ID == "" || j.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.jobs[j.ID]; ok {
		if existing.TenantID == j.TenantID {
			return ErrConflict
		}
	}
	now := m.now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	j.Rounds = NormalizeRounds(j.Rounds)
	m.jobs[j.ID] = j
	return nil
}

// UpdateJob 更新职位。
func (m *MemoryStore) UpdateJob(_ context.Context, j Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.jobs[j.ID]
	if !ok || old.TenantID != j.TenantID {
		return ErrNotFound
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = old.CreatedAt
	}
	j.UpdatedAt = m.now()
	j.Rounds = NormalizeRounds(j.Rounds)
	m.jobs[j.ID] = j
	return nil
}

// GetJob 按租户读取职位。
func (m *MemoryStore) GetJob(_ context.Context, tenantID, jobID string) (Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[jobID]
	if !ok || j.TenantID != tenantID {
		return Job{}, ErrNotFound
	}
	return j, nil
}

// ListJobs 列出职位的按创建时间倒序切片。
func (m *MemoryStore) ListJobs(_ context.Context, tenantID string) ([]Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.TenantID == tenantID {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

/* ---------------- 候选人 ---------------- */

// CreateCandidate 创建候选人档案。
func (m *MemoryStore) CreateCandidate(_ context.Context, c Candidate) error {
	if c.Ref == "" || c.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if old, ok := m.candidates[c.Ref]; ok && old.TenantID == c.TenantID {
		// 幂等: ATS 反复推送同一份简历是常态, 不该报错也不该丢新数据。
		if c.CreatedAt.IsZero() {
			c.CreatedAt = old.CreatedAt
		}
		c.UpdatedAt = now
		m.candidates[c.Ref] = c
		return nil
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	m.candidates[c.Ref] = c
	return nil
}

// UpdateCandidate 更新候选人档案。
func (m *MemoryStore) UpdateCandidate(_ context.Context, c Candidate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.candidates[c.Ref]
	if !ok || old.TenantID != c.TenantID {
		return ErrNotFound
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = old.CreatedAt
	}
	c.UpdatedAt = m.now()
	m.candidates[c.Ref] = c
	return nil
}

// GetCandidate 按租户读取候选人。
func (m *MemoryStore) GetCandidate(_ context.Context, tenantID, ref string) (Candidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.candidates[ref]
	if !ok || c.TenantID != tenantID {
		return Candidate{}, ErrNotFound
	}
	return c, nil
}

// ListCandidates 列出候选人。
func (m *MemoryStore) ListCandidates(_ context.Context, tenantID string, limit int) ([]Candidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Candidate, 0, len(m.candidates))
	for _, c := range m.candidates {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Ref > out[j].Ref
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

/* ---------------- 投递 ---------------- */

// CreateApplication 创建投递。
func (m *MemoryStore) CreateApplication(_ context.Context, a Application) error {
	if a.ID == "" || a.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if old, ok := m.applications[a.ID]; ok && old.TenantID == a.TenantID {
		if a.CreatedAt.IsZero() {
			a.CreatedAt = old.CreatedAt
		}
		a.UpdatedAt = now
		m.applications[a.ID] = a
		return nil
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	m.applications[a.ID] = a
	return nil
}

// UpdateApplication 更新投递。
func (m *MemoryStore) UpdateApplication(_ context.Context, a Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.applications[a.ID]
	if !ok || old.TenantID != a.TenantID {
		return ErrNotFound
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = old.CreatedAt
	}
	a.UpdatedAt = m.now()
	m.applications[a.ID] = a
	return nil
}

// GetApplication 按租户读取投递。
func (m *MemoryStore) GetApplication(_ context.Context, tenantID, id string) (Application, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.applications[id]
	if !ok || a.TenantID != tenantID {
		return Application{}, ErrNotFound
	}
	return a, nil
}

// ListApplications 列出投递, jobID 非空时按职位过滤。
func (m *MemoryStore) ListApplications(_ context.Context, tenantID, jobID string) ([]Application, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Application, 0, len(m.applications))
	for _, a := range m.applications {
		if a.TenantID != tenantID {
			continue
		}
		if jobID != "" && a.JobID != jobID {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// ListApplicationsByCandidate 列出某个候选人的全部投递。
func (m *MemoryStore) ListApplicationsByCandidate(_ context.Context, tenantID, candidateRef string) ([]Application, error) {
	all, err := m.ListApplications(context.Background(), tenantID, "")
	if err != nil {
		return nil, err
	}
	out := make([]Application, 0, len(all))
	for _, a := range all {
		if a.CandidateRef == candidateRef {
			out = append(out, a)
		}
	}
	return out, nil
}

/* ---------------- 题库 ---------------- */

// CreateQuestion 创建题目。
func (m *MemoryStore) CreateQuestion(_ context.Context, q QuestionItem) error {
	if q.ID == "" || q.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if old, ok := m.questions[q.ID]; ok && old.TenantID == q.TenantID {
		// 已存在: 版本号自增, 让"这题改过几次"可追溯。
		if q.Version <= old.Version {
			q.Version = old.Version + 1
		}
		if q.CreatedAt.IsZero() {
			q.CreatedAt = old.CreatedAt
		}
		q.UpdatedAt = now
		m.questions[q.ID] = q
		return nil
	}
	if q.Version == 0 {
		q.Version = 1
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = now
	}
	q.UpdatedAt = now
	m.questions[q.ID] = q
	return nil
}

// UpdateQuestion 更新题目(带乐观版本自增)。
func (m *MemoryStore) UpdateQuestion(_ context.Context, q QuestionItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.questions[q.ID]
	if !ok || old.TenantID != q.TenantID {
		return ErrNotFound
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = old.CreatedAt
	}
	q.Version = old.Version + 1
	q.UpdatedAt = m.now()
	m.questions[q.ID] = q
	return nil
}

// DeleteQuestion 删除题目。
func (m *MemoryStore) DeleteQuestion(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.questions[id]
	if !ok || q.TenantID != tenantID {
		return ErrNotFound
	}
	delete(m.questions, id)
	return nil
}

// GetQuestion 读取题目。
func (m *MemoryStore) GetQuestion(_ context.Context, tenantID, id string) (QuestionItem, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q, ok := m.questions[id]
	if !ok || q.TenantID != tenantID {
		return QuestionItem{}, ErrNotFound
	}
	return q, nil
}

// ListQuestions 按阶段与创建时间列出题目。
func (m *MemoryStore) ListQuestions(_ context.Context, tenantID string) ([]QuestionItem, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]QuestionItem, 0, len(m.questions))
	for _, q := range m.questions {
		if q.TenantID == tenantID {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage == out[j].Stage {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Stage < out[j].Stage
	})
	return out, nil
}

/* ---------------- 面试安排 ---------------- */

// CreateSchedule 创建面试安排。
func (m *MemoryStore) CreateSchedule(_ context.Context, s Schedule) error {
	if s.ID == "" || s.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if old, ok := m.schedules[s.ID]; ok && old.TenantID == s.TenantID {
		if s.CreatedAt.IsZero() {
			s.CreatedAt = old.CreatedAt
		}
		s.UpdatedAt = now
		m.schedules[s.ID] = s
		return nil
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	m.schedules[s.ID] = s
	return nil
}

// UpdateSchedule 更新面试安排。
func (m *MemoryStore) UpdateSchedule(_ context.Context, s Schedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.schedules[s.ID]
	if !ok || old.TenantID != s.TenantID {
		return ErrNotFound
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = old.CreatedAt
	}
	s.UpdatedAt = m.now()
	m.schedules[s.ID] = s
	return nil
}

// GetSchedule 读取面试安排。
func (m *MemoryStore) GetSchedule(_ context.Context, tenantID, id string) (Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.schedules[id]
	if !ok || s.TenantID != tenantID {
		return Schedule{}, ErrNotFound
	}
	return s, nil
}

// ListSchedules 列出时间窗口内的面试安排(闭区间为空时不过滤)。
func (m *MemoryStore) ListSchedules(_ context.Context, tenantID string, from, to time.Time) ([]Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Schedule, 0, len(m.schedules))
	for _, s := range m.schedules {
		if s.TenantID != tenantID {
			continue
		}
		if !from.IsZero() && s.ScheduledAt.Before(from) {
			continue
		}
		if !to.IsZero() && s.ScheduledAt.After(to) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScheduledAt.Equal(out[j].ScheduledAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ScheduledAt.Before(out[j].ScheduledAt)
	})
	return out, nil
}

/* ---------------- 录制件 ---------------- */

// CreateRecording 创建录制件元数据。
func (m *MemoryStore) CreateRecording(_ context.Context, r Recording) error {
	if r.ID == "" || r.TenantID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if old, ok := m.recordings[r.ID]; ok && old.TenantID == r.TenantID {
		if r.CreatedAt.IsZero() {
			r.CreatedAt = old.CreatedAt
		}
		r.UpdatedAt = now
		m.recordings[r.ID] = r
		return nil
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.Status == "" {
		r.Status = "uploading"
	}
	r.UpdatedAt = now
	m.recordings[r.ID] = r
	return nil
}

// UpdateRecording 更新录制件元数据。
func (m *MemoryStore) UpdateRecording(_ context.Context, r Recording) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.recordings[r.ID]
	if !ok || old.TenantID != r.TenantID {
		return ErrNotFound
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = old.CreatedAt
	}
	r.UpdatedAt = m.now()
	m.recordings[r.ID] = r
	return nil
}

// GetRecording 读取录制件。
func (m *MemoryStore) GetRecording(_ context.Context, tenantID, id string) (Recording, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.recordings[id]
	if !ok || r.TenantID != tenantID {
		return Recording{}, ErrNotFound
	}
	return r, nil
}

// GetRecordingBySession 按会话与类型读取录制件。
func (m *MemoryStore) GetRecordingBySession(_ context.Context, tenantID, sessionID, kind string) (Recording, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.recordings {
		if r.TenantID == tenantID && r.SessionID == sessionID && r.Kind == kind {
			return r, nil
		}
	}
	return Recording{}, ErrNotFound
}

// ListRecordings 列出某场面试的全部录制件。
func (m *MemoryStore) ListRecordings(_ context.Context, tenantID, sessionID string) ([]Recording, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Recording, 0, 3)
	for _, r := range m.recordings {
		if r.TenantID == tenantID && r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteRecording 删除录制件元数据。
func (m *MemoryStore) DeleteRecording(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recordings[id]
	if !ok || r.TenantID != tenantID {
		return ErrNotFound
	}
	delete(m.recordings, id)
	return nil
}

// PurgeExpiredRecordings 返回保留期已过的录制件。
//
// 刻意"只返回不删除": 存储层不知道二进制在哪(可能在对象存储),
// 由调用方删完内容再回来删元数据, 避免出现"元数据没了、文件还在"。
func (m *MemoryStore) PurgeExpiredRecordings(_ context.Context, now time.Time) ([]Recording, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Recording
	for _, r := range m.recordings {
		if !r.DeleteAfter.IsZero() && r.DeleteAfter.Before(now) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
