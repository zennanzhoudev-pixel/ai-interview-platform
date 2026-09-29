// Package store 提供面试数据的持久化与断线重连快照。
//
// 三种实现共用同一套行为契约(见 contract_test.go):
//   - memory: 零依赖, 本地演示与单元测试的默认后端;
//   - mysql:  生产环境的业务主数据;
//   - redis:  会话快照(Checkpoint)与热点状态。
//
// 两条贯穿全包的硬规则:
//
//  1. **所有读取都必须带租户**: 这不是"多传一个参数", 而是安全边界。
//     少了它, 知道 session_id 的人就能读到别家公司的面试报告。
//  2. **候选人原文不落库**: 客户传入的候选人 ID 只以假名引用值存在,
//     由 internal/privacy 生成。
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示记录不存在。跨租户访问同样返回它 ——
// 用 NotFound 而不是 Forbidden, 避免通过错误码探测"这个 ID 是否存在"。
var ErrNotFound = errors.New("store: 记录不存在")

// ErrConflict 表示违反唯一约束(例如同一职位下重复投递)。
//
// 它必须与 ErrNotFound 分开: 一个是"你看不到", 一个是"你不能这么做",
// 客户端对两者的处理完全不同(前者刷新, 后者改输入)。
var ErrConflict = errors.New("store: 记录冲突")

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
	// (ID / Status / Round), 前端读 s.session_id 只会得到 undefined。
	ID       string `json:"session_id"`
	TenantID string `json:"tenant_id"`
	// 展示元信息。它们不是"为了好看"才加的: 报告要能显示"云杉科技 ·
	// 高级后端工程师", 候选人界面要能称呼对方名字。
	Position        string `json:"position"`
	Company         string `json:"company"`
	CandidateName   string `json:"candidate_name"`
	InterviewerName string `json:"interviewer_name"`
	// CandidateRef 是候选人的假名引用值(HMAC), 客户传入的原始 ID 不落库。
	// 跨表关联、按候选人聚合统计都靠它。
	CandidateRef string `json:"candidate_ref"`
	// ApplicationID 把这场面试挂到招聘管道上。
	//
	// 它是一个业务不变量, 不是一个"方便查询的冗余字段": 招聘看板上
	// "这个候选人走到第几轮、每轮结论是什么"必须由管道数据回答,
	// 而管道数据只有知道"哪场面试属于哪次投递"才能被更新。
	ApplicationID string `json:"application_id,omitempty"`
	// ResumeJSON 是结构化简历实体的 JSON, 由解析器产出、带原文偏移。
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
	TenantID     string  `json:"tenant_id"`
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
type Report struct {
	TenantID       string    `json:"tenant_id"`
	SessionID      string    `json:"session_id"`
	Recommendation string    `json:"recommendation"`
	Confidence     float64   `json:"confidence"`
	Score          int       `json:"score"`
	Payload        []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// Consent 是候选人数据处理授权记录。
//
// IP 与 UA 都经过脱敏: 取证需要的是"某次授权来自哪个网络段",
// 不是精确到个人的定位信息。
type Consent struct {
	TenantID    string    `json:"tenant_id"`
	SessionID   string    `json:"session_id"`
	CandidateID string    `json:"candidate_id"` // 已脱敏的候选人引用值
	Scope       string    `json:"scope"`        // recording / scoring / retention
	AgreedAt    time.Time `json:"agreed_at"`
	IP          string    `json:"ip"` // 网段级
	UserAgent   string    `json:"user_agent"`
}

// AuditEntry 是审计日志: 谁在什么时候对哪个对象做了什么。
type AuditEntry struct {
	ID        int64     `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// 审计动作常量。集中定义避免各处拼字符串, 也让"哪些操作被审计了"一眼可见。
const (
	AuditSessionCreate = "session.create"
	AuditSessionFinish = "session.finish"
	AuditReportView    = "report.view"
	AuditScoreOverride = "score.override"
	AuditConsentWrite  = "consent.write"
	AuditDataExport    = "data.export"
	AuditDataErase     = "data.erase"
	AuditKeyCreate     = "key.create"
	AuditKeyRevoke     = "key.revoke"
	// 招聘业务域的审计动作。命名统一为 "<对象>.<动作>",
	// 让"这个人的档案被谁改过"这种问题可以用一条前缀查询回答。
	AuditJobUpsert         = "job.upsert"
	AuditCandidateUpsert   = "candidate.upsert"
	AuditApplicationUpsert = "application.upsert"
	AuditQuestionUpsert    = "question.upsert"
	AuditQuestionDelete    = "question.delete"
	AuditScheduleUpsert    = "schedule.upsert"
	AuditRecordingUpload   = "recording.upload"
	AuditRecordingView     = "recording.view"
	AuditRecordingDelete   = "recording.delete"
	AuditCodeRun           = "code.run"
	AuditObserverJoin      = "observer.join"
	// AuditVideoSignal 记录视频信令的建立与断开。
	//
	// 信令本身不含内容, 但"有人进入过这场面试的视频间"是必须留痕的事实:
	// 面试录像里出现谁、谁进过房间, 属于候选人有权知道的信息。
	AuditVideoSignal = "video.signal"
)

// CandidateExport 是候选人数据的导出包(个保法"可携带权")。
type CandidateExport struct {
	CandidateRef string    `json:"candidate_ref"`
	TenantID     string    `json:"tenant_id"`
	ExportedAt   time.Time `json:"exported_at"`
	Sessions     []Session `json:"sessions"`
	Turns        []Turn    `json:"turns"`
	Reports      []Report  `json:"reports"`
	Consents     []Consent `json:"consents"`
}

// Analytics 是租户维度的聚合统计。
//
// 之所以由存储层提供而不是让接入层遍历会话自己算: 前者是一条聚合 SQL,
// 后者是 N+1 次查询。看板会被频繁刷新, 这种差别在真实数据量下就是
// "秒开"和"把数据库拖慢"的区别。
type Analytics struct {
	Sessions         int            `json:"sessions"`
	Finished         int            `json:"finished"`
	Running          int            `json:"running"`
	ByRecommendation map[string]int `json:"by_recommendation"`
	ByStatus         map[string]int `json:"by_status"`
	AvgScore         float64        `json:"avg_score"`
	TotalTurns       int            `json:"total_turns"`
	ScoredTurns      int            `json:"scored_turns"`
	DegradedTurns    int            `json:"degraded_turns"`
	ProbeTurns       int            `json:"probe_turns"`
	// ByCompetency: 能力项 -> 等级 -> 出现次数, 用于"哪类考点整体偏弱"的分析。
	ByCompetency map[string]map[string]int `json:"by_competency"`
}

// SessionStore 是业务主数据的持久化接口。
//
// 所有读取方法都要求 tenantID: 调用方必须从认证主体里取, 而不是接受
// 客户端传入的参数 —— 接口签名本身就是在强制这件事。
type SessionStore interface {
	// 业务域(职位/候选人/投递/题库/安排/录制件)也由同一个存储实现提供。
	// 嵌入而不是并列: 任何调用方拿到的都是一个"完整的存储", 不会出现
	// "会话存在但职位查不到"这种半可用状态。
	BusinessStore

	CreateSession(ctx context.Context, s Session) error
	UpdateSession(ctx context.Context, s Session) error
	// CreateSessionWithConsents 在同一个事务里创建会话并写入授权留痕。
	//
	// 为什么需要组合方法而不是让上层连调三次: "会话建好了但授权没记上"
	// 意味着我们持有录音却没有同意凭据 —— 这是合规事故, 不是数据不一致。
	// 单表写入方法各自都正确, 组合起来仍然可能违反业务不变量, 所以
	// 事务边界必须由存储层按业务语义划定。
	CreateSessionWithConsents(ctx context.Context, s Session, consents []Consent) error
	// FinishSession 在同一个事务里保存报告并更新会话终态。
	FinishSession(ctx context.Context, s Session, r Report) error
	GetSession(ctx context.Context, tenantID, sessionID string) (Session, error)
	ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error)

	// AppendTurn 追加一次问答。
	// 重复投递同一个 Index 必须是幂等的 —— 消息队列至少一次投递是常态。
	AppendTurn(ctx context.Context, t Turn) error
	ListTurns(ctx context.Context, tenantID, sessionID string) ([]Turn, error)

	SaveReport(ctx context.Context, r Report) error
	GetReport(ctx context.Context, tenantID, sessionID string) (Report, error)

	SaveConsent(ctx context.Context, c Consent) error
	ListConsents(ctx context.Context, tenantID, sessionID string) ([]Consent, error)

	// AppendAudit 写审计日志。审计是"只追加"的, 因此没有更新与删除接口。
	AppendAudit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error)

	// ExportCandidate 导出某个候选人的全部数据(可携带权)。
	ExportCandidate(ctx context.Context, tenantID, candidateRef string) (CandidateExport, error)
	// EraseCandidate 删除某个候选人的全部数据(删除权), 返回删除的会话数。
	EraseCandidate(ctx context.Context, tenantID, candidateRef string) (int, error)
	// Analytics 返回租户维度的聚合统计。
	Analytics(ctx context.Context, tenantID string) (Analytics, error)

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
