package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/privacy"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/resume"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

/* ---------------- 运维端点 ---------------- */

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReady 是就绪探针: 只表示"能接流量"。
// 存储不可用时应返回 503, 让负载均衡把实例摘掉, 而不是继续接单再失败。
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if _, err := s.cfg.Store.ListSessions(ctx, platform.Tenant(r.Context()), 1); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "reason": "存储不可用",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

/* ---------------- 面试会话 ---------------- */

type createSessionRequest struct {
	Round           int    `json:"round"`
	Minutes         int    `json:"minutes"`
	CandidateID     string `json:"candidate_id"`
	CandidateName   string `json:"candidate_name"`
	Position        string `json:"position"`
	Company         string `json:"company"`
	InterviewerName string `json:"interviewer_name"`
	ResumeText      string `json:"resume_text"`
	// ApplicationID 把这场面试挂到招聘管道上(可选)。
	// 传了它, 面试结果会自动回流到看板的对应轮次。
	ApplicationID    string `json:"application_id"`
	ConsentRecording bool   `json:"consent_recording"`
	ConsentScoring   bool   `json:"consent_scoring"`
}

type createSessionResponse struct {
	SessionID       string `json:"session_id"`
	TenantID        string `json:"tenant_id"`
	Round           int    `json:"round"`
	Minutes         int    `json:"minutes"`
	Position        string `json:"position"`
	Company         string `json:"company"`
	CandidateName   string `json:"candidate_name"`
	CandidateRef    string `json:"candidate_ref"`
	InterviewerName string `json:"interviewer_name"`
	Stage           string `json:"stage"`
	Status          string `json:"status"`
	WSURL           string `json:"ws_url"`
	// CandidateURL 是可以直接发给候选人的完整链接(含令牌)。
	// 服务端生成它, 而不是让每个调用方自己拼: 拼错的链接会变成
	// "候选人点进去看到 401", 而排查这件事要跨两个团队。
	CandidateURL string `json:"candidate_url"`
	// SessionToken 是发给候选人的一次性凭证, 只在这里返回一次。
	SessionToken string    `json:"session_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// clientError 表示"请求本身有问题"的错误, 需要翻译成 4xx 而不是 5xx。
//
// 不用字符串匹配来区分: 靠文案判断错误类型, 会在某次改文案时静默
// 把 400 变成 500, 而线上表现只是"这个接口偶尔报服务器错误"。
type clientError struct{ msg string }

func (e clientError) Error() string { return e.msg }

func clientErr(format string, args ...any) error {
	return clientError{msg: fmt.Sprintf(format, args...)}
}

// createSessionInput 是创建会话所需的全部输入。
//
// 抽出来是为了让"控制台发起面试"与"按面试安排开面"走同一条实现 ——
// 合规校验(同意、凭证、租户)只应该有一份, 否则迟早出现一条路径没校验。
type createSessionInput struct {
	Round         int
	Minutes       int
	Position      string
	Company       string
	CandidateName string
	CandidateRef  string
	Interviewer   string
	ResumeText    string
	ResumeJSON    []byte
	ApplicationID string
	// ConsentIP / ConsentUserAgent 是授权留痕的来源信息, 由接入层脱敏后传入。
	ConsentIP        string
	ConsentUserAgent string
	// ConsentConfirmed 表示授权是否在创建会话时就已取得。
	//
	// 两种来源的语义完全不同, 必须显式区分:
	//   - 客户 ATS 在投递环节已收集同意 -> true, 建会话时一并留痕;
	//   - 招聘同事在后台点"开始面试" -> false, 同意还没拿到, 必须由
	//     候选人本人在面试间确认。把这种情况当成"已同意"是伪造同意记录,
	//     它比不做记录更危险 —— 因为它看起来是有证据的。
	ConsentConfirmed bool
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	// 合规前置: 录音与 AI 评分都属于个人信息处理, 两项都必须有明示同意。
	//
	// 这里刻意做成服务端硬门槛: 前端勾选框可以被绕过, 而"未获同意就录音"
	// 或"未获同意就评分"在法律上不是技术瑕疵, 是违规处理个人信息。
	switch {
	case !req.ConsentRecording:
		writeError(w, http.StatusBadRequest, "需要候选人明示同意录音后才能开始面试")
		return
	case !req.ConsentScoring:
		writeError(w, http.StatusBadRequest, "需要候选人明示同意由 AI 评分后才能开始面试")
		return
	}

	// 授权留痕与建会话在同一事务里落库: "会话建好了但授权没记上"意味着
	// 持有录音却没有同意凭据 —— 那是合规事故, 不是数据不一致。
	tenant := platform.Tenant(ctx)
	candidateRef := ""
	if strings.TrimSpace(req.CandidateID) != "" {
		candidateRef = privacy.Ref(s.cfg.Secret, tenant, req.CandidateID)
	}
	in := createSessionInput{
		Round: req.Round, Minutes: req.Minutes,
		Position: req.Position, Company: req.Company,
		CandidateName: req.CandidateName, CandidateRef: candidateRef,
		Interviewer: req.InterviewerName, ResumeText: req.ResumeText,
		ApplicationID:    req.ApplicationID,
		ConsentIP:        privacy.MaskIP(clientIP(r)),
		ConsentUserAgent: privacy.MaskUserAgent(r.UserAgent()),
		// 请求带了两个同意标志, 说明调用方(通常是 ATS)已完成告知。
		ConsentConfirmed: true,
	}
	// 带上简历原文: 简历参与评分与追问(原文定位), 不再是"只存本地"。
	if strings.TrimSpace(req.ResumeText) != "" {
		in.ResumeJSON = resumeJSONOf(req.ResumeText)
	}

	sessionCtx := platform.WithTenant(ctx, tenant)
	sess, token, expiresAt, err := s.createSession(sessionCtx, in)
	if err != nil {
		var ce clientError
		if errors.As(err, &ce) {
			writeError(w, http.StatusBadRequest, ce.msg)
			return
		}
		s.storeErr(w, err, "create_session")
		return
	}
	candidateRef = sess.CandidateRef
	writeJSON(w, http.StatusOK, createSessionResponse{
		SessionID: sess.ID, TenantID: tenant, Round: sess.Round, Minutes: sess.Minutes,
		Position: sess.Position, Company: sess.Company, CandidateName: sess.CandidateName,
		CandidateRef: candidateRef, InterviewerName: sess.InterviewerName,
		Stage: sess.Stage, Status: string(sess.Status),
		WSURL: "/ws/interview/" + sess.ID, SessionToken: token, ExpiresAt: expiresAt,
		CandidateURL: "/?s=" + sess.ID + "&t=" + token,
	})
}

// createSession 建会话 + 记授权 + 签发凭证。控制台与面试安排共用这一条路径。
func (s *Server) createSession(ctx context.Context, in createSessionInput) (store.Session, string, time.Time, error) {
	tenant := platform.Tenant(ctx)
	if tenant == "" {
		return store.Session{}, "", time.Time{}, clientErr("缺少租户上下文")
	}
	if in.Round < 1 || in.Round > 5 {
		return store.Session{}, "", time.Time{}, clientErr("轮次必须在 1 到 5 之间")
	}
	if in.Minutes <= 0 {
		in.Minutes = 45
	}
	if in.Minutes > 180 {
		return store.Session{}, "", time.Time{}, clientErr("时长上限为 180 分钟")
	}
	// 题目数量与轮次强相关: 五面(HR)不应该被塞进 45 分钟技术深挖的题库。
	if in.ApplicationID != "" {
		sctx, cancel := s.storeCtx(ctx)
		app, err := s.cfg.Store.GetApplication(sctx, tenant, in.ApplicationID)
		cancel()
		if err != nil {
			return store.Session{}, "", time.Time{}, err
		}
		in.CandidateRef = defaultString(in.CandidateRef, app.CandidateRef)
	}

	sess := store.Session{
		ID:              newSessionID(),
		TenantID:        tenant,
		Position:        defaultString(in.Position, "后端工程师"),
		Company:         defaultString(in.Company, "示例科技"),
		CandidateName:   defaultString(in.CandidateName, "候选人"),
		CandidateRef:    in.CandidateRef,
		InterviewerName: defaultString(in.Interviewer, "林澈"),
		ResumeJSON:      in.ResumeJSON,
		ApplicationID:   in.ApplicationID,
		Round:           in.Round,
		Minutes:         in.Minutes,
		Stage:           "INIT",
		Status:          store.StatusRunning,
		CreatedAt:       time.Now().UTC(),
	}

	now := time.Now().UTC()
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	var consents []store.Consent
	if in.ConsentConfirmed {
		for _, scope := range []string{"recording", "scoring"} {
			consents = append(consents, store.Consent{
				TenantID: tenant, SessionID: sess.ID, CandidateID: sess.CandidateRef,
				Scope: scope, AgreedAt: now,
				IP: in.ConsentIP, UserAgent: in.ConsentUserAgent,
			})
		}
	}
	if err := s.cfg.Store.CreateSessionWithConsents(sctx, sess, consents); err != nil {
		s.metrics.StoreErrors.WithLabelValues("create_session").Inc()
		return store.Session{}, "", time.Time{}, err
	}

	token, expiresAt, err := s.issueSessionToken(sess.ID, tenant)
	if err != nil {
		return store.Session{}, "", time.Time{}, err
	}
	s.audit(ctx, store.AuditSessionCreate, sess.ID,
		fmt.Sprintf("round=%d minutes=%d position=%s", sess.Round, sess.Minutes, sess.Position))
	// 先把本租户的延迟类序列建出来, 让看板从 0 开始而不是"无数据"。
	s.metrics.Prime(tenant)
	s.metrics.SessionsStarted.WithLabelValues(tenant, strconv.Itoa(sess.Round)).Inc()
	return sess, token, expiresAt, nil
}

// resumeJSONOf 解析简历为带原文偏移的结构化实体。
func resumeJSONOf(text string) []byte {
	parsed := resume.NewRuleExtractor().Extract(text)
	data, err := json.Marshal(parsed)
	if err != nil {
		return nil
	}
	return data
}

// handleCandidateSession 让候选人用自己的令牌换取本场面试的展示信息。
func (s *Server) handleCandidateSession(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期, 请回到面试链接重新进入")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}
	// 候选人只看到与自己有关的最小信息, 不返回租户与候选人引用值等内部字段。
	payload := map[string]any{
		"session_id":       sess.ID,
		"round":            sess.Round,
		"minutes":          sess.Minutes,
		"position":         sess.Position,
		"company":          sess.Company,
		"candidate_name":   sess.CandidateName,
		"interviewer_name": sess.InterviewerName,
		"stage":            sess.Stage,
		"status":           sess.Status,
		"ws_url":           "/ws/interview/" + sess.ID,
		// 面试形态(视频/语音/编程)来自职位的轮次编排, 而不是写死在
		// 前端: 同一套页面要同时服务"一面纯视频"和"二面编程"。
		"mode":              "video",
		"round_name":        "第 1 轮",
		"human_panel":       false,
		"ice_servers":       s.iceServers(),
		"recording_enabled": s.recordingEnabled(),
		"resume_uploaded":   len(sess.ResumeJSON) > 0,
		"code_run_enabled":  s.cfg.Sandbox != nil,
		// 候选人必须自己确认授权; 只有招聘方在投递环节已代收同意时才为 false。
		"consent_required": !s.consentsComplete(r.Context(), claims.TenantID, sess.ID),
		"consent_scopes":   s.consentScopes(r.Context(), claims.TenantID, sess.ID)["scopes"],
	}
	if spec, ok := s.roundSpec(ctx, sess); ok {
		payload["mode"] = spec.Mode
		payload["round_name"] = spec.Name
		payload["human_panel"] = spec.HumanPanel
		payload["ai_lead"] = spec.AILead
	}
	writeJSON(w, http.StatusOK, payload)
}

// roundSpec 从"会话 -> 投递 -> 职位"取回本轮的编排配置。
//
// 链路是两跳, 因此容错必须显式: 任何一跳缺失(手动开的面试、职位被删)
// 都退回到默认的"视频面试", 而不是让候选人看到一个空白的面试间。
func (s *Server) roundSpec(ctx context.Context, sess store.Session) (store.RoundSpec, bool) {
	if sess.ApplicationID == "" {
		return store.RoundSpec{}, false
	}
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	app, err := s.cfg.Store.GetApplication(sctx, sess.TenantID, sess.ApplicationID)
	if err != nil {
		return store.RoundSpec{}, false
	}
	job, err := s.cfg.Store.GetJob(sctx, sess.TenantID, app.JobID)
	if err != nil {
		return store.RoundSpec{}, false
	}
	return job.RoundSpecOf(sess.Round)
}

// handleCandidateResume 让候选人在面试前上传/更新简历。
//
// 这是"简历参与评分"的入口。此前简历只存在浏览器本地, 面试官看到的
// 追问与简历无关 —— 那等于把"经历深挖"这一段做成了随机提问。
// 上传后简历会被解析成带原文偏移的实体, 既参与追问定位, 也会随
// 候选人数据一起被导出与删除。
func (s *Server) handleCandidateResume(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !decodeBody(w, r, 1<<20, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "简历内容不能为空")
		return
	}

	parsed := resume.NewRuleExtractor().Extract(req.Text)
	raw, err := json.Marshal(parsed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "解析简历失败")
		return
	}

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}
	sess.ResumeJSON = raw
	if err := s.cfg.Store.UpdateSession(ctx, sess); err != nil {
		s.storeErr(w, err, "update_session")
		return
	}
	// 同步到候选人档案(如果这场面试挂在投递上), 让"候选人详情页"
	// 与"面试间"看到的是同一份简历。
	if sess.CandidateRef != "" {
		if candidate, err := s.cfg.Store.GetCandidate(ctx, claims.TenantID, sess.CandidateRef); err == nil {
			candidate.ResumeSource = req.Text
			candidate.ResumeJSON = raw
			if err := s.cfg.Store.UpdateCandidate(ctx, candidate); err != nil {
				s.logger.WarnContext(r.Context(), "同步简历到候选人档案失败",
					append(platform.AuditAttrs(r.Context()), slog.Any("err", err))...)
			}
		}
	}
	s.audit(r.Context(), store.AuditCandidateUpsert, sess.ID,
		fmt.Sprintf("候选人上传简历, 解析出 %d 个实体", len(parsed.Entities)))
	writeJSON(w, http.StatusOK, parsed)
}

// handleCandidateReport 让候选人读取自己的报告。
//
// 候选人不可能拿 API Key 去调管理侧的 /sessions/{id}/report, 但"看到
// 关于自己的评估结论"是必须支持的 —— 面试结束后界面会跳到报告页,
// 而刷新页面不应该让报告消失。因此单独开一条候选人视角的读取接口,
// 只返回本人这一场的数据。
func (s *Server) handleCandidateReport(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}

	out := map[string]any{
		"session_id":   sess.ID,
		"round":        sess.Round,
		"status":       sess.Status,
		"stage":        sess.Stage,
		"position":     sess.Position,
		"company":      sess.Company,
		"created_at":   sess.CreatedAt,
		"consents":     s.consentScopes(r.Context(), claims.TenantID, sess.ID)["scopes"],
		"report_ready": sess.Status == store.StatusFinished,
	}
	if len(sess.ResumeJSON) > 0 {
		out["resume"] = json.RawMessage(sess.ResumeJSON)
	}
	if sess.Status != store.StatusFinished {
		writeJSON(w, http.StatusOK, out)
		return
	}
	rep, err := s.cfg.Store.GetReport(ctx, claims.TenantID, sess.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取报告失败")
		return
	}
	out["report"] = json.RawMessage(rep.Payload)
	writeJSON(w, http.StatusOK, out)
}

// proctorEvent 是候选人端上报的防作弊信号。
type proctorEvent struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	AtMS   int64  `json:"at_ms"`
}

// handleCandidateEvents 接收防作弊信号。
//
// 只记录、不判定: 系统产出的是"风险事件链", 由人决定它意味着什么。
// 自动淘汰会带来误伤与歧视风险, 这是明确的产品边界。
func (s *Server) handleCandidateEvents(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效")
		return
	}
	var ev proctorEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ev); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Store.AppendAudit(ctx, store.AuditEntry{
		TenantID: claims.TenantID,
		Actor:    "candidate",
		Action:   "proctor." + defaultString(ev.Type, "unknown"),
		Target:   claims.SessionID,
		Detail:   privacy.MaskUserAgent(ev.Detail),
	}); err != nil {
		s.metrics.StoreErrors.WithLabelValues("append_audit").Inc()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	// 租户来自凭据, 不接受查询参数 —— 否则多租户隔离形同虚设。
	sessions, err := s.cfg.Store.ListSessions(sctx, platform.Tenant(ctx), limit)
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("list_sessions").Inc()
		writeError(w, http.StatusInternalServerError, "查询会话列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.loadSession(r)
	if err != nil {
		s.writeStoreError(w, err, "读取会话失败")
		return
	}
	sctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	turns, err := s.cfg.Store.ListTurns(sctx, sess.TenantID, sess.ID)
	if err != nil {
		s.writeStoreError(w, err, "读取问答记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "turns": turns})
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	rep, err := s.cfg.Store.GetReport(sctx, platform.Tenant(ctx), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "报告尚未生成")
		return
	}
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("get_report").Inc()
		writeError(w, http.StatusInternalServerError, "读取报告失败")
		return
	}
	// 查看报告必须留痕: 报告里有候选人原话与评分依据,
	// "谁在什么时候看过"本身就是需要被审计的事实。
	s.audit(ctx, store.AuditReportView, id, "报告已被查看")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rep.Payload)
}

func (s *Server) handleGetConsents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	consents, err := s.cfg.Store.ListConsents(sctx, platform.Tenant(ctx), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取授权记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": consents})
}

type overrideRequest struct {
	Recommendation string `json:"recommendation"`
	Comment        string `json:"comment"`
}

// handleOverrideScore 记录人工改分。
//
// AI 给的是建议, 人做的是决定。这里不抹掉 AI 的原始结论, 而是把人工结论
// 与理由写进审计流 —— 原结论与人工结论都要留得住,
// 否则线上出问题无法复盘"当初为什么改的"。
func (s *Server) handleOverrideScore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req overrideRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	allowed := map[string]bool{
		"STRONG_HIRE": true, "HIRE": true, "PASS_WITH_CONCERN": true, "NO_HIRE": true,
	}
	if !allowed[req.Recommendation] {
		writeError(w, http.StatusBadRequest, "结论取值不合法")
		return
	}

	sess, err := s.loadSession(r)
	if err != nil {
		s.writeStoreError(w, err, "读取会话失败")
		return
	}
	previous := sess.Recommendation
	sess.Recommendation = req.Recommendation

	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	if err := s.cfg.Store.UpdateSession(sctx, sess); err != nil {
		s.writeStoreError(w, err, "更新会话失败")
		return
	}
	s.audit(ctx, store.AuditScoreOverride, sess.ID,
		fmt.Sprintf("人工结论 %s -> %s; 备注: %s",
			previous, req.Recommendation, privacy.MaskUserAgent(req.Comment)))
	s.metrics.HumanReviews.Inc()

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID, "recommendation": req.Recommendation, "previous": previous,
	})
}

// handleParseResume 解析简历文本并返回带原文偏移的结构化实体。
func (s *Server) handleParseResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "简历文本不能为空")
		return
	}
	parsed := resume.NewRuleExtractor().Extract(req.Text)
	writeJSON(w, http.StatusOK, parsed)
}

/* ---------------- 内部工具 ---------------- */

func (s *Server) loadSession(r *http.Request) (store.Session, error) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	return s.cfg.Store.GetSession(ctx, platform.Tenant(r.Context()), r.PathValue("id"))
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	s.metrics.StoreErrors.WithLabelValues("unknown").Inc()
	writeError(w, http.StatusInternalServerError, msg)
}

// audit 写审计日志。失败只记日志不阻断业务: 审计很重要, 但让"审计写不进去"
// 导致面试创建失败, 等于把可用性绑在次要路径上。
func (s *Server) audit(ctx context.Context, action, target, detail string) {
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	err := s.cfg.Store.AppendAudit(sctx, store.AuditEntry{
		TenantID: platform.Tenant(ctx),
		Actor:    platform.Actor(ctx),
		Action:   action,
		Target:   target,
		Detail:   detail,
	})
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("append_audit").Inc()
		s.logger.ErrorContext(ctx, "写审计日志失败",
			append(platform.AuditAttrs(ctx), slog.String("action", action), slog.Any("err", err))...)
	}
}

func (s *Server) issueSessionToken(sessionID, tenant string) (string, time.Time, error) {
	// TTL 兜底放在这里而不是只放在 NewServer: 任何绕过构造函数的调用
	// (测试、脚本、将来的新入口) 都会生成一张"签发即过期"的凭证,
	// 而它的表现是"候选人点链接后一直显示凭证无效", 极难定位。
	ttl := s.cfg.SessionTokenTTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	expiresAt := time.Now().Add(ttl)
	token, err := auth.IssueSessionToken(s.cfg.Secret, auth.SessionToken{
		SessionID: sessionID, TenantID: tenant, ExpiresAt: expiresAt,
	})
	return token, expiresAt, err
}

// candidateFromRequest 解析候选人凭证。
// 兼容 query 参数是因为浏览器 WebSocket 无法自定义请求头。
func (s *Server) candidateFromRequest(r *http.Request) (auth.SessionToken, error) {
	raw := bearerToken(r)
	if raw == "" {
		raw = r.URL.Query().Get("token")
	}
	if raw == "" {
		return auth.SessionToken{}, errors.New("api: 缺少候选人凭证")
	}
	return auth.VerifySessionToken(s.cfg.Secret, raw, time.Now())
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if idx := strings.Index(forwarded, ","); idx > 0 {
			return strings.TrimSpace(forwarded[:idx])
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newSessionID() string {
	return fmt.Sprintf("s_%d_%04d", time.Now().Unix(), rand.Intn(10000))
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
