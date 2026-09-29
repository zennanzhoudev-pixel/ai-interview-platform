package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	// ErrTokenInvalid 表示令牌签名不合法或格式错误。
	ErrTokenInvalid = errors.New("auth: 会话令牌无效")
	// ErrTokenExpired 表示令牌已过期。
	ErrTokenExpired = errors.New("auth: 会话令牌已过期")
)

// SessionToken 是发给候选人的面试凭证。
type SessionToken struct {
	SessionID string    `json:"s"`
	TenantID  string    `json:"t"`
	ExpiresAt time.Time `json:"e"`
	// Role 区分"候选人"与"人类面试官旁听席"。
	//
	// 旁听席不能用 API Key 直连 WebSocket: 浏览器的 WebSocket API
	// 无法自定义请求头, 密钥只能塞进 URL, 而 URL 会进访问日志、进浏览器
	// 历史、进 Referer。因此改为先用 API Key 换一张短期票据, 票据只
	// 对一场面试、一个角色有效, 泄漏的代价被压到最小。
	Role string `json:"r,omitempty"`
}

// 令牌角色取值。
//
// 名字带 Token 前缀是有意的: 它们与 Role(账号角色, 如 auth.RoleCandidate)
// 是两个不同维度 —— 令牌角色回答"这张票能进哪种连接", 账号角色回答
// "这个人能做什么"。两者同名会让"候选人令牌"和"候选人账号"混在一起,
// 而它们的权限边界完全不同。
const (
	// TokenRoleCandidate 候选人本人的面试连接。
	TokenRoleCandidate = "candidate"
	// TokenRoleObserver 人类面试官(旁听/接管)。
	TokenRoleObserver = "observer"
)

// TokenRole 返回令牌角色, 空值按候选人处理(兼容旧令牌)。
func (t SessionToken) TokenRole() string {
	if t.Role == "" {
		return TokenRoleCandidate
	}
	return t.Role
}

// IssueSessionToken 签发候选人令牌。
//
// 无状态(HMAC 签名, 不落库): 面试是长连接 + 高频校验的场景, 每次校验都查库
// 既慢又给数据库添无谓压力; 而且令牌天然带过期时间, 不需要额外的清理任务。
func IssueSessionToken(secret []byte, t SessionToken) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("auth: 缺少签名密钥")
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := sign(secret, body)
	return body + "." + sig, nil
}

// VerifySessionToken 校验令牌并返回声明。
func VerifySessionToken(secret []byte, raw string, now time.Time) (SessionToken, error) {
	body, sig, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || body == "" || sig == "" {
		return SessionToken{}, ErrTokenInvalid
	}
	expect := sign(secret, body)
	if !hmac.Equal([]byte(sig), []byte(expect)) {
		return SessionToken{}, ErrTokenInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return SessionToken{}, ErrTokenInvalid
	}
	var t SessionToken
	if err := json.Unmarshal(payload, &t); err != nil {
		return SessionToken{}, ErrTokenInvalid
	}
	if !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt) {
		return SessionToken{}, ErrTokenExpired
	}
	return t, nil
}

func sign(secret []byte, body string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
