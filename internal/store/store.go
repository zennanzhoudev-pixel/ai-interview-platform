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
	// JSON tag 必须显式写: 没有 tag 时 encoding/json 会用 Go 字段名
	// (ID / Status / Round), 前端读 s.session_id 只会得到 undefined ——
	// 这类"字段名对不上"的问题在前端表现为整块内容空白, 极难定位。
	ID       string `json:"session_id"`
	TenantID string `json:"tenant_id"`
	// 展示元信息。它们不是"为了好看"才加的: 报告要能显示"云杉科技 ·
	// 高级后端工程师 · 第 2 阶段", 候选人界面要能称呼对方名字。
	// 只有一堆 ID 的报告没人愿意读, 而没人读的报告等于没有报告。
	Position        string `json:"position"`
	Company         string `json:"company"`
	CandidateName   string `json:"candidate_name"`
	InterviewerName string `json:"interviewer_name"`
	// ResumeJSON 是结构化简历实体的 JSON, 由解析器产出、带原文偏移。
	// 它是"追问能引用简历原话"的数据基础。
	//
	// 用 json:"-" 排除: 字节切片会被编码成 base64, 而列表接口带上整份
	// 简历原文既浪费带宽, 也没有消费方。
	ResumeJSON     []byte        `json:"-"`
	Round          int           `json:"round"`
	Minutes        int           `json:"minutes"`
	Stage          string        `json:"stage"`
	Status         SessionStatus `json:"status"`
	Recommendation string        `json:"recommendation"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// Turn 是一次问答的持久化视图。
type Turn struct {
	SessionID    string  `json:"session_id"`
	Index        int     `json:"index"`
	Stage        string  `json:"stage"`
	QuestionID   string  `json:"question_id"`
	Competency   string  `json:"competency,omitempty"`
	Question     string  `json:"question"`
	Answer       string  `json:"answer"`
	DurationMS   int64   `json:"duration_ms"`
	IsProbe      bool    `json:"is_probe"`
	Scored       bool    `json:"scored"`
	Level        string  `json:"level,omitempty"`
	LevelNum     int     `json:"level_num"`
	Confidence   float64 `json:"confidence"`
	DegradedFrom string  `json:"degraded_from,omitempty"`
	// Verdict 是完整评分结论的 JSON。等级/置信度另外拆成了标量列用于
	// SQL 统计(分数分布监控), 但报告要能精确复现 —— 只存几个标量
	// 会让断线重连后的报告丢掉分歧、仲裁等过程信息。
	Verdict   []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// Report 是评估报告的持久化视图。
// Payload 保存完整报告 JSON: 报告必须可复现, 而不是只存一个结论字符串。
type Report struct {
	SessionID      string    `json:"session_id"`
	Recommendation string    `json:"recommendation"`
	Confidence     float64   `json:"confidence"`
	Payload        []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// Consent 是候选人数据处理授权记录。
//
// 它不是"合规装饰": 招聘场景下录音属于个人信息, 缺少这条记录,
// 后续任何一次数据使用都没有依据, 也无法证明当时确实告知过。
type Consent struct {
	SessionID   string    `json:"session_id"`
	CandidateID string    `json:"candidate_id"`
	Scope       string    `json:"scope"` // recording / scoring / retention
	AgreedAt    time.Time `json:"agreed_at"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
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
