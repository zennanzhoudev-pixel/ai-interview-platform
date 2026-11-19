package account

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
)

// Notifier 负责把验证码送到用户手里(邮件/短信)。
//
// 抽成接口而不是直接写死 SMTP: 本地开发用"打日志"实现就能跑通全流程,
// 生产换成真实邮件/短信只需要换一个实现, 业务代码一行不改。
type Notifier interface {
	Send(ctx context.Context, kind, target, code string) error
}

// LogNotifier 是开发模式: 只把验证码写进日志, 不真实发送。
type LogNotifier struct{ Logger *slog.Logger }

// Send 记录验证码。
func (n LogNotifier) Send(_ context.Context, kind, target, code string) error {
	logger := n.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("验证码(开发模式, 未真实发送)", "kind", kind, "target", target, "code", code)
	return nil
}

// SMTPNotifier 通过 SMTP 发送邮件验证码。
type SMTPNotifier struct {
	Host string
	Port string
	User string
	Pass string
	From string
}

// Send 发送邮件验证码。
func (n SMTPNotifier) Send(_ context.Context, kind, target, code string) error {
	if kind != "email" {
		return fmt.Errorf("%w: SMTP 通道只支持邮箱验证码", ErrNotifierUnavailable)
	}
	if n.Host == "" || n.From == "" {
		return fmt.Errorf("%w: SMTP 未配置(SMTP_HOST/SMTP_FROM)", ErrNotifierUnavailable)
	}
	addr := n.Host + ":587"
	if n.Port != "" {
		addr = n.Host + ":" + n.Port
	}
	auth := smtp.PlainAuth("", n.User, n.Pass, n.Host)
	msg := []byte("To: " + target + "\r\n" +
		"Subject: AI Interview OS 验证码\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		"你的验证码是 " + code + ", 10 分钟内有效。\r\n")
	return smtp.SendMail(addr, auth, n.From, []string{target}, msg)
}
