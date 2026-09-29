package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 面试间的实时房间。
//
// 1 面到 5 面里, 越往后人类面试官的参与越重(见产品里的轮次表)。
// 所以在"候选人 + AI"这条主线之外, 还需要一条**人类面试官随时进房间**的
// 支线: 旁听、接管提问、与候选人视频。这条支线的技术形态有三种可选:
//
//  1. 服务端 SFU 混流 —— 最强, 也最贵: 要引入媒体服务器, 带宽与运维成本
//     都是另一个量级, 而面试间的并发数是"几十路"而不是"几十万路";
//  2. 端到端 WebRTC(P2P) —— 媒体不过服务端, 服务端只转发 SDP/ICE 信令。
//     延迟最低、成本最低, 代价是 NAT 穿透失败时需要 TURN 兜底;
//  3. 只录不连 —— 最便宜, 但人类面试官无法实时参与, 3 面以上不成立。
//
// 这里选 2: AI 面试官本身是一条独立的音频流(WebSocket + PCM),
// 人类面试官与候选人之间用 P2P 视频, 两者互不干扰。信令复用已经存在的
// 面试 WebSocket, 因此不需要新增任何端口、连接或部署组件。
//
// 服务端在这个模型里的职责被压到最小: **只做信令转发与在场状态**,
// 不碰媒体内容。这既是成本考虑, 也是隐私考虑 —— 服务端看不到的画面
// 就永远不会泄漏。

// peer 是房间里的一个连接。
type peer struct {
	id     string
	role   string
	writer *connWriter
}

// roomHub 维护"会话 -> 参与者"的映射。
type roomHub struct {
	mu    sync.RWMutex
	rooms map[string]map[string]*peer
	// onJoin/onLeave 用于打点与审计, 由外部注入, 避免 hub 依赖 metrics 结构。
	onJoin  func(sessionID, role string)
	onLeave func(sessionID, role string)
}

func newRoomHub() *roomHub {
	return &roomHub{rooms: make(map[string]map[string]*peer)}
}

// join 把一个连接加入房间。
func (h *roomHub) join(sessionID string, p *peer) {
	h.mu.Lock()
	if h.rooms[sessionID] == nil {
		h.rooms[sessionID] = make(map[string]*peer)
	}
	h.rooms[sessionID][p.id] = p
	h.mu.Unlock()
	if h.onJoin != nil {
		h.onJoin(sessionID, p.role)
	}
}

// leave 移除连接, 并顺手清掉空房间。
//
// 不清理空房间的后果不是"内存慢慢涨", 而是秋招期间几万个空 map 常驻 ——
// 这类泄漏在压测中看不出来, 因为它只在"人来人往"之后才显形。
func (h *roomHub) leave(sessionID, id string) {
	h.mu.Lock()
	room := h.rooms[sessionID]
	var role string
	if room != nil {
		if p, ok := room[id]; ok {
			role = p.role
			delete(room, id)
		}
		if len(room) == 0 {
			delete(h.rooms, sessionID)
		}
	}
	h.mu.Unlock()
	if role != "" && h.onLeave != nil {
		h.onLeave(sessionID, role)
	}
}

// peers 返回房间内的参与者快照。
func (h *roomHub) peers(sessionID string) []peer {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]peer, 0, len(h.rooms[sessionID]))
	for _, p := range h.rooms[sessionID] {
		out = append(out, *p)
	}
	return out
}

// count 返回某角色的在场数。
func (h *roomHub) count(sessionID, role string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, p := range h.rooms[sessionID] {
		if p.role == role {
			n++
		}
	}
	return n
}

// toPeer 把消息转发给房间内指定 id 的参与者。
func (h *roomHub) toPeer(sessionID, targetID string, payload any) bool {
	h.mu.RLock()
	var target *peer
	if room := h.rooms[sessionID]; room != nil {
		if p, ok := room[targetID]; ok {
			target = p
		}
	}
	h.mu.RUnlock()
	if target == nil {
		return false
	}
	target.writer.writeJSON(payload)
	return true
}

// broadcast 把消息发给房间里除 exceptID 之外的所有人。
func (h *roomHub) broadcast(sessionID string, payload any, exceptID string) {
	for _, p := range h.peers(sessionID) {
		if p.id == exceptID {
			continue
		}
		p.writer.writeJSON(payload)
	}
}

/* ---------------- 信令处理 ---------------- */

// signalMessage 是 WebRTC 信令消息。
//
// 服务端不解析 SDP/ICE 的内容: 它只是"一个能保证投递的通道"。
// 解析媒体协商内容意味着跟随 WebRTC 规范演进, 而这对我们没有收益 ——
// 我们要保证的是"谁在房间里"和"消息送达", 不是"这次协商是否优雅"。
type signalMessage struct {
	Type     string          `json:"type"`
	TargetID string          `json:"target_id,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// handleSignal 处理来自面试间任一连接的信令消息。
func (s *Server) handleSignal(ctx context.Context, sessionID, selfID, selfRole string, raw []byte) bool {
	var msg signalMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}
	switch msg.Type {
	case "webrtc.offer", "webrtc.answer", "webrtc.ice", "webrtc.bye":
	default:
		return false
	}
	s.metrics.WebRTCSig.WithLabelValues(strings.TrimPrefix(msg.Type, "webrtc.")).Inc()

	forward := map[string]any{
		"type":      msg.Type,
		"from_id":   selfID,
		"from_role": selfRole,
		"payload":   msg.Payload,
	}
	if msg.TargetID != "" {
		if !s.rooms.toPeer(sessionID, msg.TargetID, forward) {
			// 目标已离线: 明确告知, 否则发起方会一直等在"connecting"状态,
			// 而界面上看起来只是"视频没连上"。
			s.rooms.toPeer(sessionID, selfID, map[string]any{
				"type": "webrtc.peer_gone", "target_id": msg.TargetID,
			})
		}
		return true
	}
	s.rooms.broadcast(sessionID, forward, selfID)
	return true
}

/* ---------------- 人类面试官旁听席 ---------------- */

// handleObserverTicket 用 API Key 换一张旁听票据。
//
// 为什么不直接把 API Key 放进 WebSocket URL: 浏览器 WebSocket 无法自定义
// 请求头, 密钥只能进 URL, 而 URL 会进访问日志、浏览器历史与 Referer。
// 票据只对一场面试、一个角色、两小时内有效, 泄漏代价被压到最小。
func (s *Server) handleObserverTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant := platform.Tenant(ctx)
	sessionID := r.PathValue("id")

	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	if _, err := s.cfg.Store.GetSession(sctx, tenant, sessionID); err != nil {
		s.storeErr(w, err, "get_session")
		return
	}

	expiresAt := time.Now().Add(s.cfg.ObserverTicketTTL)
	ticket, err := auth.IssueSessionToken(s.cfg.Secret, auth.SessionToken{
		SessionID: sessionID, TenantID: tenant, ExpiresAt: expiresAt, Role: auth.TokenRoleObserver,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "签发旁听票据失败")
		return
	}
	s.audit(ctx, store.AuditObserverJoin, sessionID, "签发旁听票据")
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":      ticket,
		"expires_at":  expiresAt,
		"ws_url":      "/ws/interview/" + sessionID + "?role=observer&ticket=" + ticket,
		"ice_servers": s.iceServers(),
	})
}

// handleObserver 是人类面试官的实时通道。
//
// 它做三件事, 按重要性排序:
//  1. 把候选人端的问答事件镜像给面试官(实时旁听);
//  2. 转发 WebRTC 信令, 让面试官与候选人建立点对点视频;
//  3. 记录"谁进过房间"。
//
// 它刻意**不能**替候选人作答: 处理 answer/question 的只有候选人那条连接。
// 人类面试官的介入方式是通过视频/语音与候选人交流, 而不是冒充 AI 提交答案 ——
// 后者会污染评分证据链。
func (s *Server) handleObserver(w http.ResponseWriter, r *http.Request, sess store.Session) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	writer := &connWriter{conn: conn}
	selfID := newID("peer")
	s.rooms.join(sess.ID, &peer{id: selfID, role: "observer", writer: writer})
	defer s.rooms.leave(sess.ID, selfID)

	conn.SetReadLimit(wsReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	stopPing := s.startKeepalive(r.Context(), writer)
	defer stopPing()

	s.metrics.WSConnections.Inc()
	defer s.metrics.WSConnections.Dec()

	writer.writeJSON(map[string]any{
		"type":        "observer_joined",
		"peer_id":     selfID,
		"session_id":  sess.ID,
		"round":       sess.Round,
		"stage":       sess.Stage,
		"peers":       s.peerRoster(sess.ID),
		"ice_servers": s.iceServers(),
	})
	// 让候选人知道有人进来了 —— 候选人有权知道谁在听这场面试。
	s.rooms.broadcast(sess.ID, map[string]any{
		"type": "peer_joined", "peer_id": selfID, "role": "observer",
		"peers": s.peerRoster(sess.ID),
	}, selfID)

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.metrics.WSMessages.WithLabelValues(messageKind(mt)).Inc()
		if mt != websocket.TextMessage {
			continue
		}
		if s.handleSignal(r.Context(), sess.ID, selfID, "observer", data) {
			continue
		}
		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "ping":
			writer.writeJSON(map[string]any{"type": "pong"})
		case "roster":
			writer.writeJSON(map[string]any{"type": "roster", "peers": s.peerRoster(sess.ID)})
		}
	}
}

// peerRoster 返回房间的在场名单。
func (s *Server) peerRoster(sessionID string) []map[string]string {
	peers := s.rooms.peers(sessionID)
	out := make([]map[string]string, 0, len(peers))
	for _, p := range peers {
		out = append(out, map[string]string{"peer_id": p.id, "role": p.role})
	}
	return out
}

// iceServers 返回 WebRTC 的 ICE 服务器配置。
//
// STUN 只解决"双方能看见彼此", TURN 解决"实在连不上时要不要中继"。
// 两者都是部署配置, 不写死在代码里 —— 内网部署的 STUN 地址与公网完全不同。
func (s *Server) iceServers() []map[string]any {
	if len(s.cfg.ICEServers) > 0 {
		return s.cfg.ICEServers
	}
	return []map[string]any{{"urls": []string{"stun:stun.l.google.com:19302"}}}
}

// logPeerEvent 把进出房间写进日志(便于与审计日志对照)。
func (s *Server) logPeerEvent(ctx context.Context, sessionID, role, event string) {
	s.logger.InfoContext(ctx, "面试间在场变化",
		append(platform.AuditAttrs(ctx),
			slog.String("session_id", sessionID),
			slog.String("role", role),
			slog.String("event", event))...)
}
