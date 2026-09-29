package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
)

// withRequestID 为每个请求生成请求 ID 并写入日志上下文。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(platform.WithRequestID(r.Context(), id)))
	})
}

// withRecovery 兜底处理器里的 panic。
//
// net/http 自己会 recover 处理器 panic, 但那是"这一条连接断掉"的语义,
// 且不会打点。这里显式兜底是为了: 让请求返回 500 而不是直接断连,
// 并把 panic 计入指标 —— 否则线上只能看到"偶发连接重置"。
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if platform.Recover(r.Context(), s.logger, "http.handler", func(name string) {
				s.metrics.Panics.WithLabelValues(name).Inc()
			}) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"服务内部错误"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录响应码, 供访问日志与指标使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack 让 WebSocket 升级仍然可用。
func (r *statusRecorder) Hijack() (net.Conn, *bufio_ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("api: 底层不支持 hijack")
	}
	return h.Hijack()
}

// withAccessLog 记录访问日志与 HTTP 指标。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := routeLabel(r)
		elapsed := time.Since(start)
		s.metrics.HTTPRequests.WithLabelValues(route, r.Method, http.StatusText(rec.status)).Inc()
		s.metrics.HTTPDuration.WithLabelValues(route).Observe(elapsed.Seconds())

		// 静态资源不记访问日志, 否则日志会被 /app.js 之类淹没。
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ws/") {
			return
		}
		s.logger.InfoContext(r.Context(), "http request",
			append(platform.AuditAttrs(r.Context()),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("elapsed", elapsed),
			)...)
	})
}

// withPermission 校验 API Key 与权限点, 并把租户注入 context。
//
// 租户只能来自凭据, 绝不能来自请求参数 —— 否则"多租户隔离"就只是一句
// 写在文档里的话。
//
// 它同时接受两种身份, 因为它们代表两种调用方:
//   - API Key: 机器(客户的 ATS 集成), 一次配置长期使用;
//   - 账号会话: 人(招聘同学/面试官), 有登录与退出, 可被单独停权。
//
// 只有一种身份是不够的: 只认 API Key, 人就无法登录; 只认账号,
// 系统集成就得让某个人的账号去跑后台任务 —— 那既无法追责也不可撤销。
func (s *Server) withPermission(perm auth.Permission, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.RequireAuth {
			// 本地演示模式: 未开启鉴权时放行。生产必须关掉这个开关。
			//
			// 但即便在演示模式, 只要浏览器带着登录会话, 就按该账号的租户与
			// 角色走 —— 否则"用账号 A 登录后看到的却是默认租户的数据",
			// 会让人误以为多租户隔离已经生效。
			if user, err := s.currentAccount(r); err == nil {
				ctx := platform.WithActor(platform.WithTenant(r.Context(), user.TenantID), "user:"+user.ID)
				next(w, r.WithContext(ctx))
				return
			}
			ctx := platform.WithActor(platform.WithTenant(r.Context(), s.cfg.TenantID), "demo")
			next(w, r.WithContext(ctx))
			return
		}

		// 1) 优先按机器身份(API Key)处理: 集成方调用时不应依赖浏览器 Cookie。
		if raw := bearerToken(r); raw != "" {
			if s.cfg.Keys == nil {
				s.rejectAuth(w, r, perm, "invalid", "")
				return
			}
			ctx, cancel := s.storeCtx(r.Context())
			principal, err := s.cfg.Keys.Resolve(ctx, raw)
			cancel()
			if err != nil {
				s.rejectAuth(w, r, perm, "invalid", "")
				return
			}
			if !principal.Can(perm) {
				s.rejectAuth(w, r, perm, "forbidden", string(principal.Role))
				return
			}
			ctx = platform.WithActor(platform.WithTenant(r.Context(), principal.TenantID), principal.String())
			next(w, r.WithContext(ctx))
			return
		}

		// 2) 再按人的身份(登录会话)处理。
		if s.cfg.Accounts != nil {
			if user, err := s.currentAccount(r); err == nil {
				if !auth.RoleCan(user.Role, perm) {
					s.rejectAuth(w, r, perm, "forbidden", string(user.Role))
					return
				}
				ctx := platform.WithActor(platform.WithTenant(r.Context(), user.TenantID), "user:"+user.ID)
				next(w, r.WithContext(ctx))
				return
			}
		}

		s.rejectAuth(w, r, perm, "missing", "")
	})
}

// rejectAuth 统一处理认证/授权失败: 打点、记日志、给出响应。
//
// 抽出来是因为两条身份路径 + 三种失败原因(缺凭据/凭据无效/权限不足)
// 如果在各处分别写, 迟早会漏掉某一处的审计 —— 而"谁在什么时候被拒绝过"
// 恰恰是排查越权尝试时最需要的信息。
func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request, perm auth.Permission, reason, role string) {
	s.metrics.AuthFailures.WithLabelValues(reason).Inc()
	attrs := append(platform.AuditAttrs(r.Context()),
		slog.String("required", string(perm)),
		slog.String("reason", reason))
	if role != "" {
		attrs = append(attrs, slog.String("role", role))
	}
	s.logger.WarnContext(r.Context(), "认证或授权失败", attrs...)
	switch reason {
	case "forbidden":
		writeError(w, http.StatusForbidden, "当前角色无权执行该操作")
	default:
		writeError(w, http.StatusUnauthorized, "缺少或无效的凭据: 请登录, 或提供 API Key")
	}
}

var errNoCredential = errors.New("api: 缺少凭据")

func (s *Server) principalFromRequest(r *http.Request) (auth.Principal, error) {
	raw := bearerToken(r)
	if raw == "" {
		return auth.Principal{}, errNoCredential
	}
	if s.cfg.Keys == nil {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	return s.cfg.Keys.Resolve(ctx, raw)
}

// bearerToken 支持标准 Authorization 头与 X-API-Key, 覆盖多数客户端习惯。
func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if token, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// newRequestID 生成一个短请求 ID。
func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "req-unknown"
	}
	return "req_" + hex.EncodeToString(buf)
}

// routeLabel 把路径归一化成低基数标签, 避免 /sessions/xxx 把指标打爆。
func routeLabel(r *http.Request) string {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/v1/sessions/") {
		return "/api/v1/sessions/{id}"
	}
	if strings.HasPrefix(p, "/api/v1/keys/") {
		return "/api/v1/keys/{id}"
	}
	if strings.HasPrefix(p, "/api/v1/candidates/") {
		return "/api/v1/candidates/{ref}"
	}
	if strings.HasPrefix(p, "/api/v1/jobs/") {
		return "/api/v1/jobs/{id}"
	}
	if strings.HasPrefix(p, "/api/v1/applications/") {
		return "/api/v1/applications/{id}"
	}
	if strings.HasPrefix(p, "/api/v1/questions/") {
		return "/api/v1/questions/{id}"
	}
	if strings.HasPrefix(p, "/api/v1/schedules/") {
		return "/api/v1/schedules/{id}"
	}
	if strings.HasPrefix(p, "/ws/") {
		return "/ws/interview/{id}"
	}
	return p
}

// withRateLimit 按客户端 IP 限流。
//
// 限流键用 IP 而不是租户: 这一层的目的是保护进程自身不被任何单一来源
// 打爆, 与租户配额(业务级、按合同约定)是两个不同的问题, 不该混在一起。
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "请求过于频繁, 请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}
