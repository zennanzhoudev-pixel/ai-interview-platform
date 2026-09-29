package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/account"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 账号相关的接入层: 注册、登录、人脸、个人中心, 以及"登录后进哪个界面"。
//
// 身份模型有三种, 彼此不混用:
//   - 账号会话(Cookie): 人, 也就是这套界面服务招聘同学与候选人;
//   - API Key: 机器, 客户 ATS 集成;
//   - 会话令牌(URL 参数): 一次面试, 给没有账号的候选人用。
//
// 为什么候选人也要账号: 用户要求"做完面试后有历史记录回看"。
// 只有账号才能把"某个人"和"他参加过的多场面试"稳定地关联起来 ——
// 一次性链接可以看当场报告, 但无法回答"我上次那场怎么样"。

const sessionCookieName = "ios_session"

// setSessionCookie 下发登录会话 Cookie。
//
// 用 HttpOnly 而不是把令牌交给 JS 存 localStorage: XSS 一旦发生,
// localStorage 里的令牌会被直接读走, 而 HttpOnly Cookie 读不到。
// SameSite=Lax 让"从别的站点发起的跨站请求"不带这个 Cookie,
// 这是对 CSRF 的第一道防线(Cookie 认证方案必须有的防护)。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 本地 http 调试时不能加 Secure, 否则浏览器直接丢弃这个 Cookie。
		Secure: r.TLS != nil,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// currentAccount 从 Cookie 解析当前登录账号。
func (s *Server) currentAccount(r *http.Request) (account.User, error) {
	if s.cfg.Accounts == nil {
		return account.User{}, account.ErrSessionExpired
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return account.User{}, account.ErrSessionExpired
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	user, _, err := s.cfg.Accounts.Authenticate(ctx, cookie.Value)
	return user, err
}

// accountError 把账号域错误翻译成 HTTP 响应。
//
// 这里的取舍很具体: 注册类错误必须说清原因(否则用户无法完成注册),
// 登录类错误必须含糊(否则接口变成账号枚举器)。
func accountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, account.ErrUserExists):
		writeError(w, http.StatusConflict, "该邮箱或手机号已注册, 请直接登录")
	case errors.Is(err, account.ErrInviteRequired):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, account.ErrWeakPassword), errors.Is(err, account.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, account.ErrInvalidCredential):
		writeError(w, http.StatusUnauthorized, "账号或密码不正确")
	case errors.Is(err, account.ErrUserDisabled):
		writeError(w, http.StatusForbidden, "账号已被停用, 请联系管理员")
	case errors.Is(err, account.ErrFaceNotEnrolled):
		writeError(w, http.StatusPreconditionFailed, "该账号还没有录入人脸, 请先用密码登录并在个人中心完成录入")
	case errors.Is(err, account.ErrFaceMismatch):
		writeError(w, http.StatusUnauthorized, "人脸比对未通过, 可改用密码登录")
	case errors.Is(err, account.ErrFaceLowQuality), errors.Is(err, account.ErrUnsupportedImage):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, account.ErrSessionExpired):
		writeError(w, http.StatusUnauthorized, "登录已过期, 请重新登录")
	default:
		writeError(w, http.StatusInternalServerError, "账号操作失败")
	}
}

// handleRegister 注册账号。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账号功能未启用")
		return
	}
	var req struct {
		Name       string `json:"name"`
		Email      string `json:"email"`
		Phone      string `json:"phone"`
		Password   string `json:"password"`
		Role       string `json:"role"`
		InviteCode string `json:"invite_code"`
	}
	if !decodeBody(w, r, 256<<10, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	user, err := s.cfg.Accounts.Register(ctx, account.RegisterInput{
		Name: req.Name, Email: req.Email, Phone: req.Phone,
		Password: req.Password, Role: auth.Role(req.Role), InviteCode: req.InviteCode,
	})
	if err != nil {
		s.metrics.AuthFailures.WithLabelValues("register").Inc()
		accountError(w, err)
		return
	}
	// 注册后直接建立登录态: 让用户跳过"刚注册完还要再登录一次"这一步。
	_, session, err := s.cfg.Accounts.Login(ctx, account.LoginInput{
		TenantID:   user.TenantID,
		Identifier: firstNonEmpty(user.Email, user.Phone),
		Password:   req.Password,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		// 注册成功但自动登录失败: 仍然返回成功, 让前端走登录页。
		payload := s.sessionPayload(user, time.Time{})
		payload["auto_login"] = false
		writeJSON(w, http.StatusOK, payload)
		return
	}
	s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
	// 注册与登录返回**同一个响应结构**: 前端只需要处理一种"登录成功"
	// 的形态, 不必为两条路径各写一遍跳转逻辑(而两份逻辑迟早会不一致)。
	payload := s.sessionPayload(user, session.ExpiresAt)
	payload["auto_login"] = true
	writeJSON(w, http.StatusOK, payload)
}

// handleLogin 密码登录。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账号功能未启用")
		return
	}
	var req struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	user, session, err := s.cfg.Accounts.Login(ctx, account.LoginInput{
		Identifier: req.Identifier, Password: req.Password,
		IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		s.metrics.AuthFailures.WithLabelValues("password").Inc()
		s.logger.WarnContext(r.Context(), "密码登录失败",
			append(platform.AuditAttrs(r.Context()), slog.String("identifier_kind", identifierKind(req.Identifier)))...)
		accountError(w, err)
		return
	}
	s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.auditAccount(r.Context(), user, "auth.login", "密码登录")
	writeJSON(w, http.StatusOK, s.sessionPayload(user, session.ExpiresAt))
}

// handleFaceLogin 人脸登录。
func (s *Server) handleFaceLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "账号功能未启用")
		return
	}
	var req struct {
		Identifier string `json:"identifier"`
		Frame      string `json:"frame"`
	}
	if !decodeBody(w, r, 2<<20, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	user, session, score, err := s.cfg.Accounts.LoginWithFace(ctx, account.FaceLoginInput{
		Identifier: req.Identifier, Frame: req.Frame,
		IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		s.metrics.AuthFailures.WithLabelValues("face").Inc()
		s.auditAccount(r.Context(), account.User{TenantID: platform.Tenant(r.Context())}, "auth.face_fail",
			"人脸登录未通过")
		accountError(w, err)
		return
	}
	s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.auditAccount(r.Context(), user, "auth.face_login", "人脸登录成功")
	payload := s.sessionPayload(user, session.ExpiresAt)
	// 把相似度一起返回: 让"这次识别有多勉强"是可观测的, 而不是一个黑盒结论。
	payload["match_score"] = score
	payload["assurance"] = s.cfg.Accounts.MatcherInfo()["assurance"]
	writeJSON(w, http.StatusOK, payload)
}

// handleLogout 退出登录。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && s.cfg.Accounts != nil {
		ctx, cancel := s.storeCtx(r.Context())
		_ = s.cfg.Accounts.Logout(ctx, cookie.Value)
		cancel()
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe 返回当前登录账号。
//
// 前端靠它做两件事: 判断"要不要跳登录页", 以及"登录后进哪个界面"。
// 把"角色 -> 界面"的判断放在服务端返回的 role/staff 字段上, 而不是
// 让前端猜 —— 猜错的后果是把候选人放进管理工作台。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	payload := map[string]any{
		"user":  user,
		"role":  user.Role,
		"staff": user.Staff(),
	}
	if profile, err := s.cfg.Accounts.FaceStatus(ctx, user.TenantID, user.ID); err == nil {
		payload["face"] = map[string]any{
			"enrolled":    true,
			"matcher":     profile.Matcher,
			"assurance":   profile.Assurance,
			"quality":     profile.Quality,
			"frames":      profile.Frames,
			"enrolled_at": profile.EnrolledAt,
		}
	} else {
		payload["face"] = map[string]any{"enrolled": false}
	}
	payload["matcher"] = s.cfg.Accounts.MatcherInfo()
	writeJSON(w, http.StatusOK, payload)
}

// handleChangePassword 修改密码。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	var req struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Accounts.ChangePassword(ctx, user.TenantID, user.ID, req.Current, req.Next); err != nil {
		accountError(w, err)
		return
	}
	// 改密码会让所有会话失效(包括当前这个), 因此这里必须清掉 Cookie,
	// 并明确告诉前端"请重新登录" —— 否则用户会看到一个莫名其妙的 401。
	s.clearSessionCookie(w, r)
	s.auditAccount(r.Context(), user, "auth.password_change", "修改密码, 其它会话已失效")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relogin_required": true})
}

// handleFaceEnroll 录入/更新人脸。
func (s *Server) handleFaceEnroll(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	var req struct {
		Frames []string `json:"frames"`
	}
	if !decodeBody(w, r, 4<<20, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	profile, err := s.cfg.Accounts.EnrollFace(ctx, user.TenantID, user.ID, req.Frames)
	if err != nil {
		accountError(w, err)
		return
	}
	s.auditAccount(r.Context(), user, "auth.face_enroll",
		"录入人脸 "+strconv.Itoa(profile.Frames)+" 帧, 匹配器 "+profile.Matcher)
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled": true, "matcher": profile.Matcher, "assurance": profile.Assurance,
		"quality": profile.Quality, "frames": profile.Frames, "dim": profile.Dim,
		"note": "人脸模板只保存特征向量, 不保存照片; 可随时在个人中心移除。",
	})
}

// handleFaceDelete 移除人脸。
func (s *Server) handleFaceDelete(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Accounts.DeleteFace(ctx, user.TenantID, user.ID); err != nil {
		accountError(w, err)
		return
	}
	s.auditAccount(r.Context(), user, "auth.face_delete", "移除人脸模板")
	writeJSON(w, http.StatusOK, map[string]any{"enrolled": false})
}

// handleAccounts 列出本租户账号(管理员可见, 联系方式脱敏)。
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	users, err := s.cfg.Accounts.ListUsers(ctx, platform.Tenant(r.Context()), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账号列表失败")
		return
	}
	out := make([]account.User, 0, len(users))
	for _, u := range users {
		out = append(out, u.Masked())
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// handleCandidateHistory 返回当前候选人的历史面试记录。
//
// 这是"做完面试后能回看"的落点。候选人账号通过手机号/邮箱派生出
// 候选人引用值, 因此能对上招聘方导入的候选人档案与那些面试会话。
func (s *Server) handleCandidateHistory(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	ref := s.cfg.Accounts.CandidateRef(user)
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	type item struct {
		SessionID      string `json:"session_id"`
		Position       string `json:"position"`
		Company        string `json:"company"`
		Round          int    `json:"round"`
		Status         string `json:"status"`
		Recommendation string `json:"recommendation"`
		Score          int    `json:"score"`
		CreatedAt      string `json:"created_at"`
		ReportReady    bool   `json:"report_ready"`
	}
	items := make([]item, 0, 8)
	if ref != "" {
		sessions, err := s.cfg.Store.ListSessionsByCandidate(ctx, user.TenantID, ref, 50)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取历史记录失败")
			return
		}
		for _, sess := range sessions {
			it := item{
				SessionID: sess.ID, Position: sess.Position, Company: sess.Company,
				Round: sess.Round, Status: string(sess.Status),
				Recommendation: sess.Recommendation,
				CreatedAt:      sess.CreatedAt.Format(time.RFC3339),
				ReportReady:    sess.Status == "finished",
			}
			if rep, err := s.cfg.Store.GetReport(ctx, user.TenantID, sess.ID); err == nil {
				it.Score = rep.Score
			}
			items = append(items, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidate_ref": ref,
		"sessions":      items,
		"count":         len(items),
	})
}

// sessionPayload 是登录成功后返回给前端的信息。
func (s *Server) sessionPayload(user account.User, expiresAt time.Time) map[string]any {
	return map[string]any{
		"user":       user,
		"role":       user.Role,
		"staff":      user.Staff(),
		"expires_at": expiresAt,
		// next_path 让"登录后去哪"只由服务端决定一次, 前端不再自己判断。
		"next_path": staffHome(user),
	}
}

// staffHome 返回该角色登录后应进入的界面。
func staffHome(user account.User) string {
	if user.Staff() {
		return "/console/dashboard"
	}
	return "/candidate"
}

// withAccount 用账号会话鉴权并检查权限。
func (s *Server) withAccount(perm auth.Permission, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentAccount(r)
		if err != nil {
			accountError(w, err)
			return
		}
		if !auth.RoleCan(user.Role, perm) {
			s.metrics.AuthFailures.WithLabelValues("forbidden").Inc()
			writeError(w, http.StatusForbidden, "当前账号无权执行该操作")
			return
		}
		ctx := platform.WithActor(platform.WithTenant(r.Context(), user.TenantID), "user:"+user.ID)
		next(w, r.WithContext(ctx))
	})
}

func (s *Server) auditAccount(ctx context.Context, user account.User, action, detail string) {
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	actor := "anonymous"
	if user.ID != "" {
		actor = "user:" + user.ID
	}
	err := s.cfg.Store.AppendAudit(sctx, storeAuditEntry(user, actor, action, detail))
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("append_audit").Inc()
	}
}

func storeAuditEntry(user account.User, actor, action, detail string) store.AuditEntry {
	return store.AuditEntry{
		TenantID: user.TenantID,
		Actor:    actor,
		Action:   action,
		Target:   user.ID,
		Detail:   detail,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func identifierKind(identifier string) string {
	if strings.Contains(identifier, "@") {
		return "email"
	}
	return "phone"
}
