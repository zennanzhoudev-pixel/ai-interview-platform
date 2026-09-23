package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 一份"什么要点都提到了"的回答, 让测试聚焦在链路上而不是评分逻辑上。
const richAnswer = "用了 ZSet 和 score 存权重, 内存做过估算, 排序和 70% 的收益都验证过; " +
	"幂等靠唯一键去重, 状态机保证最终一致; 令牌桶和滑动窗口都考虑过, 容量估算配了降级预案; " +
	"三色标记加写屏障, 本地队列和抢占用上了用户态的优势, 冷启动靠编译产物复用。"

func newTestServer(t *testing.T) (*httptest.Server, store.SessionStore) {
	t.Helper()
	st := store.NewMemoryStore()
	srv := NewServer(Config{Store: st})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func createSession(t *testing.T, base string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(base+"/api/v1/sessions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("创建会话请求失败: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func dialWS(t *testing.T, ts *httptest.Server, sessionID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/interview/" + sessionID
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("连接 WebSocket 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readMsg(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	return msg
}

// answerUntilReport 一直答题直到拿到报告, 返回报告负载。
func answerUntilReport(t *testing.T, conn *websocket.Conn, maxTurns int) map[string]any {
	t.Helper()
	for i := 0; i < maxTurns; i++ {
		if err := conn.WriteJSON(map[string]any{"type": "answer", "text": richAnswer}); err != nil {
			t.Fatalf("发送回答失败: %v", err)
		}
		msg := readMsg(t, conn)
		if msg["type"] == "report" {
			return msg["payload"].(map[string]any)
		}
		if msg["type"] != "turn_result" {
			t.Fatalf("第 %d 轮应先收到 turn_result, 实际 %v", i+1, msg["type"])
		}
		next := readMsg(t, conn)
		if next["type"] == "report" {
			return next["payload"].(map[string]any)
		}
		if next["type"] != "question" {
			t.Fatalf("第 %d 轮之后应收到 question, 实际 %v", i+1, next["type"])
		}
	}
	t.Fatalf("答了 %d 轮仍未结束", maxTurns)
	return nil
}

func TestHealthEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查应返回 200, 实际 %d", resp.StatusCode)
	}
}

// 前端可以被绕过, 法律风险不会: 未取得同意时服务端必须拒绝开工。
func TestCreateSessionRequiresConsent(t *testing.T) {
	ts, st := newTestServer(t)

	status, body := createSession(t, ts.URL, map[string]any{
		"round": 1, "minutes": 30, "consent_recording": false,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("缺少同意应返回 400, 实际 %d", status)
	}
	if body["error"] == nil {
		t.Fatal("错误响应应包含原因")
	}

	sessions, _ := st.ListSessions(context.Background(), "", 10)
	if len(sessions) != 0 {
		t.Fatalf("未同意的请求不应创建会话, 实际 %d 条", len(sessions))
	}
}

func TestCreateSessionRecordsConsentEvidence(t *testing.T) {
	ts, st := newTestServer(t)

	status, body := createSession(t, ts.URL, map[string]any{
		"round": 3, "minutes": 40, "candidate_id": "c_1001",
		"consent_recording": true,
	})
	if status != http.StatusOK {
		t.Fatalf("创建会话应返回 200, 实际 %d (%v)", status, body)
	}
	id, _ := body["session_id"].(string)
	if id == "" {
		t.Fatal("响应里应包含 session_id")
	}

	consents, err := st.ListConsents(context.Background(), id)
	if err != nil {
		t.Fatalf("读取授权失败: %v", err)
	}
	if len(consents) != 2 {
		t.Fatalf("应记录 recording 与 scoring 两项授权, 实际 %d 条", len(consents))
	}
	for _, c := range consents {
		if c.AgreedAt.IsZero() || c.IP == "" {
			t.Fatalf("授权留痕不完整: %+v", c)
		}
	}
}

func TestCreateSessionValidatesRound(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, round := range []int{0, 6, -1} {
		status, _ := createSession(t, ts.URL, map[string]any{
			"round": round, "consent_recording": true,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("轮次 %d 应被拒绝, 实际状态码 %d", round, status)
		}
	}
}

func TestStaticFrontendIsServed(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取首页失败: %v", err)
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "AI Interview OS") {
		t.Fatal("首页应返回面试前端页面")
	}
}

func TestReportEndpointReturns404BeforeFinish(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := createSession(t, ts.URL, map[string]any{"round": 1, "consent_recording": true})
	id := body["session_id"].(string)

	resp, err := http.Get(ts.URL + "/api/v1/sessions/" + id + "/report")
	if err != nil {
		t.Fatalf("请求报告失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未结束的会话应返回 404, 实际 %d", resp.StatusCode)
	}
}

func TestInterviewWebSocketRunsFullSessionAndPersists(t *testing.T) {
	ts, st := newTestServer(t)
	_, body := createSession(t, ts.URL, map[string]any{
		"round": 1, "minutes": 45, "candidate_id": "c_1001", "consent_recording": true,
	})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	state := readMsg(t, conn)
	if state["type"] != "state" || state["resumed"] != false {
		t.Fatalf("首个消息应为未恢复的 state, 实际 %v", state)
	}
	first := readMsg(t, conn)
	if first["type"] != "question" || first["text"] == "" {
		t.Fatalf("应推送首个问题, 实际 %v", first)
	}

	report := answerUntilReport(t, conn, 60)
	if report["recommendation"] == nil {
		t.Fatalf("报告应包含结论: %v", report)
	}
	dims, _ := report["dimensions"].([]any)
	if len(dims) == 0 {
		t.Fatal("报告应包含能力维度")
	}
	stats, _ := report["stats"].(map[string]any)
	if stats["max_probe_depth"].(float64) > 2 {
		t.Fatalf("追问深度超过上限: %v", stats["max_probe_depth"])
	}

	ctx := context.Background()
	sess, err := st.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if sess.Status != store.StatusFinished {
		t.Fatalf("面试结束后会话状态应为 finished, 实际 %q", sess.Status)
	}
	if sess.Recommendation == "" {
		t.Fatal("会话应记录 AI 建议结论")
	}

	turns, _ := st.ListTurns(ctx, id)
	if len(turns) < 5 {
		t.Fatalf("应落库多轮问答, 实际 %d 轮", len(turns))
	}
	for i, tn := range turns {
		if tn.Index != i+1 {
			t.Fatalf("问答序号应连续递增: 第 %d 位是 %d", i, tn.Index)
		}
		if tn.Scored && len(tn.Verdict) == 0 {
			t.Fatalf("第 %d 轮标记载了评分却没有评分结论", tn.Index)
		}
	}

	if _, err := st.GetReport(ctx, id); err != nil {
		t.Fatalf("应保存报告: %v", err)
	}
}

// 断线重连: 断开后重新连上, 必须从断点继续, 而不是从头再问一遍。
func TestInterviewResumesAfterDisconnect(t *testing.T) {
	ts, st := newTestServer(t)
	_, body := createSession(t, ts.URL, map[string]any{"round": 1, "minutes": 45, "consent_recording": true})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	_ = readMsg(t, conn) // state
	first := readMsg(t, conn)

	for i := 0; i < 2; i++ {
		if err := conn.WriteJSON(map[string]any{"type": "answer", "text": richAnswer}); err != nil {
			t.Fatalf("发送回答失败: %v", err)
		}
		_ = readMsg(t, conn) // turn_result
		_ = readMsg(t, conn) // 下一题
	}
	_ = conn.Close()

	// 服务端不保存内存态: 重放完全依赖已落库的问答记录。
	turns, err := st.ListTurns(context.Background(), id)
	if err != nil || len(turns) != 2 {
		t.Fatalf("断开前应落库 2 轮问答, 实际 %d 轮 (err=%v)", len(turns), err)
	}

	conn2 := dialWS(t, ts, id)
	state := readMsg(t, conn2)
	if state["resumed"] != true {
		t.Fatalf("重连后应标记为已恢复, 实际 %v", state)
	}
	if state["turn_count"].(float64) != 2 {
		t.Fatalf("重连后应知道已完成 2 轮, 实际 %v", state["turn_count"])
	}

	resumed := readMsg(t, conn2)
	if resumed["type"] != "question" {
		t.Fatalf("重连后应继续追问, 实际 %v", resumed)
	}
	if resumed["id"] == first["id"] && state["turn_count"].(float64) == 0 {
		t.Fatal("重连后不应从第一题重新开始")
	}

	// 继续答完, 报告应当包含全部轮次(而不是只剩下重连之后的)
	report := answerUntilReport(t, conn2, 60)
	stats, _ := report["stats"].(map[string]any)
	if stats["turns"].(float64) <= 2 {
		t.Fatalf("报告应包含重连前的轮次, 实际 %v 轮", stats["turns"])
	}
}

// fakeCheckpoint 记录快照读写, 用来验证 Redis 路径确实被走到。
type fakeCheckpoint struct {
	mu      sync.Mutex
	saved   map[string][]byte
	deleted []string
}

func newFakeCheckpoint() *fakeCheckpoint {
	return &fakeCheckpoint{saved: make(map[string][]byte)}
}

func (f *fakeCheckpoint) Save(_ context.Context, id string, payload []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[id] = payload
	return nil
}

func (f *fakeCheckpoint) Load(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, ok := f.saved[id]; ok {
		return data, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCheckpoint) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCheckpointWrittenDuringInterviewAndClearedAtEnd(t *testing.T) {
	st := store.NewMemoryStore()
	ckpt := newFakeCheckpoint()
	ts := httptest.NewServer(NewServer(Config{Store: st, Checkpoint: ckpt}).Handler())
	defer ts.Close()

	_, body := createSession(t, ts.URL, map[string]any{"round": 1, "minutes": 45, "consent_recording": true})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	_ = readMsg(t, conn)
	_ = readMsg(t, conn)
	_ = answerUntilReport(t, conn, 60)

	ckpt.mu.Lock()
	defer ckpt.mu.Unlock()
	if len(ckpt.saved) == 0 {
		t.Fatal("面试过程中应写入会话快照")
	}
	var found bool
	for _, deleted := range ckpt.deleted {
		if deleted == id {
			found = true
		}
	}
	if !found {
		t.Fatal("面试正常结束后应清理快照, 否则峰值期会把 Redis 越堆越满")
	}
}

func TestReconnectingToFinishedSessionReturnsReport(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := createSession(t, ts.URL, map[string]any{"round": 1, "minutes": 45, "consent_recording": true})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	_ = readMsg(t, conn)
	_ = readMsg(t, conn)
	_ = answerUntilReport(t, conn, 60)

	// 重新连到已经结束的会话: 应当直接拿到报告, 而不是重开一场
	conn2 := dialWS(t, ts, id)
	msg := readMsg(t, conn2)
	if msg["type"] != "report" {
		t.Fatalf("已结束的会话重连应返回报告, 实际 %v", msg["type"])
	}
}

func TestUnknownSessionReturns404(t *testing.T) {
	ts, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/interview/not-exist"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("不存在的会话不应允许建立 WebSocket 连接")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("应返回 404, 实际 %v", resp)
	}
}
