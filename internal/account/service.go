package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/privacy"
)

// Service 是账号域的应用服务: 注册、登录、人脸、会话。
//
// 所有与"人"有关的规则都集中在这里, 接入层只负责把 HTTP 翻译成调用 ——
// 这样"密码策略""谁能注册成管理员""人脸登录的前置条件"这些规则
// 只有一处定义, 不会出现"网页端校验了、接口没校验"的漏洞。
type Service struct {
	store   Store
	hasher  *PasswordHasher
	matcher Matcher
	logger  *slog.Logger

	tenantID        string
	adminInviteCode string
	secret          []byte
	sessionTTL      time.Duration
	now             func() time.Time
}

// Config 配置账号服务。
type Config struct {
	Store Store
	// TenantID 是默认租户。单租户部署时所有注册都落在这里;
	// 多租户场景由邀请码/链接决定, 但这需要一个明确的来源,
	// 不能接受注册表单自己填租户 —— 那等于让任何人把账号建到别家公司里。
	TenantID string
	// AdminInviteCode 是企业成员(管理员/面试官)注册的邀请码。
	// 为空表示"不允许自助注册企业成员", 只能由管理员在后台创建。
	AdminInviteCode string
	// Secret 用于派生候选人引用值, 让账号能与候选人档案对上。
	Secret []byte
	// Hasher 可为 nil, 默认使用生产参数的 PBKDF2。
	Hasher  *PasswordHasher
	Matcher Matcher
	// SessionTTL 默认 7 天。面试系统的登录态过长没有好处:
	// 候选人用完就走, 而企业成员应当定期重新认证。
	SessionTTL time.Duration
	Logger     *slog.Logger
	Now        func() time.Time
}

// New 构造账号服务。
func New(cfg Config) *Service {
	if cfg.Hasher == nil {
		cfg.Hasher = NewPasswordHasher(0)
	}
	if cfg.Matcher == nil {
		cfg.Matcher = NewLocalMatcher()
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		store:           cfg.Store,
		hasher:          cfg.Hasher,
		matcher:         cfg.Matcher,
		logger:          cfg.Logger,
		tenantID:        cfg.TenantID,
		adminInviteCode: cfg.AdminInviteCode,
		secret:          cfg.Secret,
		sessionTTL:      cfg.SessionTTL,
		now:             cfg.Now,
	}
}

// MatcherInfo 描述当前的人脸匹配能力, 用于自检与界面提示。
func (s *Service) MatcherInfo() map[string]any {
	return map[string]any{
		"name":      s.matcher.Name(),
		"assurance": s.matcher.Assurance(),
		"threshold": faceMatchThreshold,
		"note": "当前使用的是本地图像相似度匹配(开发用途), 不是认证级人脸识别; " +
			"生产环境请接入云厂商人脸 API 或本地 SDK(实现 Matcher 接口即可)",
	}
}

// RegisterInput 是注册输入。
type RegisterInput struct {
	TenantID   string
	Name       string
	Email      string
	Phone      string
	Password   string
	Role       auth.Role
	InviteCode string
}

// Register 创建账号。
func (s *Service) Register(ctx context.Context, in RegisterInput) (User, error) {
	tenant := in.TenantID
	if tenant == "" {
		tenant = s.tenantID
	}
	if tenant == "" {
		return User{}, fmt.Errorf("%w: 缺少租户", ErrInvalidInput)
	}
	name := trimSpace(in.Name)
	if len([]rune(name)) < 1 || len([]rune(name)) > 32 {
		return User{}, fmt.Errorf("%w: 姓名长度需在 1-32 字符之间", ErrInvalidInput)
	}

	email, err := ValidateEmail(in.Email)
	if err != nil {
		return User{}, err
	}
	phone := ""
	if trimSpace(in.Phone) != "" {
		phone, err = NormalizePhone(in.Phone)
		if err != nil {
			return User{}, err
		}
	}
	if email == "" && phone == "" {
		return User{}, fmt.Errorf("%w: 邮箱与手机号至少要填一个", ErrInvalidInput)
	}
	if err := ValidatePassword(in.Password); err != nil {
		return User{}, err
	}

	role := in.Role
	if role == "" {
		role = auth.RoleCandidate
	}
	if !auth.RoleValid(role) {
		return User{}, fmt.Errorf("%w: 角色不合法", ErrInvalidInput)
	}
	// 企业成员不能自助注册: 否则任何人都能给自己开一个管理员账号,
	// 拿到全部候选人数据。这是整个账号体系里最关键的一道闸。
	if auth.StaffRole(role) {
		switch {
		case s.adminInviteCode == "":
			return User{}, fmt.Errorf("%w: 未开启企业成员自助注册, 请联系管理员创建账号", ErrInviteRequired)
		case trimSpace(in.InviteCode) != s.adminInviteCode:
			return User{}, fmt.Errorf("%w: 邀请码不正确", ErrInviteRequired)
		}
	}

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return User{}, err
	}
	id, err := RandomToken(12)
	if err != nil {
		return User{}, err
	}
	u := User{
		ID:           "u_" + id,
		TenantID:     tenant,
		Name:         name,
		Email:        email,
		Phone:        phone,
		Role:         role,
		PasswordHash: hash,
		Status:       "active",
		CreatedAt:    s.now(),
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return User{}, err
	}
	s.logger.InfoContext(ctx, "账号注册成功",
		"tenant", tenant, "user_id", u.ID, "role", string(role), "has_email", email != "", "has_phone", phone != "")
	return u, nil
}

// IssuedSession 是签发出去的登录会话: 明文令牌只在这里出现一次。
type IssuedSession struct {
	Token     string
	ExpiresAt time.Time
}

// LoginInput 是密码登录输入。
type LoginInput struct {
	TenantID   string
	Identifier string // 邮箱或手机号
	Password   string
	IP         string
	UserAgent  string
}

// Login 用密码登录。
func (s *Service) Login(ctx context.Context, in LoginInput) (User, IssuedSession, error) {
	tenant := in.TenantID
	if tenant == "" {
		tenant = s.tenantID
	}
	identifier := trimSpace(in.Identifier)
	if identifier == "" || in.Password == "" {
		return User{}, IssuedSession{}, ErrInvalidCredential
	}

	u, err := s.store.FindUser(ctx, tenant, identifier)
	if err != nil {
		// 这里**故意**把所有"找不到账号"的情况都翻译成同一个错误:
		// 如果区分"账号不存在"和"密码错误", 登录接口就变成了账号枚举器,
		// 攻击者可以拿它来确认某个人是否在你们公司投过简历。
		return User{}, IssuedSession{}, ErrInvalidCredential
	}
	if !u.Active() {
		return User{}, IssuedSession{}, ErrUserDisabled
	}
	ok, rehash := s.hasher.Verify(u.PasswordHash, in.Password)
	if !ok {
		return User{}, IssuedSession{}, ErrInvalidCredential
	}
	// 哈希参数升级: 用户在下次登录时被透明迁移到新参数, 不需要改密码。
	if rehash {
		if hash, err := s.hasher.Hash(in.Password); err == nil {
			u.PasswordHash = hash
			if err := s.store.UpdateUser(ctx, u); err != nil {
				s.logger.WarnContext(ctx, "登录时升级密码哈希失败", "user_id", u.ID, "err", err)
			}
		}
	}
	return s.finishLogin(ctx, u, in.IP, in.UserAgent)
}

// FaceLoginInput 是人脸登录输入。
type FaceLoginInput struct {
	TenantID   string
	Identifier string
	// Frame 是一张 data URL 图片。
	Frame     string
	IP        string
	UserAgent string
}

// LoginWithFace 用人脸登录。
//
// 前置条件是有意收紧的:
//  1. **必须先有账号**, 且必须在个人中心录入过人脸 —— 不支持"刷脸即注册",
//     否则一次误识别就会凭空创建一个账号, 而且没人知道它是谁的;
//  2. 登录时仍需提供账号标识, 做的是 1:1 比对而不是 1:N 检索 ——
//     1:N 需要在大库里"猜"你是谁, 误识率随用户数上升, 不适合做登录凭证。
func (s *Service) LoginWithFace(ctx context.Context, in FaceLoginInput) (User, IssuedSession, float64, error) {
	tenant := in.TenantID
	if tenant == "" {
		tenant = s.tenantID
	}
	identifier := trimSpace(in.Identifier)
	if identifier == "" {
		return User{}, IssuedSession{}, 0, fmt.Errorf("%w: 请先填写手机号或邮箱", ErrInvalidInput)
	}
	u, err := s.store.FindUser(ctx, tenant, identifier)
	if err != nil {
		return User{}, IssuedSession{}, 0, ErrInvalidCredential
	}
	if !u.Active() {
		return User{}, IssuedSession{}, 0, ErrUserDisabled
	}
	profile, err := s.store.GetFace(ctx, tenant, u.ID)
	if err != nil {
		return User{}, IssuedSession{}, 0, ErrFaceNotEnrolled
	}
	img, err := DecodeDataURL(in.Frame)
	if err != nil {
		return User{}, IssuedSession{}, 0, err
	}
	vec, quality, err := s.matcher.Embed(img)
	if err != nil {
		return User{}, IssuedSession{}, 0, err
	}
	score := s.matcher.Compare(profile.Template, vec)
	s.logger.InfoContext(ctx, "人脸登录比对",
		"tenant", tenant, "user_id", u.ID, "score", score, "quality", quality,
		"matcher", s.matcher.Name(), "assurance", s.matcher.Assurance())
	if score < faceMatchThreshold {
		return User{}, IssuedSession{}, score, ErrFaceMismatch
	}
	user, session, err := s.finishLogin(ctx, u, in.IP, in.UserAgent)
	return user, session, score, err
}

func (s *Service) finishLogin(ctx context.Context, u User, ip, userAgent string) (User, IssuedSession, error) {
	token, err := RandomToken(32)
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	expiresAt := s.now().Add(s.sessionTTL)
	session := LoginSession{
		TokenHash: HashToken(token),
		TenantID:  u.TenantID,
		UserID:    u.ID,
		Role:      u.Role,
		IP:        privacy.MaskIP(ip),
		UserAgent: privacy.MaskUserAgent(userAgent),
		CreatedAt: s.now(),
		ExpiresAt: expiresAt,
	}
	if err := s.store.CreateLoginSession(ctx, session); err != nil {
		return User{}, IssuedSession{}, err
	}
	u.LastLoginAt = s.now()
	if err := s.store.UpdateUser(ctx, u); err != nil {
		// 记录登录时间失败不该让登录失败: 这是一个观测字段, 不是安全边界。
		s.logger.WarnContext(ctx, "更新最近登录时间失败", "user_id", u.ID, "err", err)
	}
	return u, IssuedSession{Token: token, ExpiresAt: expiresAt}, nil
}

// Authenticate 用登录令牌换取账号。它是所有网页接口的入口。
func (s *Service) Authenticate(ctx context.Context, token string) (User, LoginSession, error) {
	if trimSpace(token) == "" {
		return User{}, LoginSession{}, ErrSessionExpired
	}
	session, err := s.store.GetLoginSession(ctx, HashToken(token))
	if err != nil {
		return User{}, LoginSession{}, ErrSessionExpired
	}
	if session.Expired(s.now()) {
		_ = s.store.DeleteLoginSession(ctx, session.TokenHash)
		return User{}, LoginSession{}, ErrSessionExpired
	}
	u, err := s.store.GetUser(ctx, session.TenantID, session.UserID)
	if err != nil {
		return User{}, LoginSession{}, ErrSessionExpired
	}
	if !u.Active() {
		// 账号被停用后, 已经发出的会话也必须立刻失效 —— 否则"停用"只是
		// 一个在页面上生效、而实际还能继续用旧会话操作的假动作。
		_ = s.store.DeleteUserSessions(ctx, session.TenantID, session.UserID)
		return User{}, LoginSession{}, ErrUserDisabled
	}
	return u, session, nil
}

// Logout 删除当前会话。
func (s *Service) Logout(ctx context.Context, token string) error {
	if trimSpace(token) == "" {
		return nil
	}
	return s.store.DeleteLoginSession(ctx, HashToken(token))
}

// ChangePassword 修改密码, 并让其它会话全部失效。
//
// 撤销其它会话是必须的: 改密码的常见动因就是"怀疑密码泄露了",
// 如果旧会话还能继续用, 这次修改等于没做。
func (s *Service) ChangePassword(ctx context.Context, tenantID, userID, current, next string) error {
	u, err := s.store.GetUser(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	if ok, _ := s.hasher.Verify(u.PasswordHash, current); !ok {
		return ErrInvalidCredential
	}
	if err := ValidatePassword(next); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(next)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return err
	}
	return s.store.DeleteUserSessions(ctx, tenantID, userID)
}

// EnrollFace 录入人脸(要求多帧一致, 见 faceEnrollStability 的说明)。
func (s *Service) EnrollFace(ctx context.Context, tenantID, userID string, frames []string) (FaceProfile, error) {
	if len(frames) == 0 {
		return FaceProfile{}, fmt.Errorf("%w: 没有收到画面", ErrFaceLowQuality)
	}
	if len(frames) > 6 {
		frames = frames[:6]
	}
	u, err := s.store.GetUser(ctx, tenantID, userID)
	if err != nil {
		return FaceProfile{}, err
	}

	vecs := make([][]float32, 0, len(frames))
	var qualitySum float64
	for i, frame := range frames {
		img, err := DecodeDataURL(frame)
		if err != nil {
			return FaceProfile{}, fmt.Errorf("第 %d 帧无法解析: %w", i+1, err)
		}
		vec, quality, err := s.matcher.Embed(img)
		if err != nil {
			return FaceProfile{}, fmt.Errorf("第 %d 帧质量不合格: %w", i+1, err)
		}
		vecs = append(vecs, vec)
		qualitySum += quality
	}
	// 多帧一致性: 画面必须是稳定的同一个对象。抖动/遮挡/中途换人都会掉分。
	for i := 1; i < len(vecs); i++ {
		if score := s.matcher.Compare(vecs[i-1], vecs[i]); score < faceEnrollStability {
			return FaceProfile{}, fmt.Errorf(
				"%w: 相邻两帧差异过大(相似度 %.2f), 请保持姿态稳定后重试",
				ErrFaceLowQuality, score)
		}
	}
	template, err := AverageTemplates(vecs)
	if err != nil {
		return FaceProfile{}, err
	}
	now := s.now()
	profile := FaceProfile{
		TenantID:   tenantID,
		UserID:     userID,
		Template:   template,
		Dim:        len(template),
		Matcher:    s.matcher.Name(),
		Assurance:  s.matcher.Assurance(),
		Quality:    qualitySum / float64(len(vecs)),
		Frames:     len(vecs),
		EnrolledAt: now,
		UpdatedAt:  now,
	}
	if err := s.store.PutFace(ctx, profile); err != nil {
		return FaceProfile{}, err
	}
	u.FaceEnrolled = true
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return FaceProfile{}, err
	}
	s.logger.InfoContext(ctx, "人脸模板已录入",
		"tenant", tenantID, "user_id", userID, "frames", len(vecs), "matcher", s.matcher.Name())
	return profile, nil
}

// DeleteFace 删除人脸模板(个人中心里的"移除人脸")。
func (s *Service) DeleteFace(ctx context.Context, tenantID, userID string) error {
	if err := s.store.DeleteFace(ctx, tenantID, userID); err != nil {
		return err
	}
	if u, err := s.store.GetUser(ctx, tenantID, userID); err == nil {
		u.FaceEnrolled = false
		if err := s.store.UpdateUser(ctx, u); err != nil {
			return err
		}
	}
	s.logger.InfoContext(ctx, "人脸模板已删除", "tenant", tenantID, "user_id", userID)
	return nil
}

// FaceStatus 返回人脸录入状态(不返回模板本身)。
func (s *Service) FaceStatus(ctx context.Context, tenantID, userID string) (FaceProfile, error) {
	return s.store.GetFace(ctx, tenantID, userID)
}

// ListUsers 列出本租户账号(调用方负责脱敏展示与权限校验)。
func (s *Service) ListUsers(ctx context.Context, tenantID string, limit int) ([]User, error) {
	return s.store.ListUsers(ctx, tenantID, limit)
}

// DeleteAccount 删除账号及其人脸模板与全部会话(数据主体权利)。
func (s *Service) DeleteAccount(ctx context.Context, tenantID, userID string) error {
	return s.store.DeleteUser(ctx, tenantID, userID)
}

// Get 读取账号。
func (s *Service) Get(ctx context.Context, tenantID, userID string) (User, error) {
	return s.store.GetUser(ctx, tenantID, userID)
}

// CandidateRef 由账号派生候选人引用值, 用来把"登录的人"与"招聘流程里的候选人"对上。
//
// 规则必须与接入层导入候选人时完全一致: 优先用规范化的手机号, 其次是规范化邮箱。
// 两边不一致的话, 候选人登录后就会看到"没有面试记录" —— 而数据其实在库里,
// 这种问题排查起来非常费劲。
func (s *Service) CandidateRef(u User) string {
	id := CanonicalIdentifier(u.Email, u.Phone)
	if id == "" {
		return ""
	}
	return privacy.Ref(s.secret, u.TenantID, id)
}

// CanonicalIdentifier 把联系方式归一成"用于派生的唯一标识"。
func CanonicalIdentifier(email, phone string) string {
	if normalized, err := NormalizePhone(phone); err == nil && normalized != "" {
		return normalized
	}
	return NormalizeEmail(email)
}

// IsCredentialError 判断错误是否属于"凭据类"错误, 便于接入层统一处理。
func IsCredentialError(err error) bool {
	return errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrUserDisabled)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
