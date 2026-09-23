// Package api 提供面试的 HTTP / WebSocket 接入层。
//
// 接入层刻意保持"薄": 它只做四件事 —— 合规留痕、把请求翻译成引擎调用、
// 把引擎状态推给前端、把结果落库。业务规则一条都不放在这里,
// 否则状态机、预算调度和评分策略会被拆散到各个 handler 里, 再也无法回归。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/resume"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/web"
)

// Scorers 是一组用于交叉评分的评分器。
type Scorers struct {
	Primary   scoring.Scorer
	Secondary scoring.Scorer
	Arbiter   scoring.Scorer
	Tolerance int
}

// Config 配置接入层。
type Config struct {
	Store store.SessionStore
	// Checkpoint 为 nil 时退化为只用 MySQL: 快照只是热路径缓存,
	// 问答记录本身已经足够恢复会话, 缺少它不影响正确性。
	Checkpoint store.CheckpointStore
	TenantID   string
	// Scorers 每次开新会话时调用, 便于把模型消耗按会话归属。
	Scorers func() Scorers
	// ProbePlanner 决定追问方向。nil 时回落到"关键词缺失"策略。
	// 传 RAG 规划器时, 追问会检索参考答案要点、以原文为依据。
	ProbePlanner orchestrator.ProbePlanner
	Logger       *log.Logger
	// RateLimitPerSecond 与 RateLimitBurst 控制单机限流。
	// 留 0 时使用默认值(20/s, 突发 60)。
	RateLimitPerSecond float64
	RateLimitBurst     float64
}

// Server 是面试的 HTTP / WebSocket 接入层。
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	upgrader websocket.Upgrader
	logger   *log.Logger
	limiter  *limiter
}

// NewServer 构造接入层。
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	if cfg.Scorers == nil {
		cfg.Scorers = DefaultScorers
	}

	s := &Server{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		logger:  cfg.Logger,
		limiter: newLimiter(cfg.RateLimitPerSecond, cfg.RateLimitBurst),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 同源部署: 浏览器来的连接必须来自本机 host。
			// 空 Origin 放行是为了让 curl 与测试客户端也能连。
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == "" || strings.Contains(origin, r.Host)
			},
		},
	}
	s.routes()
	return s
}

// DefaultScorers 返回默认的评分器组合: 两个同源的规则评分器加一个仲裁器。
// 它保证系统在没有配置任何大模型时也能完整跑通 —— 这是降级能力的底线。
func DefaultScorers() Scorers {
	return Scorers{
		Primary:   scoring.NewKeywordScorer("rule-baseline-a", 0),
		Secondary: scoring.NewKeywordScorer("rule-baseline-b", 0),
		Arbiter:   scoring.NewKeywordScorer("rule-arbiter-c", 0),
		Tolerance: 1,
	}
}

// Handler 返回 HTTP 处理器(带单机限流)。
func (s *Server) Handler() http.Handler { return s.withRateLimit(s.mux) }

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

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	s.mux.HandleFunc("POST /api/v1/resume/parse", s.handleParseResume)
	s.mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("GET /api/v1/sessions/{id}/report", s.handleGetReport)
	s.mux.HandleFunc("GET /api/v1/sessions/{id}/consents", s.handleGetConsents)
	s.mux.HandleFunc("GET /ws/interview/{id}", s.handleInterview)
	// 静态资源挂在根路径。Go 1.22 的 ServeMux 优先匹配更具体的模式,
	// 所以 /api 与 /ws 不会被这里吞掉。
	s.mux.Handle("/", http.FileServer(http.FS(web.FS)))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"backend": fmt.Sprintf("%T", s.cfg.Store),
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

type createSessionRequest struct {
	Round            int    `json:"round"`
	Minutes          int    `json:"minutes"`
	CandidateID      string `json:"candidate_id"`
	TenantID         string `json:"tenant_id"`
	Position         string `json:"position"`
	Company          string `json:"company"`
	CandidateName    string `json:"candidate_name"`
	InterviewerName  string `json:"interviewer_name"`
	ResumeText       string `json:"resume_text"`
	ConsentRecording bool   `json:"consent_recording"`
}

type sessionResponse struct {
	SessionID       string `json:"session_id"`
	Round           int    `json:"round"`
	Minutes         int    `json:"minutes"`
	Position        string `json:"position"`
	Company         string `json:"company"`
	CandidateName   string `json:"candidate_name"`
	InterviewerName string `json:"interviewer_name"`
	Stage           string `json:"stage"`
	Status          string `json:"status"`
	WSURL           string `json:"ws_url"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Round < 1 || req.Round > 5 {
		writeError(w, http.StatusBadRequest, "轮次必须在 1 到 5 之间")
		return
	}
	if req.Minutes <= 0 {
		req.Minutes = 45
	}
	if req.Minutes > 180 {
		writeError(w, http.StatusBadRequest, "时长上限为 180 分钟")
		return
	}

	// 合规前置: 招聘场景下录音与评分属于个人信息处理, 没有明示同意
	// 就不能开始。把它做成服务端的硬门槛而不是前端的一个勾选框,
	// 是因为前端可以被绕过, 而法律风险不会。
	if !req.ConsentRecording {
		writeError(w, http.StatusBadRequest, "需要候选人明示同意后才能开始面试")
		return
	}

	tenant := req.TenantID
	if tenant == "" {
		tenant = s.cfg.TenantID
	}

	var resumeJSON []byte
	if strings.TrimSpace(req.ResumeText) != "" {
		parsed := resume.NewRuleExtractor().Extract(req.ResumeText)
		if data, err := json.Marshal(parsed); err == nil {
			resumeJSON = data
		}
	}

	sess := store.Session{
		ID:              newSessionID(),
		TenantID:        tenant,
		Position:        defaultString(req.Position, "后端工程师"),
		Company:         defaultString(req.Company, "示例科技"),
		CandidateName:   defaultString(req.CandidateName, "候选人"),
		InterviewerName: defaultString(req.InterviewerName, "林澈"),
		ResumeJSON:      resumeJSON,
		Round:           req.Round,
		Minutes:         req.Minutes,
		Stage:           string(orchestrator.StageInit),
		Status:          store.StatusRunning,
	}
	if err := s.cfg.Store.CreateSession(r.Context(), sess); err != nil {
		s.logger.Printf("创建会话失败: %v", err)
		writeError(w, http.StatusInternalServerError, "创建会话失败")
		return
	}

	// 授权留痕: 记下同意的时间、范围、来源 IP 与 UA。
	// 事后追责时, 只有"某人某时从某 IP 同意了某范围"这一组信息
	// 才构成有效证据。
	now := time.Now().UTC()
	for _, scope := range []string{"recording", "scoring"} {
		if err := s.cfg.Store.SaveConsent(r.Context(), store.Consent{
			SessionID:   sess.ID,
			CandidateID: req.CandidateID,
			Scope:       scope,
			AgreedAt:    now,
			IP:          clientIP(r),
			UserAgent:   r.UserAgent(),
		}); err != nil {
			s.logger.Printf("写入授权留痕失败: %v", err)
		}
	}

	writeJSON(w, http.StatusOK, sessionResponse{
		SessionID:       sess.ID,
		Round:           sess.Round,
		Minutes:         sess.Minutes,
		Position:        sess.Position,
		Company:         sess.Company,
		CandidateName:   sess.CandidateName,
		InterviewerName: sess.InterviewerName,
		Stage:           sess.Stage,
		Status:          string(sess.Status),
		WSURL:           "/ws/interview/" + sess.ID,
	})
}

// handleParseResume 解析简历文本并返回带原文偏移的结构化实体。
//
// 这是无状态接口: 输入一段简历, 输出技能/项目/时间线, 以及交叉校验告警。
// 它独立于面试会话存在, 便于候选人在准备阶段先看到"系统读懂了什么"。
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

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	sessions, err := s.cfg.Store.ListSessions(r.Context(), r.URL.Query().Get("tenant_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询会话列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.cfg.Store.GetSession(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}
	turns, err := s.cfg.Store.ListTurns(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取问答记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "turns": turns})
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.cfg.Store.GetReport(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "报告尚未生成")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取报告失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rep.Payload)
}

// handleGetConsents 返回候选人数据授权留痕。
//
// 把这个接口开放出来不是为了"功能齐全", 而是因为授权本来就该可查:
// 候选人有权知道自己同意了什么、什么时候同意的。把它藏起来,
// 等于让合规声明变成一句无法验证的话。
func (s *Server) handleGetConsents(w http.ResponseWriter, r *http.Request) {
	consents, err := s.cfg.Store.ListConsents(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取授权记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": consents})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type clientMessage struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	DurationMS int64  `json:"duration_ms"`
}

// handleInterview 是面试的实时通道。
//
// 断线重连在这里是自然成立的: 引擎状态完全由已落库的问答记录重放得到,
// 服务端不在内存里保存任何会话状态。这意味着重连不仅能跨连接, 还能跨
// 进程重启 —— 而不是"只有同一个进程没挂才能恢复"。
func (s *Server) handleInterview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	sess, err := s.cfg.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 失败时已经写过响应
	}
	defer func() { _ = conn.Close() }()

	// 已结束的会话: 直接把报告推回去, 而不是重新开一场。
	if sess.Status == store.StatusFinished {
		if rep, err := s.cfg.Store.GetReport(ctx, id); err == nil {
			_ = conn.WriteJSON(map[string]any{
				"type": "report", "payload": json.RawMessage(rep.Payload),
			})
			return
		}
	}

	eng, turns, err := s.buildEngine(ctx, sess)
	if err != nil {
		_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
		return
	}

	// 先确定"接下来要问什么", 再推送状态。顺序反过来会让 state 里的
	// 阶段是空的 —— 因为引擎在 Start() 之前还没有进入任何阶段。
	var next orchestrator.Decision
	if qid, qtext, ok := eng.Pending(); ok {
		next = orchestrator.Decision{
			Action:     orchestrator.ActionAsk,
			Stage:      eng.Stage(),
			QuestionID: qid,
			Question:   qtext,
			IsProbe:    isProbeID(qid),
		}
	} else {
		next = eng.Start()
	}

	_ = conn.WriteJSON(map[string]any{
		"type":             "state",
		"session_id":       sess.ID,
		"round":            sess.Round,
		"minutes":          sess.Minutes,
		"position":         sess.Position,
		"company":          sess.Company,
		"candidate_name":   sess.CandidateName,
		"interviewer_name": sess.InterviewerName,
		"stage":            string(eng.Stage()),
		"resumed":          len(turns) > 0,
		"turn_count":       len(turns),
		"elapsed_sec":      elapsedSeconds(turns),
	})

	// 记录里的问答已经把面试推到终点: 不重新问, 直接出报告。
	if eng.Finished() {
		rep, err := s.finishSession(ctx, sess, eng)
		if err != nil {
			_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
			return
		}
		payload, _ := json.Marshal(rep)
		_ = conn.WriteJSON(map[string]any{"type": "report", "payload": json.RawMessage(payload)})
		return
	}

	_ = conn.WriteJSON(questionPayload(next, len(turns)+1))

	turnCount := len(turns)
	questionSentAt := time.Now()

	for {
		var msg clientMessage
		if err := conn.ReadJSON(&msg); err != nil {
			// 客户端断开(关页面、切网络、手机锁屏)是常态, 不是异常。
			// 这里不打错误日志, 否则线上日志会被正常断开淹没。
			return
		}

		switch msg.Type {
		case "ping":
			_ = conn.WriteJSON(map[string]any{"type": "pong"})

		case "answer":
			if strings.TrimSpace(msg.Text) == "" {
				_ = conn.WriteJSON(map[string]any{"type": "error", "message": "回答不能为空"})
				continue
			}

			// 回答耗时用服务端测量的值: 前端上报的值可以造假,
			// 而耗时直接影响时间预算调度和报告里的节奏指标。
			took := time.Since(questionSentAt)
			if took > 30*time.Minute {
				took = 30 * time.Minute
			}

			decision, err := eng.Submit(msg.Text, took)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
				continue
			}
			if err := s.persistTurn(ctx, sess.ID, decision.Turn); err != nil {
				s.logger.Printf("落库失败: %v", err)
				_ = conn.WriteJSON(map[string]any{"type": "error", "message": "记录本轮问答失败"})
				continue
			}
			turnCount++
			_ = conn.WriteJSON(turnResultPayload(decision))

			if decision.Action == orchestrator.ActionFinish {
				rep, err := s.finishSession(ctx, sess, eng)
				if err != nil {
					_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
					return
				}
				payload, _ := json.Marshal(rep)
				_ = conn.WriteJSON(map[string]any{
					"type": "report", "payload": json.RawMessage(payload),
				})
				return
			}

			_ = conn.WriteJSON(questionPayload(decision, turnCount+1))
			questionSentAt = time.Now()
			s.saveCheckpoint(ctx, sess.ID, eng.Stage(), turnCount, decision.QuestionID)
		}
	}
}

func (s *Server) buildEngine(ctx context.Context, sess store.Session) (*orchestrator.Engine, []store.Turn, error) {
	turns, err := s.cfg.Store.ListTurns(ctx, sess.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("读取问答记录失败: %w", err)
	}

	total := time.Duration(sess.Minutes) * time.Minute
	plan := orchestrator.DefaultPlan(total)
	plan.Round = sess.Round

	sc := s.cfg.Scorers()
	opts := []orchestrator.Option{
		orchestrator.WithScorers(sc.Primary, sc.Secondary, sc.Arbiter, sc.Tolerance),
	}
	if s.cfg.ProbePlanner != nil {
		opts = append(opts, orchestrator.WithProbePlanner(s.cfg.ProbePlanner))
	}
	eng := orchestrator.NewEngine(plan, orchestrator.DefaultBank(), total, opts...)
	if len(sess.ResumeJSON) > 0 {
		var parsed resume.Resume
		if err := json.Unmarshal(sess.ResumeJSON, &parsed); err == nil {
			eng.SetResume(&parsed)
		}
	}

	if len(turns) == 0 {
		return eng, turns, nil
	}

	engineTurns := make([]orchestrator.Turn, 0, len(turns))
	for _, t := range turns {
		et, err := toEngineTurn(t)
		if err != nil {
			return nil, nil, err
		}
		engineTurns = append(engineTurns, et)
	}
	if err := eng.Restore(engineTurns); err != nil {
		return nil, nil, fmt.Errorf("恢复面试进度失败: %w", err)
	}
	return eng, turns, nil
}

func (s *Server) persistTurn(ctx context.Context, sessionID string, t *orchestrator.Turn) error {
	if t == nil {
		return nil
	}
	st := store.Turn{
		SessionID:  sessionID,
		Index:      t.Index,
		Stage:      string(t.Stage),
		QuestionID: t.QuestionID,
		Competency: t.Competency,
		Question:   t.Question,
		Answer:     t.Answer,
		DurationMS: t.Duration.Milliseconds(),
		IsProbe:    t.IsProbe,
		Scored:     t.Scored,
		CreatedAt:  time.Now().UTC(),
	}
	if t.Verdict != nil {
		raw, err := json.Marshal(t.Verdict)
		if err != nil {
			return err
		}
		st.Verdict = raw
		st.Level = t.Verdict.Final.Level.String()
		st.LevelNum = t.Verdict.Final.Level.Number()
		st.Confidence = t.Verdict.Final.Confidence
		st.DegradedFrom = t.Verdict.Final.DegradedFrom
	}
	return s.cfg.Store.AppendTurn(ctx, st)
}

func (s *Server) finishSession(ctx context.Context, sess store.Session, eng *orchestrator.Engine) (orchestrator.Report, error) {
	rep := eng.Report()
	payload, err := json.Marshal(rep)
	if err != nil {
		return rep, err
	}
	if err := s.cfg.Store.SaveReport(ctx, store.Report{
		SessionID:      sess.ID,
		Recommendation: rep.Recommendation,
		Confidence:     rep.Confidence,
		Payload:        payload,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		return rep, fmt.Errorf("保存报告失败: %w", err)
	}

	sess.Stage = string(orchestrator.StageDone)
	sess.Status = store.StatusFinished
	sess.Recommendation = rep.Recommendation
	if err := s.cfg.Store.UpdateSession(ctx, sess); err != nil {
		return rep, fmt.Errorf("更新会话状态失败: %w", err)
	}

	// 面试正常结束: 清掉快照。快照是热路径缓存而不是数据来源,
	// 该过期就得过期, 否则峰值期会把 Redis 越堆越满。
	if s.cfg.Checkpoint != nil {
		_ = s.cfg.Checkpoint.Delete(ctx, sess.ID)
	}
	return rep, nil
}

type checkpoint struct {
	SessionID       string    `json:"session_id"`
	Stage           string    `json:"stage"`
	TurnCount       int       `json:"turn_count"`
	PendingQuestion string    `json:"pending_question"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (s *Server) saveCheckpoint(ctx context.Context, sessionID string, stage orchestrator.Stage, turnCount int, pending string) {
	if s.cfg.Checkpoint == nil {
		return
	}
	raw, err := json.Marshal(checkpoint{
		SessionID:       sessionID,
		Stage:           string(stage),
		TurnCount:       turnCount,
		PendingQuestion: pending,
		UpdatedAt:       time.Now().UTC(),
	})
	if err != nil {
		return
	}
	// 面试进行中每轮写一次快照。它换来的是"断开后立刻知道从哪继续",
	// 而不必先把整场问答从 MySQL 读出来重放一遍。
	if err := s.cfg.Checkpoint.Save(ctx, sessionID, raw, 24*time.Hour); err != nil {
		s.logger.Printf("写会话快照失败: %v", err)
	}
}

func questionPayload(d orchestrator.Decision, index int) map[string]any {
	return map[string]any{
		"type":  "question",
		"id":    d.QuestionID,
		"text":  d.Question,
		"index": index,
		"probe": d.IsProbe,
		"stage": string(d.Stage),
	}
}

func turnResultPayload(d orchestrator.Decision) map[string]any {
	out := map[string]any{
		"type":   "turn_result",
		"action": string(d.Action),
		"stage":  string(d.Stage),
		"scored": false,
	}
	if d.Turn == nil || d.Turn.Verdict == nil {
		return out
	}
	v := d.Turn.Verdict
	out["scored"] = d.Turn.Scored
	out["index"] = d.Turn.Index
	out["level"] = v.Final.Level.String()
	out["level_num"] = v.Final.Level.Number()
	out["confidence"] = v.Final.Confidence
	out["degraded"] = v.Final.DegradedFrom != ""
	out["evidence"] = v.Final.Evidence
	out["missing"] = v.Final.Missing
	return out
}

func toEngineTurn(t store.Turn) (orchestrator.Turn, error) {
	out := orchestrator.Turn{
		Index:      t.Index,
		Stage:      orchestrator.Stage(t.Stage),
		QuestionID: t.QuestionID,
		Competency: t.Competency,
		Question:   t.Question,
		Answer:     t.Answer,
		Duration:   time.Duration(t.DurationMS) * time.Millisecond,
		IsProbe:    t.IsProbe,
		Scored:     t.Scored,
	}
	if !t.Scored {
		return out, nil
	}
	if len(t.Verdict) == 0 {
		// 标记载了评分却没有结论: 数据不完整。这里必须报错,
		// 因为"静默当作没评分"会让重连后的报告少掉一整轮考察结果,
		// 而且不会有任何人发现。
		return out, fmt.Errorf("第 %d 轮标记为已评分, 但缺少评分结论", t.Index)
	}
	var v scoring.Verdict
	if err := json.Unmarshal(t.Verdict, &v); err != nil {
		return out, fmt.Errorf("第 %d 轮评分结论无法解析: %w", t.Index, err)
	}
	out.Verdict = &v
	return out, nil
}

func elapsedSeconds(turns []store.Turn) int64 {
	var ms int64
	for _, t := range turns {
		ms += t.DurationMS
	}
	return ms / 1000
}

// isProbeID 判断题目是否为追问。
// 追问的 ID 由引擎生成为 "根题目.pN", 重连时据此还原界面上的追问标记。
func isProbeID(id string) bool { return strings.Contains(id, ".p") }
