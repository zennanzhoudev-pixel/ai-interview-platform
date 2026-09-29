package platform

import (
	"context"
	"log/slog"
	"os"
)

// LogConfig 配置日志输出。
type LogConfig struct {
	// Level 支持 debug / info / warn / error。
	Level string
	// Format 支持 text(本地可读) 与 json(采集友好)。生产默认 json。
	Format string
}

// NewLogger 构造结构化日志器。
//
// 用 log/slog 而不是 log.Printf 的原因很实际: 线上排查要靠字段检索
// (session_id、tenant_id、turn_id), 而不是对着文本 grep。面试系统一次
// 请求会跨越网关、ASR、Agent、TTS 多个环节, 没有关联字段就只能靠时间戳猜。
func NewLogger(cfg LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

type ctxKey string

const (
	ctxKeyRequestID ctxKey = "request_id"
	ctxKeyTenantID  ctxKey = "tenant_id"
	ctxKeySessionID ctxKey = "session_id"
	ctxKeyActor     ctxKey = "actor"
)

// WithRequestID 把请求 ID 放进 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID 取出请求 ID。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithTenant 把租户放进 context。
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, ctxKeyTenantID, tenant)
}

// Tenant 取出租户。
func Tenant(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTenantID).(string); ok {
		return v
	}
	return ""
}

// WithSession 把会话 ID 放进 context。
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ctxKeySessionID, sessionID)
}

// SessionID 取出会话 ID。
func SessionID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeySessionID).(string); ok {
		return v
	}
	return ""
}

// WithActor 记录操作者(审计用): API Key 所属主体或候选人标记。
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ctxKeyActor, actor)
}

// Actor 取出操作者。
func Actor(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyActor).(string); ok {
		return v
	}
	return "anonymous"
}

// AuditAttrs 返回审计日志的标准字段集合, 保证每条日志都带得上上下文。
func AuditAttrs(ctx context.Context) []any {
	return []any{
		slog.String("request_id", RequestID(ctx)),
		slog.String("tenant_id", Tenant(ctx)),
		slog.String("session_id", SessionID(ctx)),
		slog.String("actor", Actor(ctx)),
	}
}
