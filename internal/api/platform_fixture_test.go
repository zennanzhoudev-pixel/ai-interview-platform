package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/knowledge"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/recording"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/sandbox"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// platformFixture 是"完整形态"的服务: 题库、知识库、判题沙箱、录制存储都接上。
//
// 与 security_test 的 fixture 分开, 因为这里的目的是验证功能链路,
// 而不是权限边界; 两者的失败原因完全不同, 混在一起会让失败信息难以阅读。
type platformFixture struct {
	*authFixture
	dir string
}

func newPlatformFixture(t *testing.T) *platformFixture {
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

	dir := t.TempDir()
	blobs, err := recording.NewFSStore(dir)
	if err != nil {
		t.Fatalf("创建录制存储失败: %v", err)
	}

	// 知识库按租户构建: 读该租户的题目, 再叠加内置题库。
	build := func(ctx context.Context, tenant string) (*knowledge.Corpus, error) {
		items, err := st.ListQuestions(ctx, tenant)
		if err != nil {
			return nil, err
		}
		return knowledge.Build(ctx, items, rag.NewHashingEmbedder(256), rag.NewLocalReranker())
	}

	srv := NewServer(Config{
		Store:        st,
		Keys:         ks,
		Secret:       []byte("test-secret"),
		RequireAuth:  true,
		TenantID:     "tenant-a",
		KnowledgeFor: build,
		Sandbox:      sandbox.NewLocalRunner(),
		Blobs:        blobs,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &platformFixture{
		authFixture: &authFixture{
			ts: ts, store: st, keys: ks,
			tenantA: "tenant-a", tenantB: "tenant-b",
			adminKey: adminKey, viewKey: viewKey, bKey: bKey,
		},
		dir: dir,
	}
}

func (f *platformFixture) createJob(t *testing.T, title string) string {
	t.Helper()
	code, body := f.do(t, http.MethodPost, "/api/v1/jobs", f.adminKey, map[string]any{
		"title": title, "department": "基础架构", "level": "15A", "headcount": 2,
		"status": "open", "competency_model": map[string]int{"architecture": 60, "language_core": 40},
	})
	if code != http.StatusOK {
		t.Fatalf("创建职位失败: %d %v", code, body)
	}
	id, _ := body["job_id"].(string)
	if id == "" {
		t.Fatalf("响应缺少 job_id: %v", body)
	}
	return id
}

func (f *platformFixture) createCandidate(t *testing.T, withResume bool) string {
	t.Helper()
	payload := map[string]any{
		"candidate_id": "13800138000", "name": "陈雨",
		"email": "chenyu@example.com", "phone": "13800138000", "source": "内推",
	}
	if withResume {
		payload["resume_text"] = "2023.06 - 2024.03 云杉科技\n负责 ZSet 索引优化, 把查询 RT 降低 70%。\n用 Go 和 Redis 实现了分布式缓存。"
	}
	code, body := f.do(t, http.MethodPost, "/api/v1/candidates", f.adminKey, payload)
	if code != http.StatusOK {
		t.Fatalf("创建候选人失败: %d %v", code, body)
	}
	ref, _ := body["candidate_ref"].(string)
	if ref == "" {
		t.Fatalf("响应缺少 candidate_ref: %v", body)
	}
	return ref
}

func (f *platformFixture) createApplication(t *testing.T, jobID, candidateRef string) string {
	t.Helper()
	code, body := f.do(t, http.MethodPost, "/api/v1/applications", f.adminKey, map[string]any{
		"job_id": jobID, "candidate_ref": candidateRef, "owner": "招聘-王琳",
	})
	if code != http.StatusOK {
		t.Fatalf("创建投递失败: %d %v", code, body)
	}
	id, _ := body["application_id"].(string)
	if id == "" {
		t.Fatalf("响应缺少 application_id: %v", body)
	}
	return id
}

// scheduleAndStart 排期并开面, 返回会话 ID 与候选人令牌。
func (f *platformFixture) scheduleAndStart(t *testing.T, appID string, round int) (string, string) {
	t.Helper()
	code, body := f.do(t, http.MethodPost, "/api/v1/schedules", f.adminKey, map[string]any{
		"application_id": appID, "round": round, "scheduled_at": "2026-10-01T09:00:00Z",
	})
	if code != http.StatusOK {
		t.Fatalf("创建面试安排失败: %d %v", code, body)
	}
	scheduleID, _ := body["schedule_id"].(string)
	code, body = f.do(t, http.MethodPost, "/api/v1/schedules/"+scheduleID+"/start", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("开始面试失败: %d %v", code, body)
	}
	sessionID, _ := body["session_id"].(string)
	token, _ := body["session_token"].(string)
	if sessionID == "" || token == "" {
		t.Fatalf("开始面试应返回会话与令牌: %v", body)
	}
	return sessionID, token
}

func wsURL(ts *httptest.Server, sessionID string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/interview/" + sessionID
}

func dialWSWithToken(t *testing.T, ts *httptest.Server, sessionID, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, sessionID)+"?token="+token, nil)
	if err != nil {
		t.Fatalf("连接 WebSocket 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func dialWSWithTicket(t *testing.T, ts *httptest.Server, sessionID, ticket string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, sessionID)+"?role=observer&ticket="+ticket, nil)
	if err != nil {
		t.Fatalf("连接旁听席失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// sessionToken 直接为测试签发候选人令牌, 避免重复走一遍创建流程。
func (f *platformFixture) sessionToken(t *testing.T, sessionID string) string {
	t.Helper()
	sess, err := f.store.GetSession(context.Background(), f.tenantA, sessionID)
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	srv := &Server{cfg: Config{Secret: []byte("test-secret")}}
	token, _, err := srv.issueSessionToken(sess.ID, sess.TenantID)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	return token
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func TestJobsAreNormalizedToFiveRoundsAndTenantIsolated(t *testing.T) {
	f := newPlatformFixture(t)
	jobID := f.createJob(t, "高级后端工程师")

	code, body := f.do(t, http.MethodGet, "/api/v1/jobs/"+jobID, f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取职位失败: %d %v", code, body)
	}
	rounds, ok := body["rounds"].([]any)
	if !ok || len(rounds) != 5 {
		t.Fatalf("职位应补齐为 5 轮编排: %v", body["rounds"])
	}
	// 二面是编程轮、三面需要人类面试官在场 —— 这两条决定了候选人界面的形态。
	second := rounds[1].(map[string]any)
	if second["mode"] != "coding" {
		t.Fatalf("二面应为编程模式: %v", second)
	}
	third := rounds[2].(map[string]any)
	if third["human_panel"] != true {
		t.Fatalf("三面应有真人面试官在场: %v", third)
	}

	if code, _ := f.do(t, http.MethodGet, "/api/v1/jobs/"+jobID, f.bKey, nil); code != http.StatusNotFound {
		t.Fatalf("跨租户读取职位应 404, 实际 %d", code)
	}
	code, body = f.do(t, http.MethodGet, "/api/v1/jobs", f.bKey, nil)
	if code != http.StatusOK {
		t.Fatalf("租户B列表失败: %d", code)
	}
	if list, _ := body["jobs"].([]any); len(list) != 0 {
		t.Fatalf("租户 B 不应看到租户 A 的职位: %v", list)
	}
}

func TestSchedulerRoleHasNarrowPermissions(t *testing.T) {
	f := newPlatformFixture(t)
	schedKey, _, err := f.keys.Add("tenant-a", "ATS 集成", auth.RoleScheduler)
	if err != nil {
		t.Fatalf("签发排期密钥失败: %v", err)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/audit", schedKey, nil); code != http.StatusForbidden {
		t.Fatalf("排期账号不应能读审计日志, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/keys", schedKey, map[string]any{"name": "越权"}); code != http.StatusForbidden {
		t.Fatalf("排期账号不应能签密钥, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/jobs", schedKey, nil); code != http.StatusOK {
		t.Fatalf("排期账号应能读职位, 实际 %d", code)
	}
}

func TestCandidateContactsAreMaskedAndResumeIsLocated(t *testing.T) {
	f := newPlatformFixture(t)
	ref := f.createCandidate(t, true)

	code, body := f.do(t, http.MethodGet, "/api/v1/candidates/"+ref, f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取候选人失败: %d %v", code, body)
	}
	candidate, _ := body["candidate"].(map[string]any)
	email, _ := candidate["email"].(string)
	phone, _ := candidate["phone"].(string)
	if !strings.Contains(email, "***") {
		t.Fatalf("邮箱应脱敏存储: %q", email)
	}
	if strings.Contains(phone, "0138") || !strings.Contains(phone, "****") {
		t.Fatalf("手机号应脱敏存储: %q", phone)
	}

	resume, ok := body["resume"].(map[string]any)
	if !ok || resume == nil {
		t.Fatalf("应返回结构化简历: %v", body["resume"])
	}
	entities, _ := resume["entities"].([]any)
	if len(entities) == 0 {
		t.Fatal("简历应解析出实体")
	}
	foundLocated := false
	for _, e := range entities {
		m, _ := e.(map[string]any)
		start, _ := m["start"].(float64)
		end, _ := m["end"].(float64)
		if end > start {
			foundLocated = true
		}
	}
	if !foundLocated {
		t.Fatalf("实体必须带原文位置: %v", entities)
	}
}

func TestQuestionBankFeedsRetrievalAndIsTenantScoped(t *testing.T) {
	f := newPlatformFixture(t)

	code, body := f.do(t, http.MethodPost, "/api/v1/questions", f.adminKey, map[string]any{
		"stage": "TECH_FUNDAMENTAL", "competency": "distributed_system",
		"text":     "讲讲你线上做过的限流方案, 以及热点 key 怎么处理。",
		"keywords": []string{"令牌桶", "热点", "降级"},
		"reference_points": []map[string]string{
			{"key": "热点隔离", "text": "热点接口单独配置阈值, 避免被同一桶流量打穿"},
			{"key": "降级预案", "text": "限流组件故障时整体放行, 保障主链路可用"},
		},
		"importance": "high", "max_probe": 2, "status": "published",
	})
	if code != http.StatusOK {
		t.Fatalf("创建题目失败: %d %v", code, body)
	}
	qid, _ := body["question_id"].(string)
	if qid == "" {
		t.Fatalf("响应缺少 question_id: %v", body)
	}
	if v, _ := body["version"].(float64); v != 1 {
		t.Fatalf("新题版本应为 1, 实际 %v", body["version"])
	}

	code, body = f.do(t, http.MethodGet,
		"/api/v1/retrieval/search?q="+"热点接口被同一波流量打穿怎么办", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("检索失败: %d %v", code, body)
	}
	hits, _ := body["hits"].([]any)
	if len(hits) == 0 {
		t.Fatal("检索应有结果")
	}
	hit := false
	for _, h := range hits {
		m, _ := h.(map[string]any)
		if m["question_id"] == qid {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("检索应命中刚发布的题目, 实际 %v", hits)
	}

	code, body = f.do(t, http.MethodPatch, "/api/v1/questions/"+qid, f.adminKey, map[string]any{
		"text": "讲讲你线上做过的限流方案, 热点 key 与降级预案分别怎么设计?",
	})
	if code != http.StatusOK {
		t.Fatalf("更新题目失败: %d %v", code, body)
	}
	if v, _ := body["version"].(float64); v != 2 {
		t.Fatalf("更新后版本应为 2, 实际 %v", body["version"])
	}

	code, body = f.do(t, http.MethodGet, "/api/v1/knowledge/stats", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取知识库统计失败: %d", code)
	}
	if total, _ := body["questions"].(float64); total < 10 {
		t.Fatalf("知识库题目数异常: %v", body["questions"])
	}
	if src, _ := body["source"].(string); !strings.Contains(src, "租户 1 题") {
		t.Fatalf("知识库来源应包含租户题目: %v", body["source"])
	}
	if missing, _ := body["missing_structural_stages"].([]any); len(missing) != 0 {
		t.Fatalf("内置题库应覆盖流程骨架阶段: %v", missing)
	}

	code, body = f.do(t, http.MethodGet, "/api/v1/questions", f.bKey, nil)
	if code != http.StatusOK {
		t.Fatalf("租户B题库列表失败: %d", code)
	}
	for _, it := range body["questions"].([]any) {
		m, _ := it.(map[string]any)
		if m["question_id"] == qid {
			t.Fatal("租户 B 不应看到租户 A 的题目")
		}
	}
}

func TestQuestionValidationRejectsIncompleteReferencePoints(t *testing.T) {
	f := newPlatformFixture(t)
	code, body := f.do(t, http.MethodPost, "/api/v1/questions", f.adminKey, map[string]any{
		"stage": "TECH_FUNDAMENTAL", "text": "题干",
		"reference_points": []map[string]string{{"key": "只有名字"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("参考要点缺正文时应 400, 实际 %d %v", code, body)
	}
	if code, _ := f.do(t, http.MethodPost, "/api/v1/questions", f.adminKey,
		map[string]any{"stage": "TECH_FUNDAMENTAL"}); code != http.StatusBadRequest {
		t.Fatalf("缺题干时应 400, 实际 %d", code)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/retrieval/search", f.adminKey, nil); code == http.StatusOK {
		t.Fatal("空检索词不应返回 200")
	}
}

func TestPipelineMovesThroughStagesWithAudit(t *testing.T) {
	f := newPlatformFixture(t)
	jobID := f.createJob(t, "后端工程师")
	ref := f.createCandidate(t, false)
	appID := f.createApplication(t, jobID, ref)

	code, body := f.do(t, http.MethodGet, "/api/v1/applications/"+appID, f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取投递失败: %d %v", code, body)
	}
	app, _ := body["application"].(map[string]any)
	if app["stage"] != string(store.StageScreening) {
		t.Fatalf("新投递应处于初筛阶段: %v", app["stage"])
	}
	if rounds, _ := app["rounds"].([]any); len(rounds) != 5 {
		t.Fatalf("投递应带 5 轮状态: %v", app["rounds"])
	}

	code, body = f.do(t, http.MethodPatch, "/api/v1/applications/"+appID, f.adminKey, map[string]any{
		"stage": "offer", "note": "面试通过, 进入谈薪",
	})
	if code != http.StatusOK {
		t.Fatalf("推进管道失败: %d %v", code, body)
	}
	if body["stage"] != "offer" || body["status"] != "active" {
		t.Fatalf("阶段推进结果不正确: %v", body)
	}
	if code, _ := f.do(t, http.MethodPatch, "/api/v1/applications/"+appID, f.adminKey,
		map[string]any{"stage": "not_a_stage"}); code != http.StatusBadRequest {
		t.Fatalf("非法阶段应 400, 实际 %d", code)
	}

	code, body = f.do(t, http.MethodGet, "/api/v1/audit", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取审计失败: %d", code)
	}
	found := false
	for _, e := range body["entries"].([]any) {
		m, _ := e.(map[string]any)
		if m["action"] == store.AuditApplicationUpsert && m["target"] == appID {
			found = true
		}
	}
	if !found {
		t.Fatal("管道变更应写审计日志")
	}
}

func TestScheduleStartRequiresCandidateConsentBeforeInterview(t *testing.T) {
	f := newPlatformFixture(t)
	jobID := f.createJob(t, "后端工程师")
	ref := f.createCandidate(t, true)
	appID := f.createApplication(t, jobID, ref)
	sessionID, token := f.scheduleAndStart(t, appID, 1)

	code, body := f.do(t, http.MethodGet, "/api/v1/candidate/session?token="+token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("读取候选人会话失败: %d %v", code, body)
	}
	// 后台发起的面试没有代收同意, 必须由候选人本人确认。
	if body["consent_required"] != true {
		t.Fatalf("后台发起的面试应要求候选人确认授权: %v", body["consent_required"])
	}
	if body["resume_uploaded"] != true {
		t.Fatalf("候选人档案里的简历应带入会话: %v", body)
	}
	if body["mode"] != "video" {
		t.Fatalf("一面应为视频模式: %v", body["mode"])
	}

	conn := dialWSWithToken(t, f.ts, sessionID, token)
	if msg := readMsg(t, conn); msg["type"] != "consent_required" {
		t.Fatalf("未授权时应返回 consent_required, 实际 %v", msg["type"])
	}
	_ = conn.Close()

	if code, _ := f.do(t, http.MethodPost, "/api/v1/candidate/consent?token="+token, "",
		map[string]any{"recording": true, "scoring": false}); code != http.StatusBadRequest {
		t.Fatalf("只同意录音应 400, 实际 %d", code)
	}
	code, body = f.do(t, http.MethodPost, "/api/v1/candidate/consent?token="+token, "",
		map[string]any{"recording": true, "scoring": true})
	if code != http.StatusOK {
		t.Fatalf("确认授权失败: %d %v", code, body)
	}

	conn2 := dialWSWithToken(t, f.ts, sessionID, token)
	first := readMsg(t, conn2)
	if first["type"] != "state" {
		t.Fatalf("授权后应开始面试, 实际首帧 %v", first["type"])
	}
	if _, has := first["peer_id"]; !has {
		t.Fatalf("state 应带 peer_id(建立 P2P 视频需要): %v", first)
	}
	if _, has := first["ice_servers"]; !has {
		t.Fatalf("state 应带 ICE 配置: %v", first)
	}
	if first["mode"] != "video" {
		t.Fatalf("state 应带本轮模式: %v", first["mode"])
	}
	_ = readMsg(t, conn2)
	answerUntilReport(t, conn2, 80)
	_ = conn2.Close()

	code, body = f.do(t, http.MethodGet, "/api/v1/applications/"+appID, f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取投递失败: %d", code)
	}
	app, _ := body["application"].(map[string]any)
	if app["stage"] != string(store.StageAIInterview) {
		t.Fatalf("面试后管道应进入 ai_interview, 实际 %v", app["stage"])
	}
	rounds, _ := app["rounds"].([]any)
	round1, _ := rounds[0].(map[string]any)
	if round1["status"] != "finished" {
		t.Fatalf("第一轮应标记完成: %v", round1)
	}
	if score, _ := round1["score"].(float64); score <= 0 {
		t.Fatalf("第一轮应写入分数: %v", round1)
	}
	if round1["session_id"] != sessionID {
		t.Fatalf("轮次应关联会话: %v", round1)
	}
	// AI 的建议不能自动改变录用状态: 只记录事实, 由人做决定。
	if app["status"] != "active" {
		t.Fatalf("AI 结论不应自动淘汰或录用候选人: %v", app["status"])
	}

	code, body = f.do(t, http.MethodGet, "/api/v1/schedules", f.adminKey, nil)
	if code != http.StatusOK {
		t.Fatalf("读取安排失败: %d", code)
	}
	schedules, _ := body["schedules"].([]any)
	if len(schedules) != 1 {
		t.Fatalf("应有 1 条安排: %v", schedules)
	}
	if st, _ := schedules[0].(map[string]any)["status"].(string); st != "done" {
		t.Fatalf("面试结束后安排应为 done, 实际 %q", st)
	}
}

func TestCandidateCannotUseTokenOnAnotherSession(t *testing.T) {
	f := newPlatformFixture(t)
	mine := f.createSession(t, f.adminKey)
	other := f.createSession(t, f.adminKey)
	token := f.sessionToken(t, mine)
	if _, _, err := websocket.DefaultDialer.Dial(wsURL(f.ts, other)+"?token="+token, nil); err == nil {
		t.Fatal("令牌与会话不匹配时必须拒绝连接")
	}
}
