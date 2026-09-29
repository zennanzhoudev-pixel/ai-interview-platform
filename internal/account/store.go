package account

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
)

// LoginSession 是一次网页登录会话。
//
// 服务端存会话(而不是把身份塞进签名的 Cookie): 这样"退出登录"
// 与"管理员踢掉某个人的会话"才能真正生效 —— 无状态令牌做不到撤销,
// 只能等它过期。面试系统里有"离职/调岗后立刻停权"的诉求, 必须可撤销。
type LoginSession struct {
	// TokenHash 是令牌的 sha256(存储形态), 明文只在签发时返回一次。
	TokenHash string
	TenantID  string
	UserID    string
	Role      auth.Role
	// IP 与 UserAgent 都经过脱敏, 只用于"这次登录来自哪里"的审计。
	IP        string
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired 判断会话是否过期。
func (s LoginSession) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// Store 是账号域的持久化接口。
//
// 与 store.SessionStore 一样, **所有读取都必须带租户** ——
// 账号是跨租户最容易出错的一张表: 同一个手机号在两家公司都可能是候选人,
// 因此唯一键必须带上 tenant_id, 查询也必须带。
type Store interface {
	CreateUser(ctx context.Context, u User) error
	UpdateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, tenantID, userID string) (User, error)
	// FindUser 按邮箱或手机号查找(都已规范化)。
	FindUser(ctx context.Context, tenantID, identifier string) (User, error)
	ListUsers(ctx context.Context, tenantID string, limit int) ([]User, error)
	// DeleteUser 删除账号及其人脸模板与会话(数据主体权利)。
	DeleteUser(ctx context.Context, tenantID, userID string) error

	PutFace(ctx context.Context, p FaceProfile) error
	GetFace(ctx context.Context, tenantID, userID string) (FaceProfile, error)
	DeleteFace(ctx context.Context, tenantID, userID string) error

	CreateLoginSession(ctx context.Context, s LoginSession) error
	GetLoginSession(ctx context.Context, tokenHash string) (LoginSession, error)
	DeleteLoginSession(ctx context.Context, tokenHash string) error
	// DeleteUserSessions 使某个账号的所有登录会话失效(改密码/停用账号时用)。
	DeleteUserSessions(ctx context.Context, tenantID, userID string) error

	Close() error
}

// MemoryStore 是零依赖实现, 用于本地演示与单元测试。
//
// 它与 MySQL 实现共用同一套行为契约测试: 账号这种"注册完就登不上"
// 的错误, 只有在两套实现语义一致时才不会在换后端时出现。
type MemoryStore struct {
	mu       sync.RWMutex
	users    map[string]User
	faces    map[string]FaceProfile
	sessions map[string]LoginSession
	now      func() time.Time
}

// NewMemoryStore 构造内存账号存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:    make(map[string]User),
		faces:    make(map[string]FaceProfile),
		sessions: make(map[string]LoginSession),
		now:      time.Now,
	}
}

func userKey(tenantID, userID string) string { return tenantID + "|" + userID }

// CreateUser 创建账号, 并保证邮箱/手机号在租户内唯一。
func (m *MemoryStore) CreateUser(_ context.Context, u User) error {
	if u.ID == "" || u.TenantID == "" {
		return ErrInvalidInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.users[userKey(u.TenantID, u.ID)]; exists {
		return ErrUserExists
	}
	for _, existing := range m.users {
		if existing.TenantID != u.TenantID {
			continue
		}
		if u.Email != "" && existing.Email == u.Email {
			return ErrUserExists
		}
		if u.Phone != "" && existing.Phone == u.Phone {
			return ErrUserExists
		}
	}
	now := m.now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if u.Status == "" {
		u.Status = "active"
	}
	m.users[userKey(u.TenantID, u.ID)] = u
	return nil
}

// UpdateUser 更新账号。
func (m *MemoryStore) UpdateUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.users[userKey(u.TenantID, u.ID)]
	if !ok {
		return ErrUserNotFound
	}
	for _, existing := range m.users {
		if existing.TenantID != u.TenantID || existing.ID == u.ID {
			continue
		}
		if u.Email != "" && existing.Email == u.Email {
			return ErrUserExists
		}
		if u.Phone != "" && existing.Phone == u.Phone {
			return ErrUserExists
		}
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = old.CreatedAt
	}
	u.UpdatedAt = m.now()
	m.users[userKey(u.TenantID, u.ID)] = u
	return nil
}

// GetUser 按租户读取账号。
func (m *MemoryStore) GetUser(_ context.Context, tenantID, userID string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[userKey(tenantID, userID)]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return u, nil
}

// FindUser 按邮箱或手机号查找账号。
func (m *MemoryStore) FindUser(_ context.Context, tenantID, identifier string) (User, error) {
	email := NormalizeEmail(identifier)
	phone := ""
	if normalized, err := NormalizePhone(identifier); err == nil {
		phone = normalized
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.TenantID != tenantID {
			continue
		}
		if email != "" && u.Email != "" && u.Email == email {
			return u, nil
		}
		if phone != "" && u.Phone != "" && u.Phone == phone {
			return u, nil
		}
	}
	return User{}, ErrUserNotFound
}

// ListUsers 列出账号(脱敏由调用方决定)。
func (m *MemoryStore) ListUsers(_ context.Context, tenantID string, limit int) ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]User, 0, len(m.users))
	for _, u := range m.users {
		if u.TenantID == tenantID {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeleteUser 删除账号、人脸模板与全部会话。
func (m *MemoryStore) DeleteUser(_ context.Context, tenantID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userKey(tenantID, userID)
	if _, ok := m.users[key]; !ok {
		return ErrUserNotFound
	}
	delete(m.users, key)
	delete(m.faces, key)
	for tokenHash, session := range m.sessions {
		if session.TenantID == tenantID && session.UserID == userID {
			delete(m.sessions, tokenHash)
		}
	}
	return nil
}

// PutFace 保存人脸模板。
func (m *MemoryStore) PutFace(_ context.Context, p FaceProfile) error {
	if p.TenantID == "" || p.UserID == "" || len(p.Template) == 0 {
		return ErrInvalidInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userKey(p.TenantID, p.UserID)
	if old, ok := m.faces[key]; ok && p.EnrolledAt.IsZero() {
		p.EnrolledAt = old.EnrolledAt
	}
	if p.EnrolledAt.IsZero() {
		p.EnrolledAt = m.now()
	}
	p.UpdatedAt = m.now()
	p.Dim = len(p.Template)
	m.faces[key] = p
	return nil
}

// GetFace 读取人脸模板。
func (m *MemoryStore) GetFace(_ context.Context, tenantID, userID string) (FaceProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.faces[userKey(tenantID, userID)]
	if !ok {
		return FaceProfile{}, ErrFaceNotEnrolled
	}
	return p, nil
}

// DeleteFace 删除人脸模板。
func (m *MemoryStore) DeleteFace(_ context.Context, tenantID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userKey(tenantID, userID)
	if _, ok := m.faces[key]; !ok {
		return ErrFaceNotEnrolled
	}
	delete(m.faces, key)
	return nil
}

// CreateLoginSession 保存登录会话。
func (m *MemoryStore) CreateLoginSession(_ context.Context, s LoginSession) error {
	if s.TokenHash == "" || s.UserID == "" {
		return ErrInvalidInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = m.now()
	}
	m.sessions[s.TokenHash] = s
	return nil
}

// GetLoginSession 读取登录会话。
func (m *MemoryStore) GetLoginSession(_ context.Context, tokenHash string) (LoginSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[tokenHash]
	if !ok {
		return LoginSession{}, ErrSessionExpired
	}
	if s.Expired(m.now()) {
		return LoginSession{}, ErrSessionExpired
	}
	return s, nil
}

// DeleteLoginSession 删除会话(退出登录)。
func (m *MemoryStore) DeleteLoginSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

// DeleteUserSessions 删除某账号的全部会话。
func (m *MemoryStore) DeleteUserSessions(_ context.Context, tenantID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tokenHash, s := range m.sessions {
		if s.TenantID == tenantID && s.UserID == userID {
			delete(m.sessions, tokenHash)
		}
	}
	return nil
}

// Close 实现 Store 接口。
func (m *MemoryStore) Close() error { return nil }

// identifierKind 判断输入是邮箱还是手机号, 用于给出可理解的错误信息。
func identifierKind(identifier string) string {
	if strings.Contains(identifier, "@") {
		return "邮箱"
	}
	return "手机号"
}
