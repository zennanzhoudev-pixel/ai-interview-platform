package api

import (
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

func TestRecordingUploadPlaybackPermissionAndDelete(t *testing.T) {
	f := newPlatformFixture(t)
	jobID := f.createJob(t, "后端工程师")
	ref := f.createCandidate(t, false)
	appID := f.createApplication(t, jobID, ref)
	sessionID, token := f.scheduleAndStart(t, appID, 1)

	// 候选人用会话令牌上传分片, 不需要 API Key。
	for i, chunk := range []string{"chunk", "-0-", "end"} {
		req, err := http.NewRequest(http.MethodPost,
			f.ts.URL+"/api/v1/candidate/recording/chunks?kind=video&index="+itoa(i),
			strings.NewReader(chunk))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("上传分片失败: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 个分片上传应成功, 实际 %d", i, resp.StatusCode)
		}
	}

	code, body := f.do(t, http.MethodPost, "/api/v1/candidate/recording/finalize?token="+token, "",
		map[string]any{"kind": "video", "chunks": 3, "duration_ms": 60000})
	if code != http.StatusOK {
		t.Fatalf("合并录制失败: %d %v", code, body)
	}
	// "chunk" + "-0-" + "end" = 11 字节, 顺序必须与上传序号一致。
	if size, _ := body["size_bytes"].(float64); size != 11 {
		t.Fatalf("合并后大小应为 11, 实际 %v", body["size_bytes"])
	}

	// 面试官角色没有 recording:read —— 录像是比报告更敏感的数据。
	if code, _ := f.do(t, http.MethodGet, "/api/v1/sessions/"+sessionID+"/recordings", f.viewKey, nil); code != http.StatusForbidden {
		t.Fatalf("面试官不应能列出录像, 实际 %d", code)
	}
	code, body = f.do(t, http.MethodGet, "/api/v1/sessions/"+sessionID+"/recordings", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("管理员列出录像失败: %d %v", code, body)
	}
	if list, _ := body["recordings"].([]any); len(list) != 1 {
		t.Fatalf("应有 1 个录制件: %v", list)
	}

	// 回放支持 Range, 且播放行为必须留痕。
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/api/v1/sessions/"+sessionID+"/recordings/video", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	req.Header.Set("Range", "bytes=0-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应返回 206, 实际 %d", resp.StatusCode)
	}
	if string(data) != "chunk" {
		t.Fatalf("Range 内容不正确: %q", data)
	}
	code, body = f.do(t, http.MethodGet, "/api/v1/audit", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取审计失败: %d", code)
	}
	viewed := false
	for _, e := range body["entries"].([]any) {
		m, _ := e.(map[string]any)
		if m["action"] == store.AuditRecordingView {
			viewed = true
		}
	}
	if !viewed {
		t.Fatal("播放录像必须留痕")
	}

	if code, _ := f.do(t, http.MethodDelete, "/api/v1/sessions/"+sessionID+"/recordings/video", f.adminKey, nil); code != http.StatusOK {
		t.Fatalf("删除录像失败: %d", code)
	}
	code, body = f.do(t, http.MethodGet, "/api/v1/sessions/"+sessionID+"/recordings", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("再次列出录像失败: %d", code)
	}
	if list, _ := body["recordings"].([]any); len(list) != 0 {
		t.Fatalf("删除后不应还有录制件: %v", list)
	}
}

func TestRecordingRejectsUnknownKindAndMissingCredential(t *testing.T) {
	f := newPlatformFixture(t)
	code, body := f.do(t, http.MethodPost, "/api/v1/sessions", f.adminKey, map[string]any{
		"round": 1, "consent_recording": true, "consent_scoring": true,
	})
	if code != http.StatusOK {
		t.Fatalf("创建会话失败: %d %v", code, body)
	}
	token, _ := body["session_token"].(string)

	req, _ := http.NewRequest(http.MethodPost,
		f.ts.URL+"/api/v1/candidate/recording/chunks?kind=blob&index=0", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知录制类型应 400, 实际 %d", resp.StatusCode)
	}

	req2, _ := http.NewRequest(http.MethodPost,
		f.ts.URL+"/api/v1/candidate/recording/chunks?kind=video&index=0", strings.NewReader("x"))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭证上传应 401, 实际 %d", resp2.StatusCode)
	}
}

func TestCodeRunReportsIsolationAndJudgesCases(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("本机没有 python3, 跳过判题测试")
	}
	f := newPlatformFixture(t)

	code, body := f.do(t, http.MethodPost, "/api/v1/code/run", f.adminKey, map[string]any{
		"language": "python",
		"code":     "import sys\nprint(int(sys.stdin.read().strip()) * 2)\n",
		"test_cases": []map[string]any{
			{"name": "示例", "stdin": "2\n", "expected_stdout": "4"},
			{"name": "隐藏", "stdin": "5\n", "expected_stdout": "10", "hidden": true},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("判题失败: %d %v", code, body)
	}
	if body["passed"].(float64) != 2 || body["total"].(float64) != 2 {
		t.Fatalf("用例应全部通过: %v", body)
	}
	// 隔离等级必须如实回传, 否则调用方会把不隔离的执行当成等价结果。
	if body["isolated"] != false {
		t.Fatalf("本机沙箱应标注未隔离: %v", body["isolated"])
	}
	if w, _ := body["warning"].(string); w == "" {
		t.Fatal("未隔离时必须给出告警文案")
	}
	cases, _ := body["cases"].([]any)
	hidden, _ := cases[1].(map[string]any)
	if _, has := hidden["expected"]; has {
		t.Fatalf("隐藏用例不应回传期望输出: %v", hidden)
	}

	if code, _ := f.do(t, http.MethodPost, "/api/v1/code/run", f.adminKey,
		map[string]any{"language": "brainfuck", "code": "+"}); code != http.StatusBadRequest {
		t.Fatalf("未知语言应 400, 实际 %d", code)
	}
	many := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		many = append(many, map[string]any{"name": "c", "stdin": "1", "expected_stdout": "2"})
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/code/run", f.adminKey,
		map[string]any{"language": "python", "code": "print(2)", "test_cases": many}); code != http.StatusBadRequest {
		t.Fatalf("用例数超限应 400, 实际 %d", code)
	}
}

func TestObserverTicketRelaysSignalingAndRejectsRoleConfusion(t *testing.T) {
	f := newPlatformFixture(t)
	sessionID := f.createSession(t, f.adminKey)

	code, body := f.do(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/observer-ticket", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("签发旁听票据失败: %d %v", code, body)
	}
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatalf("响应缺少票据: %v", body)
	}
	if _, isList := body["ice_servers"].([]any); !isList {
		t.Fatalf("应返回 ICE 配置: %v", body["ice_servers"])
	}

	observer := dialWSWithTicket(t, f.ts, sessionID, ticket)
	joined := readMsg(t, observer)
	if joined["type"] != "observer_joined" {
		t.Fatalf("旁听席首帧应为 observer_joined, 实际 %v", joined["type"])
	}
	observerPeerID, _ := joined["peer_id"].(string)
	if observerPeerID == "" {
		t.Fatalf("缺少 peer_id: %v", joined)
	}

	// 候选人令牌不能当旁听票据用 —— 角色混淆会让候选人拿到信令通道。
	candidateToken := f.sessionToken(t, sessionID)
	if _, _, err := websocket.DefaultDialer.Dial(
		wsURL(f.ts, sessionID)+"?role=observer&ticket="+candidateToken, nil); err == nil {
		t.Fatal("候选人令牌不应能进入旁听席")
	}

	candidate := dialWSWithToken(t, f.ts, sessionID, candidateToken)
	if first := readMsg(t, candidate); first["type"] != "state" {
		t.Fatalf("候选人首帧应为 state, 实际 %v", first["type"])
	}
	_ = readMsg(t, candidate)

	if err := candidate.WriteJSON(map[string]any{
		"type": "webrtc.offer", "target_id": observerPeerID,
		"payload": map[string]any{"sdp": "v=0 fake"},
	}); err != nil {
		t.Fatalf("发送 offer 失败: %v", err)
	}
	// 旁听席会先收到"候选人进入房间", 再收到信令; 顺序不重要, 两者都必须到。
	sawJoin := false
	var offer map[string]any
	for i := 0; i < 4; i++ {
		msg := readMsg(t, observer)
		if msg["type"] == "peer_joined" && msg["role"] == "candidate" {
			sawJoin = true
			continue
		}
		if msg["type"] == "webrtc.offer" {
			offer = msg
			break
		}
	}
	if !sawJoin {
		t.Fatal("旁听席应收到候选人加入房间的通知")
	}
	if offer == nil {
		t.Fatal("旁听席应收到候选人发起的 offer")
	}
	if offer["from_role"] != "candidate" {
		t.Fatalf("信令应带来源角色: %v", offer)
	}

	// 目标不存在时要明确回告, 否则发起方会一直停在"连接中"。
	if err := candidate.WriteJSON(map[string]any{
		"type": "webrtc.ice", "target_id": "peer_not_exist",
		"payload": map[string]any{"candidate": "x"},
	}); err != nil {
		t.Fatalf("发送 ice 失败: %v", err)
	}
	if gone := readMsg(t, candidate); gone["type"] != "webrtc.peer_gone" {
		t.Fatalf("对端不存在时应返回 peer_gone, 实际 %v", gone["type"])
	}
}

func TestObserverTicketRequiresPermissionAndOwnership(t *testing.T) {
	f := newPlatformFixture(t)
	sessionID := f.createSession(t, f.adminKey)
	schedKey, _, err := f.keys.Add("tenant-a", "ATS", auth.RoleScheduler)
	if err != nil {
		t.Fatalf("签发密钥失败: %v", err)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/observer-ticket", schedKey, nil); code != http.StatusForbidden {
		t.Fatalf("排期账号应无旁听权限, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/observer-ticket", f.bKey, nil); code != http.StatusNotFound {
		t.Fatalf("跨租户应 404, 实际 %d", code)
	}
}

func TestSystemProvidersReportsHonestDegradation(t *testing.T) {
	f := newPlatformFixture(t)
	code, body := f.do(t, http.MethodGet, "/api/v1/system/providers", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("自检失败: %d %v", code, body)
	}
	sandboxInfo, _ := body["sandbox"].(map[string]any)
	if sandboxInfo["isolated"] != false {
		t.Fatalf("本机沙箱应标注未隔离: %v", sandboxInfo)
	}
	if _, hasNote := sandboxInfo["note"]; !hasNote {
		t.Fatal("未隔离时必须给出说明")
	}
	storeInfo, _ := body["store"].(map[string]any)
	if !strings.Contains(storeInfo["kind"].(string), "内存") {
		t.Fatalf("内存存储应如实标注: %v", storeInfo)
	}
	for _, p := range body["providers"].([]any) {
		m, _ := p.(map[string]any)
		if m["configured"] == false {
			if _, has := m["note"]; !has {
				t.Fatalf("未配置的上游要给出降级说明: %v", m)
			}
		}
	}
	knowledgeInfo, _ := body["knowledge"].(map[string]any)
	if knowledgeInfo["ready"] != true {
		t.Fatalf("知识库应可用: %v", knowledgeInfo)
	}
}
