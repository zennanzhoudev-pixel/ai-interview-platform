package account

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
)

// MySQLStore 是账号域的生产实现。
//
// 账号必须落库, 这一点比其它模块更硬: 面试记录丢了只是"回看不到",
// 而账号丢了是"用户注册过却登不上, 还会被判为重复注册" —— 后者
// 会让人直接放弃这个系统。
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore 复用调用方已有的连接池。
//
// 不在这里 Open 新连接: 一个进程里为每个模块各开一个连接池, 会让
// 数据库连接数随模块数线性增长, 而连接数是数据库最先耗尽的资源。
func NewMySQLStore(db *sql.DB) *MySQLStore { return &MySQLStore{db: db} }

// Close 不关闭共享连接池 —— 它属于调用方。
func (m *MySQLStore) Close() error { return nil }

// nullable 把空字符串写成 NULL。
//
// 这不是洁癖: account_user 上 (tenant_id, email) 是唯一键, 而 MySQL
// 唯一索引把空字符串当成一个具体值 —— 如果不用 NULL, 第二个"只填了
// 手机号"的用户就会因为邮箱都是 ” 而插入失败, 报的却是"邮箱已注册"。
func nullable(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// CreateUser 创建账号。
func (m *MySQLStore) CreateUser(ctx context.Context, u User) error {
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.Status == "" {
		u.Status = "active"
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO account_user
			(user_id, tenant_id, name, email, phone, role, password_hash,
			 face_enrolled, status, created_at, updated_at, last_login_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.TenantID, u.Name, nullable(u.Email), nullable(u.Phone), string(u.Role),
		u.PasswordHash, u.FaceEnrolled, u.Status, u.CreatedAt.UTC(), now, nullableTime(u.LastLoginAt))
	if err != nil {
		var myErr *mysqldriver.MySQLError
		if errors.As(err, &myErr) && myErr.Number == 1062 {
			return ErrUserExists
		}
		return err
	}
	return nil
}

// UpdateUser 更新账号。
func (m *MySQLStore) UpdateUser(ctx context.Context, u User) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE account_user SET name=?, email=?, phone=?, role=?, password_hash=?,
			face_enrolled=?, status=?, updated_at=?, last_login_at=?
		WHERE user_id=? AND tenant_id=?`,
		u.Name, nullable(u.Email), nullable(u.Phone), string(u.Role), u.PasswordHash,
		u.FaceEnrolled, u.Status, time.Now().UTC(), nullableTime(u.LastLoginAt), u.ID, u.TenantID)
	if err != nil {
		var myErr *mysqldriver.MySQLError
		if errors.As(err, &myErr) && myErr.Number == 1062 {
			return ErrUserExists
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// RowsAffected 为 0 也可能是"字段值完全相同", 因此要确认记录是否存在。
		if _, err := m.GetUser(ctx, u.TenantID, u.ID); err != nil {
			return err
		}
	}
	return nil
}

// GetUser 读取账号。
func (m *MySQLStore) GetUser(ctx context.Context, tenantID, userID string) (User, error) {
	return scanUser(m.db.QueryRowContext(ctx, `
		SELECT user_id, tenant_id, name, COALESCE(email,''), COALESCE(phone,''), role, password_hash,
		       face_enrolled, status, created_at, updated_at, last_login_at
		FROM account_user WHERE user_id=? AND tenant_id=?`, userID, tenantID))
}

// FindUser 按邮箱或手机号查找。
func (m *MySQLStore) FindUser(ctx context.Context, tenantID, identifier string) (User, error) {
	email := NormalizeEmail(identifier)
	phone := ""
	if normalized, err := NormalizePhone(identifier); err == nil {
		phone = normalized
	}
	if email == "" && phone == "" {
		return User{}, ErrUserNotFound
	}
	row := m.db.QueryRowContext(ctx, `
		SELECT user_id, tenant_id, name, COALESCE(email,''), COALESCE(phone,''), role, password_hash,
		       face_enrolled, status, created_at, updated_at, last_login_at
		FROM account_user
		WHERE tenant_id=? AND (email = ? OR phone = ?)
		LIMIT 1`, tenantID, nullable(email), nullable(phone))
	return scanUser(row)
}

// ListUsers 列出账号。
func (m *MySQLStore) ListUsers(ctx context.Context, tenantID string, limit int) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT user_id, tenant_id, name, COALESCE(email,''), COALESCE(phone,''), role, password_hash,
		       face_enrolled, status, created_at, updated_at, last_login_at
		FROM account_user WHERE tenant_id=? ORDER BY created_at DESC, user_id DESC LIMIT ?`,
		tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser 删除账号、人脸模板与全部会话。
func (m *MySQLStore) DeleteUser(ctx context.Context, tenantID, userID string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM account_user WHERE user_id=? AND tenant_id=?`, userID, tenantID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrUserNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_face WHERE user_id=? AND tenant_id=?`, userID, tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_login WHERE user_id=? AND tenant_id=?`, userID, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}

// PutFace 保存人脸模板(向量以 float32 小端连续存放)。
func (m *MySQLStore) PutFace(ctx context.Context, p FaceProfile) error {
	now := time.Now().UTC()
	if p.EnrolledAt.IsZero() {
		p.EnrolledAt = now
	}
	raw := encodeFloat32(p.Template)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO account_face
			(tenant_id, user_id, template, dim, matcher, assurance, quality, frames, enrolled_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			template=VALUES(template), dim=VALUES(dim), matcher=VALUES(matcher),
			assurance=VALUES(assurance), quality=VALUES(quality), frames=VALUES(frames),
			updated_at=VALUES(updated_at)`,
		p.TenantID, p.UserID, raw, len(p.Template), p.Matcher, p.Assurance,
		p.Quality, p.Frames, p.EnrolledAt.UTC(), now)
	return err
}

// GetFace 读取人脸模板。
func (m *MySQLStore) GetFace(ctx context.Context, tenantID, userID string) (FaceProfile, error) {
	var (
		p   FaceProfile
		raw []byte
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT tenant_id, user_id, template, dim, matcher, assurance, quality, frames, enrolled_at, updated_at
		FROM account_face WHERE tenant_id=? AND user_id=?`, tenantID, userID).
		Scan(&p.TenantID, &p.UserID, &raw, &p.Dim, &p.Matcher, &p.Assurance,
			&p.Quality, &p.Frames, &p.EnrolledAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FaceProfile{}, ErrFaceNotEnrolled
	}
	if err != nil {
		return FaceProfile{}, err
	}
	p.Template = decodeFloat32(raw)
	return p, nil
}

// DeleteFace 删除人脸模板。
func (m *MySQLStore) DeleteFace(ctx context.Context, tenantID, userID string) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM account_face WHERE tenant_id=? AND user_id=?`, tenantID, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrFaceNotEnrolled
	}
	return nil
}

// CreateLoginSession 保存登录会话。
func (m *MySQLStore) CreateLoginSession(ctx context.Context, s LoginSession) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO account_login
			(token_hash, tenant_id, user_id, role, ip, user_agent, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		s.TokenHash, s.TenantID, s.UserID, string(s.Role), s.IP, s.UserAgent,
		s.CreatedAt.UTC(), s.ExpiresAt.UTC())
	return err
}

// GetLoginSession 读取登录会话。
func (m *MySQLStore) GetLoginSession(ctx context.Context, tokenHash string) (LoginSession, error) {
	var (
		s    LoginSession
		role string
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT token_hash, tenant_id, user_id, role, ip, user_agent, created_at, expires_at
		FROM account_login WHERE token_hash=?`, tokenHash).
		Scan(&s.TokenHash, &s.TenantID, &s.UserID, &role, &s.IP, &s.UserAgent, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginSession{}, ErrSessionExpired
	}
	if err != nil {
		return LoginSession{}, err
	}
	s.Role = auth.Role(role)
	if s.Expired(time.Now().UTC()) {
		return LoginSession{}, ErrSessionExpired
	}
	return s, nil
}

// DeleteLoginSession 删除会话。
func (m *MySQLStore) DeleteLoginSession(ctx context.Context, tokenHash string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM account_login WHERE token_hash=?`, tokenHash)
	return err
}

// DeleteUserSessions 删除某账号的全部会话。
func (m *MySQLStore) DeleteUserSessions(ctx context.Context, tenantID, userID string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM account_login WHERE tenant_id=? AND user_id=?`, tenantID, userID)
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanUser(row rowScanner) (User, error) {
	var (
		u         User
		role      string
		lastLogin sql.NullTime
	)
	err := row.Scan(&u.ID, &u.TenantID, &u.Name, &u.Email, &u.Phone, &role, &u.PasswordHash,
		&u.FaceEnrolled, &u.Status, &u.CreatedAt, &u.UpdatedAt, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Role = auth.Role(role)
	if lastLogin.Valid {
		u.LastLoginAt = lastLogin.Time
	}
	return u, nil
}

func encodeFloat32(vec []float32) []byte {
	out := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

func decodeFloat32(raw []byte) []float32 {
	n := len(raw) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
