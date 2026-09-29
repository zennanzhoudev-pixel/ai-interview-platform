// Package auth 提供认证与授权: API Key、角色权限、候选人会话令牌。
//
// 两种身份模型, 对应两种调用方:
//   - 服务端到服务端(客户 ATS / 招聘系统): 用 API Key, 带角色与租户;
//   - 候选人: 用一次性的会话令牌, 只能访问属于自己的那一场面试。
//
// 候选人不可能持有 API Key, 所以"用 API Key 挡住所有接口"不是一个可用的设计 ——
// 这也是很多原型项目在接入真实业务时才发现的问题。
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrInvalidKey 表示 API Key 不存在或已吊销。
var ErrInvalidKey = errors.New("auth: 无效的 API Key")

// Role 是调用主体角色。
type Role string

const (
	// RoleAdmin 可管理职位、题库、密钥与全部数据。
	RoleAdmin Role = "admin"
	// RoleInterviewer 可查看报告、人工改分、发起复议。
	RoleInterviewer Role = "interviewer"
	// RoleScheduler 是 ATS 集成用的系统账号: 创建面试、读取报告结论。
	RoleScheduler Role = "scheduler"
)

// Permission 是细粒度权限点。
type Permission string

const (
	PermJobWrite      Permission = "job:write"
	PermQuestionWrite Permission = "question:write"
	PermSessionCreate Permission = "session:create"
	PermReportRead    Permission = "report:read"
	PermScoreOverride Permission = "score:override"
	PermAnalyticsRead Permission = "analytics:read"
	PermAuditRead     Permission = "audit:read"
	PermDataErase     Permission = "data:erase"
	PermKeyAdmin      Permission = "key:admin"
	// PermCandidateWrite 管理候选人档案(导入、修改标签、删除)。
	PermCandidateWrite Permission = "candidate:write"
	// PermScheduleWrite 安排面试(时间、面试官、模式)。
	PermScheduleWrite Permission = "schedule:write"
	// PermRecordingRead 观看/下载面试录像。它单独成权限点而不是并进
	// report:read: 报告是结论, 录像是候选人的影像与声音, 两者敏感度
	// 不在一个量级。多数企业的策略是"所有面试官能看报告, 只有被授权
	// 的人能看录像", 权限模型必须能表达这个区别。
	PermRecordingRead Permission = "recording:read"
	// PermCodeRun 在沙箱里执行代码(判题)。
	PermCodeRun Permission = "code:run"
	// PermRecruitRead 读取招聘配置类数据(职位/候选人/投递/题库/面试安排)。
	//
	// 用一个权限点覆盖这一整类"读招聘数据"的接口, 而不是拆成
	// job:read/candidate:read/question:read: 后者的组合空间有 8 种,
	// 而真实的角色划分只有"能看招聘数据"与"不能看"两种。权限点越多,
	// 配置错的概率越高 —— 安全边界应该小到能被完整评审。
	PermRecruitRead Permission = "recruit:read"
	// PermInterviewObserve 进入面试间旁听/参与视频。
	//
	// 它与 report:read 分开是刻意的: 读报告是读"数据", 进面试间是
	// 介入"过程"。客户 ATS 集成账号需要前者(拿结论回写自己的系统),
	// 但绝不应该能出现在候选人的摄像头前 —— 那需要的是对人的责任,
	// 而不是一个系统集成权限。
	PermInterviewObserve Permission = "interview:observe"
)

// rolePermissions 是角色到权限的映射。
//
// 显式写死而不是从数据库读: 权限矩阵是安全边界, 它应该小、稳定、可评审,
// 而不是运行时可改的数据。租户自定义权限是另一个层级的需求, 不该混进来。
var rolePermissions = map[Role]map[Permission]bool{
	RoleAdmin: {
		PermJobWrite: true, PermQuestionWrite: true, PermSessionCreate: true,
		PermReportRead: true, PermScoreOverride: true, PermAnalyticsRead: true,
		PermAuditRead: true, PermDataErase: true, PermKeyAdmin: true,
		PermCandidateWrite: true, PermScheduleWrite: true, PermRecordingRead: true,
		PermCodeRun: true, PermRecruitRead: true,
		PermInterviewObserve: true,
	},
	RoleInterviewer: {
		PermReportRead: true, PermScoreOverride: true, PermAnalyticsRead: true,
		PermAuditRead: true, PermScheduleWrite: true, PermCandidateWrite: true,
		PermCodeRun: true, PermRecruitRead: true,
		PermInterviewObserve: true,
	},
	RoleScheduler: {
		PermSessionCreate: true, PermReportRead: true, PermJobWrite: true,
		PermCandidateWrite: true, PermScheduleWrite: true, PermRecruitRead: true,
	},
}

// Principal 是通过认证的调用主体。
type Principal struct {
	TenantID string
	KeyID    string
	Role     Role
	Name     string
}

// Can 判断主体是否具备某权限。
func (p Principal) Can(perm Permission) bool {
	perms, ok := rolePermissions[p.Role]
	if !ok {
		return false
	}
	return perms[perm]
}

// String 用于日志, 不泄露密钥。
func (p Principal) String() string {
	return fmt.Sprintf("%s/%s@%s", p.Role, p.Name, p.TenantID)
}

// HashKey 返回 API Key 的存储形态。
//
// 只存哈希、不存明文: 数据库被拖库时, 攻击者拿到的是一堆 sha256,
// 而不是可以立刻调用接口的凭据。密钥明文只在创建那一刻返回一次。
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// KeyStore 解析 API Key。
type KeyStore interface {
	Resolve(ctx context.Context, rawKey string) (Principal, error)
}

// KeyRecord 是一条 API Key 记录(不含明文)。
type KeyRecord struct {
	KeyID     string
	TenantID  string
	Role      Role
	Name      string
	KeyHash   string
	CreatedAt time.Time
	RevokedAt time.Time
}

// Revoked 表示该密钥是否已吊销。
func (k KeyRecord) Revoked() bool { return !k.RevokedAt.IsZero() }

// GenerateKey 生成一个新的 API Key 明文(仅在创建时展示一次)。
func GenerateKey() (string, error) {
	buf := make([]byte, 24)
	if err := fillRandom(buf); err != nil {
		return "", err
	}
	return "ik_" + hex.EncodeToString(buf), nil
}

// MemoryKeyStore 是内存实现, 供本地演示与测试使用。
type MemoryKeyStore struct {
	mu   sync.RWMutex
	keys map[string]KeyRecord // key: keyHash
	now  func() time.Time
}

// NewMemoryKeyStore 构造内存密钥库。
func NewMemoryKeyStore() *MemoryKeyStore {
	return &MemoryKeyStore{keys: make(map[string]KeyRecord), now: time.Now}
}

// Add 写入一条密钥记录并返回明文(仅此一次)。
func (m *MemoryKeyStore) Add(tenantID, name string, role Role) (string, Principal, error) {
	raw, err := GenerateKey()
	if err != nil {
		return "", Principal{}, err
	}
	rec := KeyRecord{
		KeyID:     "key_" + HashKey(raw)[:12],
		TenantID:  tenantID,
		Role:      role,
		Name:      name,
		KeyHash:   HashKey(raw),
		CreatedAt: m.now(),
	}
	m.mu.Lock()
	m.keys[rec.KeyHash] = rec
	m.mu.Unlock()
	return raw, Principal{TenantID: tenantID, KeyID: rec.KeyID, Role: role, Name: name}, nil
}

// Resolve 实现 KeyStore。
func (m *MemoryKeyStore) Resolve(_ context.Context, rawKey string) (Principal, error) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return Principal{}, ErrInvalidKey
	}
	hash := HashKey(rawKey)

	m.mu.RLock()
	rec, found := m.keys[hash]
	m.mu.RUnlock()
	if !found {
		// 常数时间比较: 即使这里只是查 map, 也保持"比较凭据"的写法一致,
		// 避免未来换成遍历实现时引入时序侧信道。
		subtle.ConstantTimeCompare([]byte(hash), []byte(HashKey(rawKey)))
		return Principal{}, ErrInvalidKey
	}
	if rec.Revoked() {
		return Principal{}, ErrInvalidKey
	}
	return Principal{TenantID: rec.TenantID, KeyID: rec.KeyID, Role: rec.Role, Name: rec.Name}, nil
}

// Revoke 吊销密钥。
func (m *MemoryKeyStore) Revoke(keyID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, rec := range m.keys {
		if rec.KeyID == keyID {
			rec.RevokedAt = m.now()
			m.keys[h] = rec
			return true
		}
	}
	return false
}

// List 返回全部密钥记录(不含明文)。
func (m *MemoryKeyStore) List() []KeyRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]KeyRecord, 0, len(m.keys))
	for _, rec := range m.keys {
		out = append(out, rec)
	}
	return out
}
