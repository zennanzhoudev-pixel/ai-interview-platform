package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/practice"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 真人双向对练的接入层。
//
// 与 AI 面试最大的不同: 这里没有状态机, 服务端**不驱动流程** ——
// 两个真人自己聊。服务端只做三件事: 配对、把双方的 WebRTC 信令互相转发、
// 以及留下"谁和谁对练过"的记录。
//
// 之所以不做"服务端混流"或"录制": 对练是练习场景, 双方明确知道对方是谁,
// 记录一次"发生过"就够; 把画面收进服务端只会增加成本与隐私面。

func (s *Server) practiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, practice.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "房间不存在或已过期")
	case errors.Is(err, practice.ErrNotMember):
		writeError(w, http.StatusForbidden, "你不是这个房间的参与者")
	case errors.Is(err, practice.ErrInvalidRole):
		writeError(w, http.StatusBadRequest, "角色只能是 interviewer / interviewee / any")
	default:
		writeError(w, http.StatusInternalServerError, "对练操作失败")
	}
}

// handlePracticeJoin 加入配对。
func (s *Server) handlePracticeJoin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "对练功能未启用")
		return
	}
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	var req struct {
		Topic string `json:"topic"`
		Role  string `json:"role"`
	}
	if !decodeBody(w, r, 32<<10, &req) {
		return
	}
	role := practice.Role(strings.TrimSpace(req.Role))
	if role == "" {
		role = practice.RoleAny
	}
	room, mine, err := s.cfg.Practice.Join(user.TenantID, user.ID, user.Name, req.Topic, role)
	if err != nil {
		s.practiceError(w, err)
		return
	}
	paired := room.State == practice.StatePaired
	if paired {
		s.audit(r.Context(), store.AuditPracticeJoin, room.ID,
			"与 "+peerName(room, user.ID)+" 配对成功, 主题: "+room.Topic)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room":        room,
		"my_role":     mine,
		"paired":      paired,
		"ws_url":      "/ws/practice/" + room.ID,
		"ice_servers": s.iceServers(),
		"note":        "等待配对时不要关闭页面; 配对成功后双方会各自看到对方的视频。",
	})
}

// handlePracticeRoom 读取房间状态(前端用它轮询配对结果)。
func (s *Server) handlePracticeRoom(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "对练功能未启用")
		return
	}
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	room, err := s.cfg.Practice.Get(user.TenantID, r.PathValue("id"))
	if err != nil {
		s.practiceError(w, err)
		return
	}
	// 只有参与者能看房间详情 —— 否则任何人都能列出"谁在和谁对练"。
	if !room.Has(user.ID) {
		writeError(w, http.StatusForbidden, "你不是这个房间的参与者")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room": room, "ws_url": "/ws/practice/" + room.ID, "ice_servers": s.iceServers(),
	})
}

// handlePracticeLeave 退出对练。
func (s *Server) handlePracticeLeave(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "对练功能未启用")
		return
	}
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	var req struct {
		RoomID string `json:"room_id"`
	}
	if !decodeBody(w, r, 16<<10, &req) {
		return
	}
	room, ended, err := s.cfg.Practice.Leave(user.TenantID, req.RoomID, user.ID)
	if err != nil {
		s.practiceError(w, err)
		return
	}
	if ended {
		// 对练记录落审计: 这是"谁和谁练过"的唯一留痕。
		s.audit(r.Context(), store.AuditPracticeEnd, room.ID,
			"对练结束, 主题: "+room.Topic+", 参与人: "+participantNames(room))
	}
	writeJSON(w, http.StatusOK, map[string]any{"room": room, "ended": ended})
}

// handlePracticeRooms 列出本租户的对练房(工作台与候选人空间都用)。
func (s *Server) handlePracticeRooms(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "对练功能未启用")
		return
	}
	tenant := platform.Tenant(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"rooms": s.cfg.Practice.List(tenant),
		"stats": s.cfg.Practice.Stats(tenant),
	})
}

// handlePracticeSocket 是对练房间的实时通道。
//
// 鉴权用登录会话(Cookie): 浏览器在 WebSocket 握手时会自动带上同源 Cookie,
// 因此不需要像候选人那样把令牌放进 URL(那会把凭证写进访问日志与浏览器历史)。
func (s *Server) handlePracticeSocket(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "对练功能未启用")
		return
	}
	user, err := s.currentAccount(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	roomID := r.PathValue("id")
	room, err := s.cfg.Practice.Get(user.TenantID, roomID)
	if err != nil {
		s.practiceError(w, err)
		return
	}
	if !room.Has(user.ID) {
		writeError(w, http.StatusForbidden, "你不是这个房间的参与者")
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	writer := &connWriter{conn: conn}
	// 复用同一个房间表, 但键加前缀: 对练房与面试会话是两类房间,
	// 用前缀分开可以避免"面试会话 ID 与对练房 ID 撞名"这种理论上的错配。
	hubKey := "practice:" + roomID
	selfID := "peer_" + user.ID
	s.rooms.join(hubKey, &peer{id: selfID, role: string(roleOf(room, user.ID)), writer: writer})
	defer s.rooms.leave(hubKey, selfID)

	conn.SetReadLimit(wsReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	stopPing := s.startKeepalive(r.Context(), writer)
	defer stopPing()

	s.metrics.WSConnections.Inc()
	defer s.metrics.WSConnections.Dec()

	mine, _ := room.Participant(user.ID)
	peerInfo, _ := room.Peer(user.ID)
	writer.writeJSON(map[string]any{
		"type":        "practice_joined",
		"room_id":     room.ID,
		"topic":       room.Topic,
		"peer_id":     selfID,
		"my_role":     mine.Role,
		"my_name":     mine.Name,
		"peer":        peerInfo,
		"state":       room.State,
		"ice_servers": s.iceServers(),
		"peers":       s.peerRoster(hubKey),
	})
	// 通知对方"人到位了": 双方都要能主动发起 WebRTC 连接,
	// 因此进来时互相广播一次在场名单。
	s.rooms.broadcast(hubKey, map[string]any{
		"type": "practice_peer_joined", "peer_id": selfID,
		"role": string(mine.Role), "name": mine.Name, "peers": s.peerRoster(hubKey),
	}, selfID)

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			// 断开时通知对方: 让对面的界面立刻显示"对方已离开",
			// 而不是继续对着一个黑屏等待。
			s.rooms.broadcast(hubKey, map[string]any{
				"type": "practice_peer_left", "peer_id": selfID, "peers": s.peerRoster(hubKey),
			}, selfID)
			return
		}
		s.metrics.WSMessages.WithLabelValues(messageKind(mt)).Inc()
		if mt != websocket.TextMessage {
			continue
		}
		if s.handleSignal(r.Context(), hubKey, selfID, string(mine.Role), data) {
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
			writer.writeJSON(map[string]any{"type": "roster", "peers": s.peerRoster(hubKey)})
		}
	}
}

// practiceInfoFor 返回对练能力概况(自检页用)。
func (s *Server) practiceInfoFor(tenant string) map[string]any {
	if s.cfg.Practice == nil {
		return map[string]any{"enabled": false, "note": "对练功能未启用"}
	}
	return map[string]any{
		"enabled": true,
		"stats":   s.cfg.Practice.Stats(tenant),
		"note":    "配对状态放在内存里(秒级临时状态); 对练开始与结束会写入审计日志。多副本部署时需要把等待队列换成 Redis 并加锁。",
	}
}

func roleOf(room practice.Room, userID string) practice.Role {
	if p, ok := room.Participant(userID); ok {
		return p.Role
	}
	return practice.RoleAny
}

func peerName(room practice.Room, userID string) string {
	if p, ok := room.Peer(userID); ok {
		return p.Name
	}
	return "对方"
}

func participantNames(room practice.Room) string {
	names := make([]string, 0, len(room.Participants))
	for _, p := range room.Participants {
		names = append(names, string(p.Role)+":"+p.Name)
	}
	return strings.Join(names, ", ")
}
