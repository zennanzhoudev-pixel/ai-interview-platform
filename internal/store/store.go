// Package store 提供面试数据的持久化与断线重连快照。
//
// 三种实现共用同一套行为契约(见 contract_test.go):
//   - memory: 零依赖, 本地演示与单元测试的默认后端;
//   - mysql:  生产环境的业务主数据;
//   - redis:  会话快照(Checkpoint)与热点状态。
//
// 契约测试的意义在于: 换存储不应该改变任何业务行为。把这句话写成
// 可执行的用例, 才能保证"本地用内存跑通"和"线上用 MySQL 跑"不会
// 悄悄出现语义差异 —— 这类差异通常要等到数据错乱才被发现。
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("store: 记录不存在")

// SessionStatus 是会话状态。
type SessionStatus string

const (
	StatusRunning  SessionStatus = "running"
	StatusFinished SessionStatus = "finished"
	StatusAborted  SessionStatus = "aborted"
)

// Session 是一次面试会话的持久化视图。
type Session struct {
	ID             string
	TenantID       string
	Round          int
	Minutes        int
	Stage          string
	Status         SessionStatus
	Recommendation string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Turn 是一次问答的持久化视图。
type Turn struct {
	SessionID    string
	Index        int
	Stage        string
	QuestionID   string
	Competency   string
	Question     string
	Answer       string
	DurationMS   int64
	IsProbe      bool
	Scored       bool
	Level        string
	LevelNum     int
	Confidence   float64
	DegradedFrom string
	// Verdict 是完整评分结论的 JSON。等级/置信度另外拆成了标量列用于
	// SQL 统计(分数分布监控), 但报告要能精确复现 —— 只存几个标量
	// 会让断线重连后的报告丢掉分歧、仲裁等过程信息。
	Verdict   []byte
	CreatedAt time.Time
}

// Report 是评估报告的持久化视图。
// Payload 保存完整报告 JSON: 报告必须可复现, 而不是只存一个结论字符串。
type Report struct {
	SessionID      string
	Recommendation string
	Confidence     float64
	Payload        []byte
	CreatedAt      time.Time
}

// Consent 是候选人数据处理授权记录。
//
// 它不是"合规装饰": 招聘场景下录音属于个人信息, 缺少这条记录,
// 后续任何一次数据使用都没有依据, 也无法证明当时确实告知过。
type Consent struct {
	SessionID   string
	CandidateID string
	Scope       string // recording / scoring / retention
	AgreedAt    time.Time
	IP          string
	UserAgent   string
}

// SessionStore 是业务主数据的持久化接口。
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	UpdateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error)

	// AppendTurn 追加一次问答。
	// 重复投递同一个 Index 必须是幂等的 —— 消息队列至少一次投递是常态,
	// 如果这里报错或重复写入, 面试记录就会出现空洞或重复。
	AppendTurn(ctx context.Context, t Turn) error
	ListTurns(ctx context.Context, sessionID string) ([]Turn, error)

	SaveReport(ctx context.Context, r Report) error
	GetReport(ctx context.Context, sessionID string) (Report, error)

	SaveConsent(ctx context.Context, c Consent) error
	ListConsents(ctx context.Context, sessionID string) ([]Consent, error)

	Close() error
}

// CheckpointStore 保存面试会话快照, 用于断线重连。
//
// 快照必须带 TTL: 候选人弃面的会话如果永久留着, 秋招峰值下会把
// Redis 悄悄撑爆 —— 而且是在最不该出事的那几天出事。
type CheckpointStore interface {
	Save(ctx context.Context, sessionID string, payload []byte, ttl time.Duration) error
	Load(ctx context.Context, sessionID string) ([]byte, error)
	Delete(ctx context.Context, sessionID string) error
}
