// Package account 提供"人"的账号体系: 注册、密码登录、人脸登录、个人中心。
//
// 它与 internal/auth 的分工:
//   - auth 回答"这个请求是谁发来的"(API Key / 会话令牌 / 角色权限矩阵);
//   - account 回答"这个人是谁、怎么证明他是他"(账号、密码、人脸、登录会话)。
//
// API Key 面向机器(客户 ATS 集成), 账号面向人(招聘同学、面试官、候选人)。
// 这两件事必须分开: 把管理员密钥交给一个人, 就等于给了他一个不可吊销身份、
// 也无法追溯"是谁操作的"。
package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 密码哈希参数。
//
// 用 PBKDF2-HMAC-SHA256 而不是 argon2id/bcrypt 的原因很实际:
// 这个项目的依赖清单里没有 golang.org/x/crypto, 而把构建链拴在
// 一个需要额外下载的模块上, 会让"离线也能构建"这个特性失效。
// PBKDF2 是 RFC 8018 的简单构造, 用标准库的 crypto/hmac + crypto/sha256
// 就能正确实现, 而且有公开测试向量可以对照(见 password_test.go)。
//
// 生产环境的更优选择是 argon2id(抗 GPU 更强)。哈希串里记录了算法与参数,
// 因此将来换算法时可以做"登录时透明升级", 不需要让用户改密码。
const (
	pbkdf2Algorithm = "pbkdf2-sha256"
	// DefaultIterations 取 OWASP 对 PBKDF2-HMAC-SHA256 建议量级的下半区:
	// 60 万次在现代 CPU 上约 0.3-0.5 秒, 对"登录"这种低频操作可以接受,
	// 但在测试里跑几十次会明显变慢 —— 因此测试用显式的低迭代次数构造。
	DefaultIterations = 210_000
	saltBytes         = 16
	keyBytes          = 32
)

// ErrInvalidHash 表示哈希串格式不合法。
var ErrInvalidHash = errors.New("account: 密码哈希格式不合法")

// PasswordHasher 负责哈希与校验密码。迭代次数可配置, 便于测试。
type PasswordHasher struct {
	iterations int
}

// NewPasswordHasher 构造密码哈希器。iterations <= 0 时使用默认值。
func NewPasswordHasher(iterations int) *PasswordHasher {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	return &PasswordHasher{iterations: iterations}
}

// Iterations 返回当前迭代次数(便于测试与自检展示)。
func (h *PasswordHasher) Iterations() int { return h.iterations }

// Hash 生成 `算法$迭代次数$盐$派生密钥` 形式的哈希串。
//
// 把参数写进哈希串而不是只存派生密钥: 将来调参或换算法时,
// 老用户仍然能登录(用他注册时的参数校验), 然后被透明升级。
func (h *PasswordHasher) Hash(plain string) (string, error) {
	if len(plain) == 0 {
		return "", errors.New("account: 密码不能为空")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := PBKDF2SHA256([]byte(plain), salt, h.iterations, keyBytes)
	return fmt.Sprintf("%s$%d$%s$%s",
		pbkdf2Algorithm, h.iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify 校验密码。第二个返回值表示"是否需要按当前参数重新哈希"。
func (h *PasswordHasher) Verify(encoded, plain string) (bool, bool) {
	algorithm, iterations, salt, want, err := splitHash(encoded)
	if err != nil || algorithm != pbkdf2Algorithm {
		return false, false
	}
	got := PBKDF2SHA256([]byte(plain), salt, iterations, len(want))
	ok := subtle.ConstantTimeCompare(got, want) == 1
	return ok, ok && iterations != h.iterations
}

func splitHash(encoded string) (algorithm string, iterations int, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	return parts[0], iterations, salt, key, nil
}

// PBKDF2SHA256 实现 RFC 8018 定义的 PBKDF2, 伪随机函数为 HMAC-SHA256。
//
// 这是标准算法的直接实现(不是自创方案), 并配有公开测试向量。
// 之所以自己写: 见文件顶部关于依赖的说明。
func PBKDF2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	if iterations <= 0 {
		iterations = 1
	}
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)

	mac := hmac.New(sha256.New, password)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)

	for block := 1; block <= blocks; block++ {
		// U1 = PRF(P, S || INT_BE(block))
		mac.Reset()
		mac.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		mac.Write(buf)
		u = mac.Sum(u[:0])
		copy(t, u)

		// Un = PRF(P, U_{n-1});  T = U1 xor U2 xor ... xor Uc
		for i := 1; i < iterations; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// RandomToken 生成 URL 安全的高熵随机串, 用于登录会话令牌。
func RandomToken(bytes int) (string, error) {
	if bytes <= 0 {
		bytes = 32
	}
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回令牌的存储形态。
//
// 登录令牌和密码一样属于凭据: 数据库被读走时不应直接拿到可用的会话。
// 令牌本身是高熵随机值, 因此用一次 SHA-256 足够(不需要慢哈希)。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
