package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

type authFixture struct {
	ts       *httptest.Server
	store    store.SessionStore
	keys     *auth.MemoryKeyStore
	tenantA  string
	tenantB  string
	adminKey string
	viewKey  string
	bKey     string
}

// newAuthFixture 起一个开启了鉴权的服务, 并准备三个不同权限的凭据。
func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	st := store.NewMemoryStore()
	ks := auth.NewMemoryKeyStore()

	adminKey, _, err := ks.Add("tenant-a", "租户A管理员", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("签发管理员密钥失败: %v", err)
	}
	viewKey, _, err := ks.Add("tenant-a", "租户A面试官", auth.RoleInterviewer)
	if err != nil {
		t.Fatalf("签发面试官密钥失败: %v", err)
	}
	bKey, _, err := ks.Add("tenant-b", "租户B管理员", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("签发租户B密钥失败: %v", err)
	}

	srv := NewServer(Config{
		Store:       st,
		Keys:        ks,
		Secret:      []byte("test-secret"),
		RequireAuth: true,
		TenantID:    "tenant-a",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &authFixture{
		ts: ts, store: st, keys: ks,
		tenantA: "tenant-a", tenantB: "tenant-b",
		adminKey: adminKey, viewKey: viewKey, bKey: bKey,
	}
}

func (f *authFixture) do(t *testing.T, method, path, apiKey string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *authFixture) createSession(t *testing.T, apiKey string) string {
	t.Helper()
	code, body := f.do(t, http.MethodPost, "/api/v1/sessions", apiKey, map[string]any{
		"round": 1, "minutes": 30, "candidate_id": "13800138000",
		"candidate_name": "陈雨", "position": "高级后端工程师",
		"consent_recording": true, "consent_scoring": true,
	})
	if code != http.StatusOK {
		t.Fatalf("创建会话失败: %d %v", code, body)
	}
	id, _ := body["session_id"].(string)
	if id == "" {
		t.Fatalf("响应缺少 session_id: %v", body)
	}
	return id
}

// 管理接口没有凭据时必须 401 —— 这是整个系统最重要的一条边界。
func TestManagementEndpointsRequireAPIKey(t *testing.T) {
	f := newAuthFixture(t)

	for _, path := range []string{"/api/v1/sessions", "/api/v1/audit", "/api/v1/analytics/overview"} {
		if code, _ := f.do(t, http.MethodGet, path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("%s 无凭据应返回 401, 实际 %d", path, code)
		}
		if code, _ := f.do(t, http.MethodGet, path, "ik_伪造的密钥", nil); code != http.StatusUnauthorized {
			t.Errorf("%s 伪造凭据应返回 401, 实际 %d", path, code)
		}
	}

	// 运维端点不需要业务鉴权(由部署层限制来源)。
	resp, err := http.Get(f.ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查应返回 200, 实际 %d", resp.StatusCode)
	}
}

// 角色权限必须真正生效, 而不是只写在文档里。
func TestRolePermissionsEnforced(t *testing.T) {
	f := newAuthFixture(t)

	// 面试官: 能看报告、能改分, 但不能管密钥、不能读审计之外的管理接口
	if code, _ := f.do(t, http.MethodGet, "/api/v1/keys", f.viewKey, nil); code != http.StatusForbidden {
		t.Fatalf("面试官不应能列举密钥, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/audit", f.viewKey, nil); code != http.StatusOK {
		t.Fatalf("面试官应能读审计, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions", f.viewKey, map[string]any{
		"round": 1, "consent_recording": true, "consent_scoring": true,
	}); code != http.StatusForbidden {
		t.Fatalf("面试官不应能创建面试, 实际 %d", code)
	}
}

// 租户 A 的凭据读不到租户 B 的任何数据。
func TestTenantIsolationAcrossHTTP(t *testing.T) {
	f := newAuthFixture(t)
	idB := f.createSession(t, f.bKey)

	for _, path := range []string{
		"/api/v1/sessions/" + idB,
		"/api/v1/sessions/" + idB + "/report",
		"/api/v1/sessions/" + idB + "/consents",
	} {
		if code, _ := f.do(t, http.MethodGet, path, f.adminKey, nil); code != http.StatusNotFound {
			t.Errorf("跨租户访问 %s 应返回 404, 实际 %d", path, code)
		}
	}

	// 列表也必须互不可见。
	code, body := f.do(t, http.MethodGet, "/api/v1/sessions", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("列表失败: %d", code)
	}
	if sessions, ok := body["sessions"].([]any); ok && len(sessions) != 0 {
		t.Fatalf("租户 A 的列表不应包含租户 B 的会话: %v", sessions)
	}
}

// 候选人令牌只能访问自己那一场面试。
func TestCandidateTokenScopedToItsSession(t *testing.T) {
	f := newAuthFixture(t)

	code, body := f.do(t, http.MethodPost, "/api/v1/sessions", f.adminKey, map[string]any{
		"round": 1, "minutes": 30, "consent_recording": true, "consent_scoring": true,
	})
	if code != http.StatusOK {
		t.Fatalf("创建会话失败: %d", code)
	}
	id1, _ := body["session_id"].(string)
	token1, _ := body["session_token"].(string)
	if token1 == "" {
		t.Fatal("创建会话必须返回候选人令牌")
	}

	id2 := f.createSession(t, f.adminKey)

	// 自己的会话: 可以用令牌读
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/api/v1/candidate/session", nil)
	req.Header.Set("Authorization", "Bearer "+token1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("候选人请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("候选人应能读自己的会话, 实际 %d", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["session_id"] != id1 {
		t.Fatalf("返回了错误的会话: %v", got["session_id"])
	}
	// 内部字段不应暴露给候选人
	for _, leaked := range []string{"tenant_id", "candidate_ref"} {
		if _, ok := got[leaked]; ok {
			t.Fatalf("候选人响应不应包含内部字段 %s", leaked)
		}
	}

	// 拿自己的令牌连别人的会话: 必须被拒绝。
	wsURL := "ws" + strings.TrimPrefix(f.ts.URL, "http") + "/ws/interview/" + id2 + "?token=" + token1
	_, wsResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("令牌与会话不匹配时必须拒绝建立连接")
	}
	if wsResp != nil && wsResp.StatusCode != http.StatusForbidden {
		t.Fatalf("越权连接应返回 403, 实际 %d", wsResp.StatusCode)
	}

	// 没有令牌同样不行。
	wsURL2 := "ws" + strings.TrimPrefix(f.ts.URL, "http") + "/ws/interview/" + id1
	if _, _, err := websocket.DefaultDialer.Dial(wsURL2, nil); err == nil {
		t.Fatal("没有凭证时不应允许建立 WebSocket 连接")
	}
}

// 审计日志必须记录关键操作。
func TestAuditTrailCoversCriticalActions(t *testing.T) {
	f := newAuthFixture(t)
	id := f.createSession(t, f.adminKey)

	if code, _ := f.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/report", f.adminKey, nil); code != http.StatusNotFound {
		t.Fatalf("未生成报告时应 404, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/override", f.adminKey, map[string]any{
		"recommendation": "HIRE", "comment": "人工复核后通过",
	}); code != http.StatusOK {
		t.Fatalf("人工改分失败: %d", code)
	}

	code, body := f.do(t, http.MethodGet, "/api/v1/audit", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读审计失败: %d", code)
	}
	entries, _ := body["entries"].([]any)
	actions := map[string]bool{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if a, ok := m["action"].(string); ok {
			actions[a] = true
		}
	}
	for _, want := range []string{store.AuditSessionCreate, store.AuditScoreOverride} {
		if !actions[want] {
			t.Errorf("审计日志应包含 %s, 实际 %v", want, actions)
		}
	}
}

// 指标的 /metrics 端点必须真实可用(Prometheus 抓的是它)。
func TestMetricsEndpointExposesBusinessMetrics(t *testing.T) {
	f := newAuthFixture(t)
	f.createSession(t, f.adminKey)

	resp, err := http.Get(f.ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("抓取指标失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics 应返回 200, 实际 %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	text := buf.String()
	for _, metric := range []string{
		"interview_sessions_started_total",
		"interview_http_requests_total",
		"interview_ws_connections",
		"interview_first_response_seconds",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("指标 %s 未暴露", metric)
		}
	}
	if !strings.Contains(text, `round="1"`) {
		t.Error("指标应带业务标签(如 round)")
	}
}

func TestReadyzProbesStore(t *testing.T) {
	f := newAuthFixture(t)
	resp, err := http.Get(f.ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("就绪检查失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz 应返回 200, 实际 %d", resp.StatusCode)
	}
}

// 数据主体权利: 导出与删除必须可用, 且删除要精确到候选人。
func TestCandidateDataRightsOverHTTP(t *testing.T) {
	f := newAuthFixture(t)
	code, body := f.do(t, http.MethodPost, "/api/v1/sessions", f.adminKey, map[string]any{
		"round": 1, "minutes": 30, "candidate_id": "13800138000",
		"consent_recording": true, "consent_scoring": true,
	})
	if code != http.StatusOK {
		t.Fatalf("创建会话失败: %d", code)
	}
	ref, _ := body["candidate_ref"].(string)
	if ref == "" {
		t.Fatal("应返回候选人假名引用值")
	}
	if strings.Contains(ref, "13800138000") {
		t.Fatal("引用值不能包含候选人原文")
	}

	// 导出
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/api/v1/candidates/"+ref+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("导出请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导出应返回 200, 实际 %d", resp.StatusCode)
	}
	exported := new(bytes.Buffer)
	_, _ = exported.ReadFrom(resp.Body)
	if !strings.Contains(exported.String(), "candidate_ref") {
		t.Fatal("导出内容应包含结构化数据")
	}

	// 跨租户导出必须失败
	if code, _ := f.do(t, http.MethodGet, "/api/v1/candidates/"+ref+"/export", f.bKey, nil); code != http.StatusNotFound {
		t.Fatalf("跨租户导出应返回 404, 实际 %d", code)
	}

	// 删除
	code, delBody := f.do(t, http.MethodDelete, "/api/v1/candidates/"+ref, f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("删除失败: %d %v", code, delBody)
	}
	if erased, ok := delBody["erased_sessions"].(float64); !ok || erased < 1 {
		t.Fatalf("应至少删除 1 场会话, 实际 %v", delBody["erased_sessions"])
	}
	// 删除后导出应 404
	if code, _ := f.do(t, http.MethodGet, "/api/v1/candidates/"+ref+"/export", f.adminKey, nil); code != http.StatusNotFound {
		t.Fatalf("删除后导出应 404, 实际 %d", code)
	}
}

// 密钥生命周期: 创建 → 可用 → 吊销 → 失效。
func TestKeyLifecycle(t *testing.T) {
	f := newAuthFixture(t)

	code, body := f.do(t, http.MethodPost, "/api/v1/keys", f.adminKey, map[string]any{
		"name": "ATS 集成", "role": string(auth.RoleScheduler),
	})
	if code != http.StatusOK {
		t.Fatalf("创建密钥失败: %d %v", code, body)
	}
	newKey, _ := body["api_key"].(string)
	keyID, _ := body["key_id"].(string)
	if newKey == "" || keyID == "" {
		t.Fatalf("创建密钥应返回明文与 ID: %v", body)
	}

	// scheduler 可以创建面试
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions", newKey, map[string]any{
		"round": 1, "consent_recording": true, "consent_scoring": true,
	}); code != http.StatusOK {
		t.Fatalf("scheduler 应能创建面试, 实际 %d", code)
	}

	// 吊销后立即失效
	if code, _ := f.do(t, http.MethodDelete, "/api/v1/keys/"+keyID, f.adminKey, nil); code != http.StatusOK {
		t.Fatalf("吊销失败: %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions", newKey, map[string]any{
		"round": 1, "consent_recording": true, "consent_scoring": true,
	}); code != http.StatusUnauthorized {
		t.Fatalf("已吊销密钥应返回 401, 实际 %d", code)
	}
}

// 缺少任一授权都不能开始面试。
func TestConsentIsMandatoryAndRecorded(t *testing.T) {
	f := newAuthFixture(t)

	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions", f.adminKey, map[string]any{
		"round": 1, "consent_recording": false, "consent_scoring": true,
	}); code != http.StatusBadRequest {
		t.Fatalf("未同意录音应 400, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/sessions", f.adminKey, map[string]any{
		"round": 1, "consent_recording": true, "consent_scoring": false,
	}); code != http.StatusBadRequest {
		t.Fatalf("未同意 AI 评分应 400, 实际 %d", code)
	}

	id := f.createSession(t, f.adminKey)
	consents, err := f.store.ListConsents(context.Background(), f.tenantA, id)
	if err != nil {
		t.Fatalf("读取授权失败: %v", err)
	}
	if len(consents) != 2 {
		t.Fatalf("应记录录音与评分两项授权, 实际 %d 条", len(consents))
	}
	for _, c := range consents {
		if c.IP == "127.0.0.1" || c.IP == "::1" {
			t.Fatalf("授权记录里的 IP 必须脱敏, 实际 %q", c.IP)
		}
		if !strings.HasSuffix(c.IP, "x") && !strings.HasSuffix(c.IP, "/48") {
			t.Fatalf("IP 应脱敏到网段, 实际 %q", c.IP)
		}
	}
}
