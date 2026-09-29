package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/account"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 这些用例守的是用户最关心的一件事: **登录后进哪个界面由角色决定**。
//
// 全部用 ResponseRecorder 直调处理器, 不起真实 HTTP 服务 —— 一是更快,
// 二是受限环境(无网络权限的开发机、CI 沙箱)不允许绑定端口, 而
// "权限判定对不对"跟"能不能监听 socket"毫无关系。

type accountFixture struct {
	handler  http.Handler
	store    store.SessionStore
	accounts *account.Service
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	st := store.NewMemoryStore()
	accStore := account.NewMemoryStore()
	svc := account.New(account.Config{
		Store:           accStore,
		TenantID:        "tenant-a",
		AdminInviteCode: "HR-INVITE",
		Secret:          []byte("test-secret"),
		Hasher:          account.NewPasswordHasher(1000), // 测试用低迭代, 只验证逻辑
	})
	srv := NewServer(Config{
		Store:       st,
		Keys:        auth.NewMemoryKeyStore(),
		Secret:      []byte("test-secret"),
		RequireAuth: true, // 开启鉴权才会真正走"账号会话"这条路
		TenantID:    "tenant-a",
		Accounts:    svc,
	})
	return &accountFixture{handler: srv.Handler(), store: st, accounts: svc}
}

// call 发一个请求并返回响应, 以及响应里的 Cookie。
func (f *accountFixture) call(t *testing.T, method, path string, body any, cookies []*http.Cookie) (*httptest.ResponseRecorder, []*http.Cookie, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	var payload map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	}
	var out []*http.Cookie
	for _, raw := range rec.Result().Cookies() {
		if raw.Value != "" {
			out = append(out, raw)
		}
	}
	return rec, out, payload
}

func (f *accountFixture) register(t *testing.T, body map[string]any) (*httptest.ResponseRecorder, []*http.Cookie, map[string]any) {
	t.Helper()
	return f.call(t, http.MethodPost, "/api/v1/auth/register", body, nil)
}

func (f *accountFixture) login(t *testing.T, identifier, password string) (*httptest.ResponseRecorder, []*http.Cookie, map[string]any) {
	t.Helper()
	return f.call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"identifier": identifier, "password": password,
	}, nil)
}

func candidateBody(email string) map[string]any {
	return map[string]any{
		"name": "陈雨", "email": email, "password": "面试用的密码123", "role": "candidate",
	}
}

func adminBody(email string) map[string]any {
	return map[string]any{
		"name": "王琳", "email": email, "password": "面试用的密码123",
		"role": "admin", "invite_code": "HR-INVITE",
	}
}

/* ---------------- 注册与登录 ---------------- */

func TestRegisterCandidateAutoLogsInAndRoutesToCandidateSpace(t *testing.T) {
	f := newAccountFixture(t)
	rec, cookies, body := f.register(t, candidateBody("chen@example.com"))

	if rec.Code != http.StatusOK {
		t.Fatalf("候选人注册应成功, 实际 %d %v", rec.Code, body)
	}
	// 注册后直接建立登录态: 不该让用户"刚注册完还要再登录一次"。
	if len(cookies) == 0 {
		t.Fatal("注册后应下发登录 Cookie")
	}
	// 会话 Cookie 必须是 HttpOnly + SameSite: 前者防 XSS 窃取, 后者防 CSRF。
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("应下发名为 %s 的会话 Cookie, 实际 %v", sessionCookieName, cookies)
	}
	if !session.HttpOnly {
		t.Fatal("会话 Cookie 必须是 HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Fatal("会话 Cookie 必须设置 SameSite")
	}

	// 角色与"该进哪个界面"由服务端给出, 前端不自己猜。
	if body["role"] != "candidate" || body["staff"] != false {
		t.Fatalf("候选人不应被判定为企业成员: %v", body)
	}
	if body["next_path"] != "/candidate" {
		t.Fatalf("候选人登录后应进候选人空间, 实际 %v", body["next_path"])
	}
}

func TestRegisterAdminRequiresInviteAndRoutesToConsole(t *testing.T) {
	f := newAccountFixture(t)
	// 没有邀请码: 必须拒绝 —— 否则任何人都能给自己开管理员账号。
	noInvite := adminBody("hr@example.com")
	delete(noInvite, "invite_code")
	rec, _, _ := f.register(t, noInvite)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无邀请码注册管理员应 403, 实际 %d", rec.Code)
	}
	// 邀请码错误: 同样拒绝。
	wrong := adminBody("hr@example.com")
	wrong["invite_code"] = "guess"
	if rec, _, _ := f.register(t, wrong); rec.Code != http.StatusForbidden {
		t.Fatalf("邀请码错误应 403, 实际 %d", rec.Code)
	}

	rec, cookies, body := f.register(t, adminBody("hr@example.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("邀请码正确时应能注册管理员, 实际 %d %v", rec.Code, body)
	}
	if body["staff"] != true || body["role"] != "admin" {
		t.Fatalf("管理员应被判定为企业成员: %v", body)
	}
	if body["next_path"] != "/console/dashboard" {
		t.Fatalf("管理员登录后应进招聘工作台, 实际 %v", body["next_path"])
	}
	// 拿这个会话去访问一个管理接口, 应当直接通过(不需要 API Key)。
	rec, _, _ = f.call(t, http.MethodGet, "/api/v1/jobs", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员会话应能读取职位列表, 实际 %d", rec.Code)
	}
}

func TestCandidateSessionCannotReachConsoleAPIs(t *testing.T) {
	f := newAccountFixture(t)
	_, candidateCookies, _ := f.register(t, candidateBody("chen@example.com"))

	// 这是最关键的一条边界: 候选人登录后不能读招聘数据。
	for _, path := range []string{
		"/api/v1/jobs",
		"/api/v1/candidates",
		"/api/v1/applications",
		"/api/v1/questions",
		"/api/v1/audit",
		"/api/v1/analytics/pipeline",
		"/api/v1/auth/accounts",
	} {
		rec, _, _ := f.call(t, http.MethodGet, path, nil, candidateCookies)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 对候选人应返回 403, 实际 %d", path, rec.Code)
		}
	}
	// 但候选人自己的历史记录必须能看。
	rec, _, _ := f.call(t, http.MethodGet, "/api/v1/candidate/history", nil, candidateCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("候选人应能查看自己的历史记录, 实际 %d", rec.Code)
	}
}

func TestLoginFailuresLookIdenticalToClient(t *testing.T) {
	f := newAccountFixture(t)
	f.register(t, candidateBody("chen@example.com"))

	recWrong, _, bodyWrong := f.login(t, "chen@example.com", "错误的密码123")
	recNoUser, _, bodyNoUser := f.login(t, "nobody@example.com", "面试用的密码123")

	if recWrong.Code != http.StatusUnauthorized || recNoUser.Code != http.StatusUnauthorized {
		t.Fatalf("两种情况都应 401, 实际 %d / %d", recWrong.Code, recNoUser.Code)
	}
	// 区分的后果是: 这个接口可以被用来确认"某个人是否在你们公司投过简历"。
	if bodyWrong["error"] != bodyNoUser["error"] {
		t.Fatalf("两种失败的文案必须一致, 实际 %q vs %q", bodyWrong["error"], bodyNoUser["error"])
	}
}

func TestMeAndLogoutFlow(t *testing.T) {
	f := newAccountFixture(t)
	_, cookies, _ := f.register(t, candidateBody("chen@example.com"))

	rec, _, body := f.call(t, http.MethodGet, "/api/v1/auth/me", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("已登录时应能读取自己的信息, 实际 %d", rec.Code)
	}
	if body["role"] != "candidate" {
		t.Fatalf("角色不正确: %v", body["role"])
	}
	if face, ok := body["face"].(map[string]any); !ok || face["enrolled"] != false {
		t.Fatalf("未录入人脸时应如实返回 enrolled=false: %v", body["face"])
	}
	// 人脸能力的可信级别必须透出到接口, 不能只写在注释里。
	matcher, _ := body["matcher"].(map[string]any)
	if matcher["assurance"] != "development-only" {
		t.Fatalf("必须如实声明匹配器级别: %v", matcher)
	}

	if rec, _, _ := f.call(t, http.MethodPost, "/api/v1/auth/logout", nil, cookies); rec.Code != http.StatusOK {
		t.Fatalf("退出登录失败: %d", rec.Code)
	}
	if rec, _, _ := f.call(t, http.MethodGet, "/api/v1/auth/me", nil, cookies); rec.Code != http.StatusUnauthorized {
		t.Fatalf("退出后 /auth/me 应返回 401, 实际 %d", rec.Code)
	}
}

func TestChangePasswordRequiresRelogin(t *testing.T) {
	f := newAccountFixture(t)
	_, cookies, _ := f.register(t, candidateBody("chen@example.com"))

	rec, _, body := f.call(t, http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current_password": "面试用的密码123", "new_password": "换一个密码456",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("改密码失败: %d %v", rec.Code, body)
	}
	if body["relogin_required"] != true {
		t.Fatalf("改密码后应提示重新登录: %v", body)
	}
	// 旧会话必须失效(怀疑泄露才会改密码, 旧会话还能用等于没改)。
	if rec, _, _ := f.call(t, http.MethodGet, "/api/v1/auth/me", nil, cookies); rec.Code != http.StatusUnauthorized {
		t.Fatalf("改密码后旧会话应失效, 实际 %d", rec.Code)
	}
	// 新密码可以登录。
	if rec, _, _ := f.login(t, "chen@example.com", "换一个密码456"); rec.Code != http.StatusOK {
		t.Fatalf("新密码应能登录, 实际 %d", rec.Code)
	}
}

/* ---------------- 人脸 ---------------- */

func TestFaceEnrollThenLoginThroughHTTP(t *testing.T) {
	f := newAccountFixture(t)
	_, cookies, _ := f.register(t, candidateBody("chen@example.com"))

	// 未录入就用人脸登录: 必须提示"先去个人中心录入", 而不是放行。
	rec, _, _ := f.call(t, http.MethodPost, "/api/v1/auth/face/login", map[string]any{
		"identifier": "chen@example.com", "frame": faceFrame(t, 7),
	}, nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("未录入人脸时应 412, 实际 %d", rec.Code)
	}

	// 录入三帧。
	rec, _, body := f.call(t, http.MethodPost, "/api/v1/auth/face/enroll", map[string]any{
		"frames": []string{faceFrame(t, 7), faceFrame(t, 7), faceFrame(t, 7)},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("录入人脸失败: %d %v", rec.Code, body)
	}
	if body["assurance"] != "development-only" {
		t.Fatalf("接口必须如实标注匹配器级别: %v", body)
	}

	// 本人可登录。
	rec, newCookies, body := f.call(t, http.MethodPost, "/api/v1/auth/face/login", map[string]any{
		"identifier": "chen@example.com", "frame": faceFrame(t, 7),
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("人脸登录应成功, 实际 %d %v", rec.Code, body)
	}
	if len(newCookies) == 0 {
		t.Fatal("人脸登录成功后应下发会话 Cookie")
	}
	if body["assurance"] != "development-only" {
		t.Fatalf("登录结果里也要带可信级别: %v", body)
	}

	// 换一个人必须被拒。
	rec, _, _ = f.call(t, http.MethodPost, "/api/v1/auth/face/login", map[string]any{
		"identifier": "chen@example.com", "frame": faceFrame(t, 42),
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("不同的人不应登入, 实际 %d", rec.Code)
	}

	// 移除人脸后不能再用人脸登录。
	if rec, _, _ := f.call(t, http.MethodDelete, "/api/v1/auth/face", nil, cookies); rec.Code != http.StatusOK {
		t.Fatalf("移除人脸失败: %d", rec.Code)
	}
	rec, _, _ = f.call(t, http.MethodPost, "/api/v1/auth/face/login", map[string]any{
		"identifier": "chen@example.com", "frame": faceFrame(t, 7),
	}, nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("移除人脸后应回到未录入状态, 实际 %d", rec.Code)
	}
}

/* ---------------- 历史面试记录 ---------------- */

func TestCandidateHistoryOnlyShowsOwnSessions(t *testing.T) {
	f := newAccountFixture(t)
	_, cookies, _ := f.register(t, candidateBody("chen@example.com"))

	// 直接往存储里放两场面试: 一场属于这个候选人, 一场属于别人。
	ctx := context.Background()
	ref := f.accounts.CandidateRef(account.User{TenantID: "tenant-a", Email: "chen@example.com"})
	if ref == "" {
		t.Fatal("应能由账号派生出候选人引用值")
	}
	for _, item := range []store.Session{
		{ID: "s_mine", TenantID: "tenant-a", CandidateRef: ref, Position: "后端工程师", Company: "云杉科技", Round: 1, Status: store.StatusFinished},
		{ID: "s_other", TenantID: "tenant-a", CandidateRef: "cand_someone_else", Position: "前端工程师", Company: "云杉科技", Round: 1, Status: store.StatusFinished},
	} {
		if err := f.store.CreateSession(ctx, item); err != nil {
			t.Fatalf("准备测试数据失败: %v", err)
		}
	}

	rec, _, body := f.call(t, http.MethodGet, "/api/v1/candidate/history", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取历史记录失败: %d", rec.Code)
	}
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("只应看到属于自己的那场面试, 实际 %d 条: %v", len(sessions), sessions)
	}
	first, _ := sessions[0].(map[string]any)
	if first["session_id"] != "s_mine" {
		t.Fatalf("历史记录返回了别人的面试: %v", first)
	}
	if first["report_ready"] != true {
		t.Fatalf("已完成的会话应标记报告可看: %v", first)
	}
}

func TestUnknownAccountRoutesAreNotExposed(t *testing.T) {
	f := newAccountFixture(t)
	// 未登录时: 账号相关接口应 401, 而不是"未找到"或直接放行。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/candidate/history"},
		{http.MethodPost, "/api/v1/auth/password"},
		{http.MethodPost, "/api/v1/auth/face/enroll"},
	} {
		rec, _, _ := f.call(t, tc.method, tc.path, map[string]any{}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录时应 401, 实际 %d", tc.method, tc.path, rec.Code)
		}
	}
}

// faceFrame 生成一帧"人脸"图片的 data URL(与 account 包测试用的同一套合成图)。
func faceFrame(t *testing.T, seed int) string {
	t.Helper()
	img := syntheticFaceImage(seed)
	raw, err := account.EncodeJPEG(img, 85)
	if err != nil {
		t.Fatalf("编码测试图片失败: %v", err)
	}
	return "data:image/jpeg;base64," + base64StdEncode(raw)
}

func base64StdEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		sb.WriteByte(alphabet[chunk[0]>>2])
		sb.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			sb.WriteByte(alphabet[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			sb.WriteByte('=')
		}
		if n > 2 {
			sb.WriteByte(alphabet[chunk[2]&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}

func syntheticFaceImage(seed int) image.Image {
	const side = 240
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	eyeLX := 62 + (seed*17)%40
	eyeRX := 150 - (seed*13)%40
	eyeY := 78 + (seed*29)%34
	faceW := 78 + (seed*11)%26
	faceH := 104 + (seed*7)%26
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			fx := float64(x-120) / float64(faceW)
			fy := float64(y-124) / float64(faceH)
			v := 40.0
			if fx*fx+fy*fy <= 1 {
				v = 150
			}
			v -= 95 * math.Exp(-(math.Pow(float64(x-eyeLX), 2)+math.Pow(float64(y-eyeY), 2))/180)
			v -= 95 * math.Exp(-(math.Pow(float64(x-eyeRX), 2)+math.Pow(float64(y-eyeY), 2))/180)
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			c := uint8(v)
			img.Set(x, y, color.RGBA{R: c, G: c, B: c, A: 255})
		}
	}
	return img
}
