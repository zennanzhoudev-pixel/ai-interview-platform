package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/resume"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// WebSocket 保活与超时参数。
//
// 面试是 45 分钟的长连接, 中间有大量静默(候选人思考、写字)。没有心跳,
// 任何一层负载均衡/NAT 都会把空闲连接掐掉; 没有读超时, 对端半开连接
// (拔网线、切后台)会让服务端 goroutine 永久挂着。这两条在压测时不暴露,
// 一上真实网关就炸。
const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 25 * time.Second
	wsReadLimit  = 1 << 20 // 1MB, 防止超大帧打爆内存
)

var (
	errSessionForbidden = errors.New("api: 凭证与会话不匹配")
	errSessionMissing   = errors.New("api: 会话不存在")
)

// handleInterview 是面试的实时通道。
//
// 断线重连在这里是自然成立的: 引擎状态完全由已落库的问答记录重放得到,
// 服务端不在内存里保存会话状态 —— 因此重连不仅能跨连接, 还能跨进程重启。
func (s *Server) handleInterview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 人类面试官走旁听席: 它复用同一个 WebSocket 路径, 但权限模型、
	// 可做的操作、能看到的字段都完全不同。用一个查询参数分流而不是
	// 开第二条路径, 是为了让"信令中转"天然落在同一个连接里。
	if r.URL.Query().Get("role") == "observer" {
		sess, err := s.authorizeObserver(r)
		if err != nil {
			writeError(w, http.StatusForbidden, "旁听票据无效或已过期")
			return
		}
		s.handleObserver(w, r, sess)
		return
	}

	sess, err := s.authorizeSession(r)
	if err != nil {
		switch {
		case errors.Is(err, errSessionMissing):
			writeError(w, http.StatusNotFound, "会话不存在")
		case errors.Is(err, errSessionForbidden):
			writeError(w, http.StatusForbidden, "该凭证不属于这场面试")
		default:
			writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		}
		return
	}
	// 会话上下文贯穿整个连接生命周期, 日志与审计都能带上 session_id。
	ctx = platform.WithSession(platform.WithTenant(ctx, sess.TenantID), sess.ID)

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 失败时已经写过响应
	}
	defer func() { _ = conn.Close() }()

	s.metrics.WSConnections.Inc()
	defer s.metrics.WSConnections.Dec()

	conn.SetReadLimit(wsReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	writer := &connWriter{conn: conn}
	stopPing := s.startKeepalive(ctx, writer)
	defer stopPing()

	// 未取得授权就不开面。
	//
	// 这道门槛放在 WebSocket 建连之后而不是之前, 是为了能用同一条连接
	// 把"需要同意"这个原因告诉候选人 —— 直接拒绝握手只会让前端看到
	// 一个没有理由的连接失败。
	if !s.consentsComplete(ctx, sess.TenantID, sess.ID) {
		writer.writeJSON(map[string]any{
			"type":    "consent_required",
			"message": "开始前需要你确认录音与 AI 评分授权。",
			"scopes":  []string{"recording", "scoring"},
		})
		return
	}

	// 候选人进入房间。人类面试官(3 面以上常见)能从这一刻起旁听,
	// 并通过 WebRTC 与候选人建立点对点视频。
	selfID := newID("peer")
	s.rooms.join(sess.ID, &peer{id: selfID, role: "candidate", writer: writer})
	defer s.rooms.leave(sess.ID, selfID)

	// 已结束的会话: 直接把报告推回去, 而不是重新开一场。
	if sess.Status == store.StatusFinished {
		rep, rerr := s.cfg.Store.GetReport(ctx, sess.TenantID, sess.ID)
		if rerr == nil {
			writer.writeJSON(map[string]any{"type": "report", "payload": json.RawMessage(rep.Payload)})
			return
		}
	}

	eng, turns, err := s.buildEngine(ctx, sess)
	if err != nil {
		writer.writeJSON(map[string]any{"type": "error", "message": err.Error()})
		return
	}

	// 先确定"接下来要问什么", 再推送状态 —— 顺序反过来会让 state 里的阶段是空的。
	var next orchestrator.Decision
	if qid, qtext, ok := eng.Pending(); ok {
		next = orchestrator.Decision{
			Action: orchestrator.ActionAsk, Stage: eng.Stage(),
			QuestionID: qid, Question: qtext, IsProbe: isProbeID(qid),
		}
	} else {
		next = eng.Start()
	}

	statePayload := map[string]any{
		"type":             "state",
		"session_id":       sess.ID,
		"peer_id":          selfID,
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
		"peers":            s.peerRoster(sess.ID),
		"ice_servers":      s.iceServers(),
	}
	if spec, ok := s.roundSpec(ctx, sess); ok {
		statePayload["mode"] = spec.Mode
		statePayload["round_name"] = spec.Name
		statePayload["human_panel"] = spec.HumanPanel
	}
	// 面试间是"同一场面试在看的人"的共享状态, 因此新连接加入要广播。
	s.rooms.broadcast(sess.ID, map[string]any{
		"type": "peer_joined", "peer_id": selfID, "role": "candidate",
		"peers": s.peerRoster(sess.ID),
	}, selfID)
	writer.writeJSON(statePayload)

	if eng.Finished() {
		rep, ferr := s.finishSession(ctx, sess, eng)
		if ferr != nil {
			writer.writeJSON(map[string]any{"type": "error", "message": ferr.Error()})
			return
		}
		payload, _ := json.Marshal(rep)
		writer.writeJSON(map[string]any{"type": "report", "payload": json.RawMessage(payload)})
		return
	}

	writer.writeJSON(questionPayload(next, len(turns)+1))
	s.rooms.broadcast(sess.ID, questionPayload(next, len(turns)+1), selfID)

	turnCount := len(turns)
	questionSentAt := time.Now()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			// 客户端断开(关页面、切网络、手机锁屏)是常态, 不是异常, 不打错误日志。
			return
		}
		s.metrics.WSMessages.WithLabelValues(messageKind(mt)).Inc()

		if mt == websocket.BinaryMessage {
			if s.cfg.ASR == nil || s.cfg.TTS == nil {
				writer.writeJSON(map[string]any{
					"type": "error", "message": "语音模式未启用: 需要配置 ASR 与 TTS 提供方",
				})
				return
			}
			s.voiceLoop(ctx, conn, eng, sess, data)
			return
		}

		// WebRTC 信令优先于业务消息: 视频建立得早一点, 面试官就能早一点
		// 看到候选人, 而不是等第一个问题问完。
		if s.handleSignal(ctx, sess.ID, selfID, "candidate", data) {
			continue
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == "start_voice" {
			if s.cfg.ASR == nil || s.cfg.TTS == nil {
				writer.writeJSON(map[string]any{
					"type": "error", "message": "语音模式未启用: 需要配置 ASR 与 TTS 提供方",
				})
				continue
			}
			s.voiceLoop(ctx, conn, eng, sess, nil)
			return
		}

		switch msg.Type {
		case "ping":
			writer.writeJSON(map[string]any{"type": "pong"})

		case "answer":
			if len(strings.TrimSpace(msg.Text)) == 0 {
				writer.writeJSON(map[string]any{"type": "error", "message": "回答不能为空"})
				continue
			}
			took := time.Since(questionSentAt)
			if took > 30*time.Minute {
				took = 30 * time.Minute
			}
			replyStarted := time.Now()
			decision, err := eng.Submit(msg.Text, took)
			if err != nil {
				writer.writeJSON(map[string]any{"type": "error", "message": err.Error()})
				continue
			}
			if err := s.persistTurn(ctx, sess, decision.Turn); err != nil {
				s.logger.ErrorContext(ctx, "落库失败", append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
				writer.writeJSON(map[string]any{"type": "error", "message": "记录本轮问答失败"})
				continue
			}
			turnCount++
			// 首字延迟: 从收到回答到准备发出下一个问题。这是语音面试的生命线指标。
			s.metrics.FirstResponse.WithLabelValues(sess.TenantID).
				Observe(time.Since(replyStarted).Seconds())
			writer.writeJSON(turnResultPayload(decision))
			s.rooms.broadcast(sess.ID, turnResultPayload(decision), selfID)

			if decision.Action == orchestrator.ActionFinish {
				rep, ferr := s.finishSession(ctx, sess, eng)
				if ferr != nil {
					writer.writeJSON(map[string]any{"type": "error", "message": ferr.Error()})
					return
				}
				payload, _ := json.Marshal(rep)
				writer.writeJSON(map[string]any{"type": "report", "payload": json.RawMessage(payload)})
				s.rooms.broadcast(sess.ID, map[string]any{"type": "report", "payload": json.RawMessage(payload)}, selfID)
				return
			}

			writer.writeJSON(questionPayload(decision, turnCount+1))
			s.rooms.broadcast(sess.ID, questionPayload(decision, turnCount+1), selfID)
			questionSentAt = time.Now()
			s.saveCheckpoint(ctx, sess, eng.Stage(), turnCount, decision.QuestionID)
		}
	}
}

// authorizeSession 校验候选人凭证与会话的归属关系。
//
// 关键点: 凭证里的 session_id 必须与 URL 里的 id 一致。
// 否则拿到自己的令牌就能去读别人的面试 —— 这是最容易漏掉的一处越权。
func (s *Server) authorizeSession(r *http.Request) (store.Session, error) {
	id := r.PathValue("id")
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	if !s.cfg.RequireAuth {
		sess, err := s.cfg.Store.GetSession(ctx, s.cfg.TenantID, id)
		if errors.Is(err, store.ErrNotFound) {
			return store.Session{}, errSessionMissing
		}
		return sess, err
	}

	claims, err := s.candidateFromRequest(r)
	if err != nil {
		return store.Session{}, err
	}
	if claims.SessionID != id {
		return store.Session{}, errSessionForbidden
	}
	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, errSessionMissing
	}
	return sess, err
}

// startKeepalive 启动心跳。返回停止函数。
// authorizeObserver 校验旁听票据。
//
// 票据里带角色标记, 因此"候选人令牌"和"旁听票据"虽然格式相同,
// 却不能互换使用 —— 验证时必须比对角色, 否则候选人拿自己的令牌
// 就能以面试官身份进入旁听席(以及拿到房间里的信令通道)。
func (s *Server) authorizeObserver(r *http.Request) (store.Session, error) {
	raw := r.URL.Query().Get("ticket")
	if raw == "" {
		raw = bearerToken(r)
	}
	if raw == "" {
		return store.Session{}, errors.New("api: 缺少旁听票据")
	}
	claims, err := auth.VerifySessionToken(s.cfg.Secret, raw, time.Now())
	if err != nil {
		s.metrics.AuthFailures.WithLabelValues("observer_ticket").Inc()
		return store.Session{}, err
	}
	if claims.TokenRole() != auth.TokenRoleObserver {
		s.metrics.AuthFailures.WithLabelValues("observer_role").Inc()
		return store.Session{}, errors.New("api: 票据角色不是旁听席")
	}
	id := r.PathValue("id")
	if claims.SessionID != id {
		s.metrics.AuthFailures.WithLabelValues("observer_scope").Inc()
		return store.Session{}, errSessionForbidden
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, errSessionMissing
	}
	if err != nil {
		return store.Session{}, err
	}
	s.audit(r.Context(), store.AuditObserverJoin, sess.ID, "人类面试官进入面试间")
	return sess, nil
}

// startKeepalive 启动心跳。返回停止函数。
func (s *Server) startKeepalive(ctx context.Context, w *connWriter) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.writeControl(websocket.PingMessage, wsWriteWait); err != nil {
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (s *Server) buildEngine(ctx context.Context, sess store.Session) (*orchestrator.Engine, []store.Turn, error) {
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	turns, err := s.cfg.Store.ListTurns(sctx, sess.TenantID, sess.ID)
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

	// 题库与追问规划器都来自本租户的知识库: 抽什么题、追问什么方向,
	// 必须由同一份数据决定, 否则报告里的依据会和实际问的问题对不上。
	corpus := s.knowledge.get(ctx, sess.TenantID)
	bank := orchestrator.DefaultBank()
	if corpus != nil {
		bank = corpus.Bank()
		opts = append(opts, orchestrator.WithProbePlanner(corpus.Planner()))
	} else if s.cfg.ProbePlanner != nil {
		opts = append(opts, orchestrator.WithProbePlanner(s.cfg.ProbePlanner))
	}
	eng := orchestrator.NewEngine(plan, bank, total, opts...)
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

func (s *Server) persistTurn(ctx context.Context, sess store.Session, t *orchestrator.Turn) error {
	if t == nil {
		return nil
	}
	st := store.Turn{
		TenantID:   sess.TenantID,
		SessionID:  sess.ID,
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

	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	if err := s.cfg.Store.AppendTurn(sctx, st); err != nil {
		return err
	}

	s.metrics.Turns.WithLabelValues(sess.TenantID, string(t.Stage), boolLabel(t.Scored)).Inc()
	if st.Level != "" {
		s.metrics.ScoreDistribution.WithLabelValues(sess.TenantID, t.Competency, st.Level).Inc()
	}
	if st.DegradedFrom != "" {
		s.metrics.DegradedScores.WithLabelValues(sess.TenantID).Inc()
	}
	return nil
}

// finishSession 在一个事务里保存报告并更新会话终态。
func (s *Server) finishSession(ctx context.Context, sess store.Session, eng *orchestrator.Engine) (orchestrator.Report, error) {
	rep := eng.Report()
	payload, err := json.Marshal(rep)
	if err != nil {
		return rep, err
	}

	sess.Stage = string(orchestrator.StageDone)
	sess.Status = store.StatusFinished
	sess.Recommendation = rep.Recommendation

	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	if err := s.cfg.Store.FinishSession(sctx, sess, store.Report{
		TenantID:       sess.TenantID,
		SessionID:      sess.ID,
		Recommendation: rep.Recommendation,
		Confidence:     rep.Confidence,
		Score:          rep.Score,
		Payload:        payload,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		return rep, fmt.Errorf("保存报告失败: %w", err)
	}

	// 面试正常结束: 清掉快照。快照是热路径缓存而不是数据来源,
	// 该过期就得过期, 否则峰值期会把 Redis 越堆越满。
	if s.cfg.Checkpoint != nil {
		_ = s.cfg.Checkpoint.Delete(ctx, sess.ID)
	}
	s.audit(ctx, store.AuditSessionFinish, sess.ID,
		fmt.Sprintf("recommendation=%s score=%d duration=%ds", rep.Recommendation, rep.Score, rep.DurationSec))
	s.metrics.SessionsFinished.WithLabelValues(sess.TenantID, rep.Recommendation).Inc()
	s.metrics.SessionDuration.WithLabelValues(sess.TenantID).Observe(float64(rep.DurationSec))
	// 把事实回流到招聘看板(不做自动淘汰, 见 syncPipeline 的说明)。
	s.syncPipeline(ctx, sess, rep.Score, rep.Recommendation)
	return rep, nil
}

type checkpointPayload struct {
	SessionID       string    `json:"session_id"`
	Stage           string    `json:"stage"`
	TurnCount       int       `json:"turn_count"`
	PendingQuestion string    `json:"pending_question"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (s *Server) saveCheckpoint(ctx context.Context, sess store.Session, stage orchestrator.Stage, turnCount int, pending string) {
	if s.cfg.Checkpoint == nil {
		return
	}
	raw, err := json.Marshal(checkpointPayload{
		SessionID: sess.ID, Stage: string(stage), TurnCount: turnCount,
		PendingQuestion: pending, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return
	}
	if err := s.cfg.Checkpoint.Save(ctx, sess.ID, raw, 24*time.Hour); err != nil {
		s.logger.WarnContext(ctx, "写会话快照失败", append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
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
		// 因为"静默当作没评分"会让重连后的报告少掉一整轮考察结果。
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

func isProbeID(id string) bool { return strings.Contains(id, ".p") }

func messageKind(mt int) string {
	if mt == websocket.BinaryMessage {
		return "binary"
	}
	return "text"
}

func boolLabel(b bool) string {
	if b {
		return "scored"
	}
	return "unscored"
}
