package account

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

var (
	// ErrCodeInvalid 表示验证码不正确或已过期。
	ErrCodeInvalid = errors.New("account: 验证码不正确或已过期")
	// ErrTooManyCodeAttempts 表示同一验证码尝试次数过多。
	ErrTooManyCodeAttempts = errors.New("account: 验证码尝试次数过多, 请重新获取")
	// ErrNotifierUnavailable 表示没有可用的通知通道。
	ErrNotifierUnavailable = errors.New("account: 通知通道未配置")
)

const (
	codeTTL         = 10 * time.Minute
	codeMaxAttempts = 5
)

type codeEntry struct {
	code     string
	expires  time.Time
	attempts int
}

// codeStore 是进程内的验证码存储。单实例够用; 多副本部署时应换成 Redis。
type codeStore struct {
	mu sync.Mutex
	m  map[string]codeEntry
}

func newCodeStore() *codeStore { return &codeStore{m: make(map[string]codeEntry)} }

func (c *codeStore) put(key, code string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = codeEntry{code: code, expires: now.Add(codeTTL)}
}

func (c *codeStore) verify(key, code string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return ErrCodeInvalid
	}
	if now.After(e.expires) {
		delete(c.m, key)
		return ErrCodeInvalid
	}
	e.attempts++
	if e.attempts > codeMaxAttempts {
		delete(c.m, key)
		return ErrTooManyCodeAttempts
	}
	if e.code != code {
		c.m[key] = e
		return ErrCodeInvalid
	}
	delete(c.m, key)
	return nil
}

func (c *codeStore) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func codeKey(kind, target string) string { return kind + ":" + target }

func normalizeTarget(kind, target string) string {
	target = strings.TrimSpace(target)
	if kind == "email" {
		return strings.ToLower(target)
	}
	if kind == "phone" {
		if p, err := NormalizePhone(target); err == nil {
			return p
		}
	}
	return target
}

// RequestVerifyCode 生成验证码并交给通知器发送。
func (s *Service) RequestVerifyCode(ctx context.Context, kind, target string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "email" && kind != "phone" {
		return fmt.Errorf("%w: kind 只能是 email 或 phone", ErrInvalidInput)
	}
	target = normalizeTarget(kind, target)
	if target == "" {
		return fmt.Errorf("%w: 目标不能为空", ErrInvalidInput)
	}
	code, err := randomCode()
	if err != nil {
		return err
	}
	s.codes.put(codeKey(kind, target), code, s.now())
	if s.notifier == nil {
		return ErrNotifierUnavailable
	}
	return s.notifier.Send(ctx, kind, target, code)
}

// ConfirmVerifyCode 校验验证码。
func (s *Service) ConfirmVerifyCode(_ context.Context, kind, target, code string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	target = normalizeTarget(kind, target)
	return s.codes.verify(codeKey(kind, target), strings.TrimSpace(code), s.now())
}

// RequestPasswordReset 发送找回密码验证码。
//
// 账号不存在时也返回 nil: 否则这个接口会变成"某邮箱是否注册"的枚举器。
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	target := normalizeTarget("email", email)
	if _, err := s.store.FindUser(ctx, s.tenantID, target); err != nil {
		return nil
	}
	code, err := randomCode()
	if err != nil {
		return err
	}
	s.codes.put(codeKey("reset", target), code, s.now())
	if s.notifier == nil {
		return ErrNotifierUnavailable
	}
	return s.notifier.Send(ctx, "reset", target, code)
}

// ResetPassword 用验证码重置密码, 并使该账号的所有旧会话失效。
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) error {
	target := normalizeTarget("email", email)
	if err := s.codes.verify(codeKey("reset", target), strings.TrimSpace(code), s.now()); err != nil {
		return err
	}
	u, err := s.store.FindUser(ctx, s.tenantID, target)
	if err != nil {
		return ErrInvalidCredential
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	u.UpdatedAt = s.now()
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return err
	}
	_ = s.store.DeleteUserSessions(ctx, u.TenantID, u.ID)
	return nil
}
