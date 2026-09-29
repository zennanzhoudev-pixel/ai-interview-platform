package account

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
)

func testService(t *testing.T) *Service {
	t.Helper()
	// 迭代次数调到 1000: 测试要跑几十次哈希, 用生产参数(21 万次)会让
	// 整个包慢到没人愿意跑, 而这里验证的是逻辑而不是抗暴力破解强度。
	return New(Config{
		Store:           NewMemoryStore(),
		TenantID:        "tenant-a",
		AdminInviteCode: "HR-2026",
		Secret:          []byte("test-secret"),
		Hasher:          NewPasswordHasher(1000),
		SessionTTL:      time.Hour,
	})
}

func registerCandidate(t *testing.T, s *Service, email, phone string) User {
	t.Helper()
	u, err := s.Register(context.Background(), RegisterInput{
		Name: "陈雨", Email: email, Phone: phone, Password: "面试用的密码123",
		Role: auth.RoleCandidate,
	})
	if err != nil {
		t.Fatalf("注册候选人失败: %v", err)
	}
	return u
}

/* ---------------- 注册 ---------------- */

func TestRegisterCandidateNeedsOnlyContactAndPassword(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "ChenYu@Example.com ", "")

	if u.Role != auth.RoleCandidate {
		t.Fatalf("默认角色应为候选人, 实际 %q", u.Role)
	}
	// 邮箱必须被规范化, 否则同一个人用大小写不同的邮箱能注册出两个账号。
	if u.Email != "chenyu@example.com" {
		t.Fatalf("邮箱应被规范化, 实际 %q", u.Email)
	}
	if u.PasswordHash == "" || strings.Contains(u.PasswordHash, "面试密码") {
		t.Fatal("密码必须以哈希形式存储")
	}
	if !u.Active() || u.Staff() {
		t.Fatal("新注册的候选人应当是启用状态, 且不属于企业成员")
	}
}

func TestRegisterNormalizesPhoneToE164(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "", "138 0013 8000")
	if u.Phone != "+8613800138000" {
		t.Fatalf("手机号应规范化为 +86 形式, 实际 %q", u.Phone)
	}
	// 换一种写法注册同一个号码, 必须被认为是同一个账号。
	_, err := s.Register(context.Background(), RegisterInput{
		Name: "另一个人", Phone: "+86-138-0013-8000", Password: "面试用的密码123",
	})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("同一手机号的不同写法应被判为已注册, 实际 %v", err)
	}
}

func TestRegisterRejectsWeakInput(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   RegisterInput
		want error
	}{
		{"没有联系方式", RegisterInput{Name: "甲", Password: "面试用的密码123"}, ErrInvalidInput},
		{"邮箱格式错误", RegisterInput{Name: "甲", Email: "not-an-email", Password: "面试用的密码123"}, ErrInvalidInput},
		{"手机号格式错误", RegisterInput{Name: "甲", Phone: "12345", Password: "面试用的密码123"}, ErrInvalidInput},
		{"密码太短", RegisterInput{Name: "甲", Email: "a@b.com", Password: "abc123"}, ErrWeakPassword},
		{"纯数字密码", RegisterInput{Name: "甲", Email: "a@b.com", Password: "1234567890"}, ErrWeakPassword},
		{"常见弱口令", RegisterInput{Name: "甲", Email: "a@b.com", Password: "password"}, ErrWeakPassword},
		{"角色不合法", RegisterInput{Name: "甲", Email: "a@b.com", Password: "面试用的密码123", Role: auth.Role("root")}, ErrInvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Register(ctx, c.in); !errors.Is(err, c.want) {
				t.Fatalf("期望 %v, 实际 %v", c.want, err)
			}
		})
	}
}

// 企业成员不能自助注册 —— 这是整个账号体系里最关键的一道闸。
// 少了它, 任何人都能给自己开一个管理员账号, 拿到全部候选人的数据。
func TestStaffRegistrationRequiresInviteCode(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	base := RegisterInput{Name: "王琳", Email: "hr@example.com", Password: "面试用的密码123", Role: auth.RoleAdmin}

	if _, err := s.Register(ctx, base); !errors.Is(err, ErrInviteRequired) {
		t.Fatalf("没有邀请码不应能注册管理员, 实际 %v", err)
	}
	bad := base
	bad.InviteCode = "guess"
	if _, err := s.Register(ctx, bad); !errors.Is(err, ErrInviteRequired) {
		t.Fatalf("邀请码错误不应能注册, 实际 %v", err)
	}
	good := base
	good.InviteCode = "HR-2026"
	u, err := s.Register(ctx, good)
	if err != nil {
		t.Fatalf("邀请码正确时应能注册: %v", err)
	}
	if !u.Staff() {
		t.Fatal("管理员应被判定为企业成员(进工作台)")
	}
	if !auth.RoleCan(u.Role, auth.PermReportRead) {
		t.Fatal("管理员应具备查看报告的权限")
	}
}

func TestStaffRegistrationDisabledWhenNoInviteConfigured(t *testing.T) {
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Hasher: NewPasswordHasher(1000)})
	_, err := s.Register(context.Background(), RegisterInput{
		Name: "王琳", Email: "hr@example.com", Password: "面试用的密码123", Role: auth.RoleInterviewer,
	})
	if !errors.Is(err, ErrInviteRequired) {
		t.Fatalf("未配置邀请码时应拒绝企业成员注册, 实际 %v", err)
	}
}

/* ---------------- 登录与会话 ---------------- */

func TestLoginIssuesSessionAndAuthenticateResolvesIt(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()

	logged, session, err := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if logged.ID != u.ID {
		t.Fatalf("登录返回的账号不对: %s vs %s", logged.ID, u.ID)
	}
	if session.Token == "" || session.ExpiresAt.IsZero() {
		t.Fatalf("应签发令牌与过期时间: %+v", session)
	}

	// 登录后能凭令牌取回身份 —— 这是所有网页接口的基础。
	me, sess, err := s.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("令牌应可解析: %v", err)
	}
	if me.ID != u.ID || sess.Role != auth.RoleCandidate {
		t.Fatalf("解析出的身份不对: %+v / %+v", me.ID, sess.Role)
	}
	// 令牌必须以哈希形式落库: 拖库不应直接拿到可用的会话。
	if sess.TokenHash == session.Token {
		t.Fatal("存储的应是令牌哈希, 不是明文")
	}

	if err := s.Logout(ctx, session.Token); err != nil {
		t.Fatalf("退出登录失败: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, session.Token); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("退出后令牌应失效, 实际 %v", err)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	s := testService(t)
	registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()

	_, _, errWrong := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "错误密码123"})
	_, _, errNoUser := s.Login(ctx, LoginInput{Identifier: "nobody@example.com", Password: "面试用的密码123"})

	// 两种失败必须返回同一个错误: 区分开就等于提供了一个"账号是否存在"的查询接口。
	if !errors.Is(errWrong, ErrInvalidCredential) || !errors.Is(errNoUser, ErrInvalidCredential) {
		t.Fatalf("都应是 ErrInvalidCredential, 实际 %v / %v", errWrong, errNoUser)
	}
	if errWrong.Error() != errNoUser.Error() {
		t.Fatalf("两种失败的错误文案也必须一致, 实际 %q vs %q", errWrong, errNoUser)
	}
}

func TestDisabledUserLosesExistingSessions(t *testing.T) {
	store := NewMemoryStore()
	s := New(Config{Store: store, TenantID: "tenant-a", Hasher: NewPasswordHasher(1000)})
	ctx := context.Background()
	u := registerCandidate(t, s, "chen@example.com", "")
	_, session, err := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 管理员停用账号。
	u.Status = "disabled"
	if err := store.UpdateUser(ctx, u); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	// 已发出的会话必须立刻失效, 否则"停用"只是页面上的假动作。
	if _, _, err := s.Authenticate(ctx, session.Token); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("停用后旧会话应失效, 实际 %v", err)
	}
	if _, _, err := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("停用后不应能再登录, 实际 %v", err)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now()
	s := New(Config{
		Store: store, TenantID: "tenant-a", Hasher: NewPasswordHasher(1000),
		SessionTTL: time.Minute,
		Now:        func() time.Time { return now },
	})
	registerCandidate(t, s, "chen@example.com", "")
	_, session, err := s.Login(context.Background(), LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, _, err := s.Authenticate(context.Background(), session.Token); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("过期会话应被拒绝, 实际 %v", err)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()
	_, first, _ := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"})
	_, second, _ := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"})

	if err := s.ChangePassword(ctx, u.TenantID, u.ID, "面试用的密码123", "新的密码1234"); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}
	// 改密码的常见动因是"怀疑泄露", 因此旧会话必须一起失效。
	for name, token := range map[string]string{"first": first.Token, "second": second.Token} {
		if _, _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("%s 会话应在改密码后失效, 实际 %v", name, err)
		}
	}
	if _, _, err := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "新的密码1234"}); err != nil {
		t.Fatalf("新密码应能登录: %v", err)
	}
	if _, _, err := s.Login(ctx, LoginInput{Identifier: "chen@example.com", Password: "面试用的密码123"}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("旧密码不应能登录, 实际 %v", err)
	}
	// 原密码错误时必须拒绝。
	if err := s.ChangePassword(ctx, u.TenantID, u.ID, "乱填的密码", "再换一个密码789"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("原密码错误应拒绝, 实际 %v", err)
	}
}

/* ---------------- 人脸 ---------------- */

// syntheticFace 生成一张"人脸"占位图。
//
// 关键是**不同 seed 的结构真的不同**(五官位置、脸型宽窄都随 seed 变化),
// 而不是"同一张图换个相位"。第一版就是后者, 结果两张"不同的人"算出了
// 0.98 的相似度, 让测试失去了意义 —— 测试数据造得不对, 比没有测试更糟。
func syntheticFace(seed int, noise float64) image.Image {
	const side = 240
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	// 五官位置与脸型都由 seed 决定。
	eyeLX := 62 + (seed*17)%40
	eyeRX := 150 - (seed*13)%40
	eyeY := 78 + (seed*29)%34
	faceW := 78 + (seed*11)%26
	faceH := 104 + (seed*7)%26
	noseX := 110 + (seed*23)%30
	noseY := 132 + (seed*19)%24
	mouthY := 168 + (seed*31)%20
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			fx := float64(x-120) / float64(faceW)
			fy := float64(y-124) / float64(faceH)
			v := 40.0
			// 脸: 椭圆内的肤色区域
			if fx*fx+fy*fy <= 1 {
				v = 150
			}
			// 眼睛: 两个暗斑
			v -= 95 * math.Exp(-(math.Pow(float64(x-eyeLX), 2)+math.Pow(float64(y-eyeY), 2))/180)
			v -= 95 * math.Exp(-(math.Pow(float64(x-eyeRX), 2)+math.Pow(float64(y-eyeY), 2))/180)
			// 鼻子与嘴: 位置随 seed 变化
			v -= 45 * math.Exp(-(math.Pow(float64(x-noseX), 2)+math.Pow(float64(y-noseY), 2))/260)
			v -= 55 * math.Exp(-(math.Pow(float64(x-120), 2)+math.Pow(float64(y-mouthY), 2))/900)
			// 纹理: 保留少量细节, 但不足以主导相似度
			v += noise * float64((x*5+y*3+seed)%9-4)
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

func dataURL(t *testing.T, img image.Image) string {
	t.Helper()
	raw, err := EncodeJPEG(img, 85)
	if err != nil {
		t.Fatalf("编码测试图片失败: %v", err)
	}
	return "data:image/jpeg;base64," + base64Std(raw)
}

func base64Std(b []byte) string {
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

func TestFaceLoginRequiresEnrollmentFirst(t *testing.T) {
	s := testService(t)
	registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()

	// 没录入过人脸时, 即使画面没问题也不能登录 —— 不支持"刷脸即注册"。
	_, _, _, err := s.LoginWithFace(ctx, FaceLoginInput{
		Identifier: "chen@example.com", Frame: dataURL(t, syntheticFace(1, 1)),
	})
	if !errors.Is(err, ErrFaceNotEnrolled) {
		t.Fatalf("未录入人脸时应提示先去个人中心录入, 实际 %v", err)
	}
}

func TestFaceEnrollThenLoginSucceedsAndImpostorFails(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()

	profile, err := s.EnrollFace(ctx, u.TenantID, u.ID, []string{
		dataURL(t, syntheticFace(7, 0.4)),
		dataURL(t, syntheticFace(7, 0.6)),
		dataURL(t, syntheticFace(7, 0.8)),
	})
	if err != nil {
		t.Fatalf("录入人脸失败: %v", err)
	}
	if profile.Dim == 0 || profile.Frames != 3 {
		t.Fatalf("模板信息不正确: %+v", profile)
	}
	// 接口与界面都要能看到"这是开发级匹配器", 不能让人以为已经有人脸识别。
	if profile.Assurance != "development-only" || profile.Matcher != "local-image-similarity" {
		t.Fatalf("必须如实标注匹配器能力: %+v", profile)
	}

	// 本人(同 seed、轻微噪声)应能通过。
	_, session, score, err := s.LoginWithFace(ctx, FaceLoginInput{
		Identifier: "chen@example.com", Frame: dataURL(t, syntheticFace(7, 0.5)),
	})
	if err != nil {
		t.Fatalf("本人应能通过人脸登录: %v (score=%.4f)", err, score)
	}
	if session.Token == "" {
		t.Fatal("通过后应签发登录会话")
	}

	// 换一个人(不同 seed)必须被拒。
	if _, _, score, err := s.LoginWithFace(ctx, FaceLoginInput{
		Identifier: "chen@example.com", Frame: dataURL(t, syntheticFace(99, 0.5)),
	}); !errors.Is(err, ErrFaceMismatch) {
		t.Fatalf("不同的人不应通过, 实际 %v (score=%.4f)", err, score)
	}
	// 顺带把"分离间距"这件事记在测试里: 最像的"别人"与本人之间的距离
	// 只有 0.03 左右。这不是一个可以放心依赖的识别能力, 接口与界面
	// 因此必须如实标注 assurance=development-only。
	if _, _, score, err := s.LoginWithFace(ctx, FaceLoginInput{
		Identifier: "chen@example.com", Frame: dataURL(t, syntheticFace(8, 0.5)),
	}); !errors.Is(err, ErrFaceMismatch) {
		t.Fatalf("结构不同的画面不应通过, 实际 %v (score=%.4f)", err, score)
	}
}

func TestFaceEnrollRejectsUnstableFrames(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	// 第 2 帧完全不同 -> 说明画面不稳定(或中途换人), 必须拒绝。
	_, err := s.EnrollFace(context.Background(), u.TenantID, u.ID, []string{
		dataURL(t, syntheticFace(7, 0.4)),
		dataURL(t, syntheticFace(50, 0.4)),
	})
	if !errors.Is(err, ErrFaceLowQuality) {
		t.Fatalf("多帧不一致时应拒绝录入, 实际 %v", err)
	}
}

func TestFaceEnrollRejectsLowQualityImage(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	// 纯白画面: 没有可用的明暗结构。
	blank := image.NewRGBA(image.Rect(0, 0, 240, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 240; x++ {
			blank.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	if _, err := s.EnrollFace(context.Background(), u.TenantID, u.ID, []string{dataURL(t, blank)}); !errors.Is(err, ErrFaceLowQuality) {
		t.Fatalf("空白画面应被拒绝, 实际 %v", err)
	}

	// 分辨率不足也要拒绝: 小图在降采样后几乎没有可区分信息。
	tiny := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			tiny.Set(x, y, color.RGBA{R: uint8(x * 6), G: uint8(y * 6), B: 128, A: 255})
		}
	}
	if _, err := s.EnrollFace(context.Background(), u.TenantID, u.ID, []string{dataURL(t, tiny)}); !errors.Is(err, ErrFaceLowQuality) {
		t.Fatalf("分辨率过低应被拒绝, 实际 %v", err)
	}
}

func TestDeleteFaceRemovesAbilityToLoginByFace(t *testing.T) {
	s := testService(t)
	u := registerCandidate(t, s, "chen@example.com", "")
	ctx := context.Background()
	if _, err := s.EnrollFace(ctx, u.TenantID, u.ID, []string{dataURL(t, syntheticFace(3, 0.5))}); err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	if err := s.DeleteFace(ctx, u.TenantID, u.ID); err != nil {
		t.Fatalf("删除人脸失败: %v", err)
	}
	reloaded, err := s.store.GetUser(ctx, u.TenantID, u.ID)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if reloaded.FaceEnrolled {
		t.Fatal("删除后账号上的人脸标记也应清除")
	}
	if _, _, _, err := s.LoginWithFace(ctx, FaceLoginInput{
		Identifier: "chen@example.com", Frame: dataURL(t, syntheticFace(3, 0.5)),
	}); !errors.Is(err, ErrFaceNotEnrolled) {
		t.Fatalf("删除后不应还能人脸登录, 实际 %v", err)
	}
}

/* ---------------- 账号与候选人身份的对应 ---------------- */

func TestCandidateRefIsStableAcrossIdentifierSpellings(t *testing.T) {
	s := testService(t)
	a := registerCandidate(t, s, "", "13800138000")
	// 用另一种写法注册同一个人(会被判重复), 但我们关心的是引用值是否一致。
	refA := s.CandidateRef(a)
	refB := s.CandidateRef(User{TenantID: a.TenantID, Phone: "+86 138 0013 8000"})
	if refA == "" || refA != refB {
		t.Fatalf("同一手机号的不同写法必须派生出同一个候选人引用值: %q vs %q", refA, refB)
	}
	refOther := s.CandidateRef(User{TenantID: a.TenantID, Email: "other@example.com"})
	if refOther == refA {
		t.Fatal("不同的人不能得到相同的引用值")
	}
	refOtherTenant := s.CandidateRef(User{TenantID: "tenant-b", Phone: a.Phone})
	if refOtherTenant == refA {
		t.Fatal("跨租户必须是不同的引用值")
	}
}

func TestMatcherInfoIsHonestAboutAssurance(t *testing.T) {
	s := testService(t)
	info := s.MatcherInfo()
	if info["assurance"] != "development-only" {
		t.Fatalf("必须如实声明当前匹配器的可信级别: %v", info["assurance"])
	}
	if note, _ := info["note"].(string); !strings.Contains(note, "不是认证级人脸识别") {
		t.Fatalf("必须明确说明它不是认证级人脸识别: %v", note)
	}
}

func TestListUsersReturnsTenantScopedAndMaskable(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	registerCandidate(t, s, "chen@example.com", "")
	// 另一个租户的账号不应出现在列表里。
	if _, err := s.Register(ctx, RegisterInput{
		TenantID: "tenant-b", Name: "别人", Email: "other@example.com", Password: "面试用的密码123",
	}); err != nil {
		t.Fatalf("跨租户注册失败: %v", err)
	}
	list, err := s.store.ListUsers(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("列出账号失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("只应看到本租户的账号, 实际 %d", len(list))
	}
	masked := list[0].Masked()
	if masked.Email == "chen@example.com" || !strings.Contains(masked.Email, "***") {
		t.Fatalf("列表里的邮箱应脱敏, 实际 %q", masked.Email)
	}
}
