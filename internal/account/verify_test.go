package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeNotifier struct {
	kind, target, code string
}

func (f *fakeNotifier) Send(_ context.Context, kind, target, code string) error {
	f.kind, f.target, f.code = kind, target, code
	return nil
}

func TestVerifyCodeFlow(t *testing.T) {
	n := &fakeNotifier{}
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Notifier: n})
	ctx := context.Background()

	if err := s.RequestVerifyCode(ctx, "email", "Test@Example.com"); err != nil {
		t.Fatalf("请求验证码失败: %v", err)
	}
	if n.kind != "email" || n.target != "test@example.com" || n.code == "" {
		t.Fatalf("通知器参数错误: %+v", n)
	}
	if err := s.ConfirmVerifyCode(ctx, "email", "test@example.com", "000000"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("错误验证码应 ErrCodeInvalid, 实际 %v", err)
	}
	if err := s.ConfirmVerifyCode(ctx, "email", "test@example.com", n.code); err != nil {
		t.Fatalf("正确验证码应通过: %v", err)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	n := &fakeNotifier{}
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Notifier: n})
	ctx := context.Background()

	if _, err := s.Register(ctx, RegisterInput{Name: "测试", Email: "a@example.com", Password: "OldPass123"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := s.RequestPasswordReset(ctx, "a@example.com"); err != nil {
		t.Fatalf("请求重置失败: %v", err)
	}
	if n.kind != "reset" || n.code == "" {
		t.Fatalf("重置验证码未发送: %+v", n)
	}
	if err := s.ResetPassword(ctx, "a@example.com", n.code, "NewPass123"); err != nil {
		t.Fatalf("重置密码失败: %v", err)
	}
	if _, _, err := s.Login(ctx, LoginInput{Identifier: "a@example.com", Password: "OldPass123"}); err == nil {
		t.Fatal("旧密码应失效")
	}
	if _, _, err := s.Login(ctx, LoginInput{Identifier: "a@example.com", Password: "NewPass123"}); err != nil {
		t.Fatalf("新密码应可登录: %v", err)
	}
}

func TestPasswordResetUnknownEmailIsSilent(t *testing.T) {
	n := &fakeNotifier{}
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Notifier: n})
	if err := s.RequestPasswordReset(context.Background(), "nobody@example.com"); err != nil {
		t.Fatalf("未知邮箱应静默成功: %v", err)
	}
	if n.code != "" {
		t.Fatal("未知邮箱不应发送验证码")
	}
}

func TestCodeExpires(t *testing.T) {
	n := &fakeNotifier{}
	now := time.Now()
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Notifier: n, Now: func() time.Time { return now }})
	ctx := context.Background()
	if err := s.RequestVerifyCode(ctx, "email", "x@example.com"); err != nil {
		t.Fatalf("请求验证码失败: %v", err)
	}
	now = now.Add(codeTTL + time.Second)
	if err := s.ConfirmVerifyCode(ctx, "email", "x@example.com", n.code); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("过期验证码应 ErrCodeInvalid, 实际 %v", err)
	}
}
