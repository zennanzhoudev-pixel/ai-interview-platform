package account

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
)

// 账号相关的错误。它们会被接入层翻译成 4xx —— 哪些错误该让用户看到细节,
// 哪些必须含糊其辞, 是有意区分的:
//   - "邮箱已被注册"必须明确(否则用户无法完成注册);
//   - "账号或密码不正确"必须含糊(否则注册接口就变成了账号枚举器)。
var (
	ErrUserExists        = errors.New("account: 该账号已被注册")
	ErrUserNotFound      = errors.New("account: 账号不存在")
	ErrInvalidCredential = errors.New("account: 账号或密码不正确")
	ErrUserDisabled      = errors.New("account: 账号已被停用")
	ErrFaceNotEnrolled   = errors.New("account: 该账号还没有录入人脸")
	ErrFaceMismatch      = errors.New("account: 人脸比对未通过")
	ErrFaceLowQuality    = errors.New("account: 采集到的人脸图片质量不足")
	ErrSessionExpired    = errors.New("account: 登录已过期, 请重新登录")
	ErrInviteRequired    = errors.New("account: 企业账号需要邀请码")
	ErrWeakPassword      = errors.New("account: 密码强度不足")
	ErrInvalidInput      = errors.New("account: 输入不合法")
)

// User 是一个账号。
//
// 这里不存明文密码, 也不存明文手机号/邮箱? —— 手机号与邮箱**必须**明文存,
// 因为登录要用它们做唯一键与精确匹配; 而它们属于个人信息, 因此:
//   - 列表接口默认脱敏展示(见 Masked 方法);
//   - 随账号一起支持删除(数据主体权利)。
//
// 与之相对, 候选人档案里的联系方式是脱敏存储的(见 store.Candidate),
// 因为那里只需要展示, 不需要用它登录。
type User struct {
	ID           string    `json:"user_id"`
	TenantID     string    `json:"tenant_id"`
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	Phone        string    `json:"phone"`
	Role         auth.Role `json:"role"`
	PasswordHash string    `json:"-"`
	// FaceEnrolled 只是"是否已录入"的标记, 模板本身存在单独的表里。
	FaceEnrolled bool      `json:"face_enrolled"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastLoginAt  time.Time `json:"last_login_at"`
}

// Active 表示账号可以登录。
func (u User) Active() bool { return u.Status == "" || u.Status == "active" }

// Staff 表示这是企业成员(能进工作台)。
func (u User) Staff() bool { return auth.StaffRole(u.Role) }

// Masked 返回用于列表展示的脱敏副本。
//
// 账号列表会被管理员、审计人员看到, 而他们并不需要完整手机号 ——
// 完整值只在本人登录后的个人中心里出现。
func (u User) Masked() User {
	out := u
	out.Email = maskEmail(u.Email)
	out.Phone = maskPhone(u.Phone)
	return out
}

var (
	emailRE = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	// 中国大陆手机号: 1 开头, 第二位 3-9, 共 11 位。
	// 只做形态校验, 不做归属地判断 —— 后者需要运营商数据, 而且会误伤
	// 虚拟号段与企业号段。
	cnPhoneRE = regexp.MustCompile(`^1[3-9]\d{9}$`)
)

// NormalizeEmail 规范化邮箱(去空格 + 转小写)。
//
// 必须规范化后再做唯一键: "Chen@Example.com " 和 "chen@example.com"
// 是同一个人, 如果按原样比较, 同一个人能注册出两个账号,
// 而"我的报告去哪了"这种问题几乎无法解释。
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// NormalizePhone 规范化手机号到 +86 形式。
func NormalizePhone(raw string) (string, error) {
	digits := strings.Builder{}
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' || r == '-' || r == ' ' || r == '(' || r == ')':
			// 允许常见分隔符, 但只保留数字。
		default:
			return "", fmt.Errorf("%w: 手机号包含非法字符", ErrInvalidInput)
		}
	}
	value := digits.String()
	switch {
	case strings.HasPrefix(value, "86") && len(value) == 13:
		value = value[2:]
	case strings.HasPrefix(value, "0086") && len(value) == 15:
		value = value[4:]
	}
	if !cnPhoneRE.MatchString(value) {
		return "", fmt.Errorf("%w: 手机号格式不正确", ErrInvalidInput)
	}
	return "+86" + value, nil
}

// ValidateEmail 校验并规范化邮箱。
func ValidateEmail(raw string) (string, error) {
	value := NormalizeEmail(raw)
	if value == "" {
		return "", nil
	}
	if !emailRE.MatchString(value) {
		return "", fmt.Errorf("%w: 邮箱格式不正确", ErrInvalidInput)
	}
	return value, nil
}

// ValidatePassword 校验密码强度。
//
// 规则刻意保守而不是"越复杂越好": 强制符号与大小写会把人推向
// "Password1!" 这种可预测的模式, 反而降低实际安全性。
// 这里只要求长度与"不是纯数字/不是常见弱口令"。
func ValidatePassword(plain string) error {
	if len([]rune(plain)) < 8 {
		return fmt.Errorf("%w: 至少 8 个字符", ErrWeakPassword)
	}
	if len(plain) > 128 {
		// 上限是为了防止"用超长密码让哈希计算把 CPU 打满"。
		return fmt.Errorf("%w: 最多 128 个字符", ErrWeakPassword)
	}
	digits := 0
	for _, r := range plain {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits == len([]rune(plain)) {
		return fmt.Errorf("%w: 不能是纯数字", ErrWeakPassword)
	}
	for _, weak := range []string{"password", "12345678", "qwerty", "admin123", "88888888"} {
		if strings.EqualFold(plain, weak) {
			return fmt.Errorf("%w: 该密码过于常见", ErrWeakPassword)
		}
	}
	return nil
}

func maskEmail(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at == len(raw)-1 {
		return raw
	}
	local := []rune(raw[:at])
	if len(local) <= 1 {
		return string(local) + "***" + raw[at:]
	}
	return string(local[0]) + strings.Repeat("*", len(local)-1) + raw[at:]
}

func maskPhone(raw string) string {
	digits := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 7 {
		return raw
	}
	return string(digits[:3]) + "****" + string(digits[len(digits)-4:])
}
