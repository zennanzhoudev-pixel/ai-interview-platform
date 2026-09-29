package store

import (
	"context"
	"strings"
	"time"
)

// 本文件是"面试之外"的业务域: 职位、候选人档案、投递流程(管道)、题库、面试安排、录制件。
//
// 为什么这些必须和面试会话放在同一个存储契约里, 而不是各自一个微服务:
// 它们共享同一批业务不变量 —— 一个候选人不能同时在两个职位上处于"已发 offer",
// 一场面试安排必须挂在一个存在的投递上, 录制件的保留期跟着投递走。
// 这些不变量在跨服务后会立刻退化成"最终一致", 而招聘流程恰恰是
// 人对着同一份数据做判断的场景, 看到两个互相矛盾的结论是不可接受的。

/* ---------------- 职位 ---------------- */

// JobStatus 是职位状态。
type JobStatus string

const (
	JobDraft  JobStatus = "draft"
	JobOpen   JobStatus = "open"
	JobPaused JobStatus = "paused"
	JobClosed JobStatus = "closed"
)

// RoundSpec 是职位的某一轮面试如何编排。
//
// 把"1 到 5 面"做成数据而不是写死在代码里: 不同岗位的轮次构成完全不同
// (研发 5 轮、职能 3 轮), 写死意味着每来一个新岗位就发一次版。
type RoundSpec struct {
	Round   int    `json:"round"`
	Name    string `json:"name"` // 一面 / 二面 / 终面
	Minutes int    `json:"minutes"`
	// AILead 表示本轮的提问与追问由 AI 主导; false 时 AI 只做辅助与纪要。
	AILead bool `json:"ai_lead"`
	// HumanPanel 表示需要人类面试官进入面试间(视频链路会给这类轮次开启)。
	HumanPanel bool `json:"human_panel"`
	// Mode 决定候选人界面形态: video / audio / text / coding。
	Mode  string   `json:"mode"`
	Focus []string `json:"focus"`
}

// Job 是一个招聘职位。
type Job struct {
	ID         string    `json:"job_id"`
	TenantID   string    `json:"tenant_id"`
	Title      string    `json:"title"`
	Department string    `json:"department"`
	Level      string    `json:"level"`
	Location   string    `json:"location"`
	Headcount  int       `json:"headcount"`
	Status     JobStatus `json:"status"`
	// CompetencyModel 是能力项权重("这个岗位看重什么"), 用于解释报告排序,
	// 也用于把不同岗位的分数拉回同一个可比的量纲。
	CompetencyModel map[string]int `json:"competency_model"`
	Rounds          []RoundSpec    `json:"rounds"`
	Owner           string         `json:"owner"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// RoundSpecOf 返回指定轮次的编排配置。
func (j Job) RoundSpecOf(n int) (RoundSpec, bool) {
	for _, r := range j.Rounds {
		if r.Round == n {
			return r, true
		}
	}
	return RoundSpec{}, false
}

// NormalizeRounds 补齐 1..5 轮的默认编排, 让"新建职位"不必先填五轮表单。
func NormalizeRounds(rounds []RoundSpec) []RoundSpec {
	defaults := map[int]RoundSpec{
		1: {Round: 1, Name: "一面 · 技术基础", Minutes: 45, AILead: true, Mode: "video",
			Focus: []string{"project_depth", "language_core"}},
		2: {Round: 2, Name: "二面 · 编程与实战", Minutes: 60, AILead: true, Mode: "coding",
			Focus: []string{"coding", "project_depth"}},
		3: {Round: 3, Name: "三面 · 系统设计", Minutes: 60, HumanPanel: true, Mode: "video",
			Focus: []string{"architecture", "distributed_system"}},
		4: {Round: 4, Name: "四面 · 交叉面", Minutes: 45, HumanPanel: true, Mode: "video",
			Focus: []string{"tech_choice", "architecture"}},
		5: {Round: 5, Name: "五面 · HR 与意向", Minutes: 30, HumanPanel: true, Mode: "video",
			Focus: []string{"communication"}},
	}
	seen := make(map[int]bool, len(rounds))
	out := make([]RoundSpec, 0, 5)
	for _, r := range rounds {
		if r.Round < 1 || r.Round > 5 || seen[r.Round] {
			continue
		}
		if r.Name == "" {
			r.Name = defaults[r.Round].Name
		}
		if r.Minutes <= 0 {
			r.Minutes = defaults[r.Round].Minutes
		}
		if r.Mode == "" {
			r.Mode = defaults[r.Round].Mode
		}
		seen[r.Round] = true
		out = append(out, r)
	}
	for r := 1; r <= 5; r++ {
		if !seen[r] {
			out = append(out, defaults[r])
		}
	}
	sortRounds(out)
	return out
}

func sortRounds(rounds []RoundSpec) {
	for i := 1; i < len(rounds); i++ {
		for j := i; j > 0 && rounds[j].Round < rounds[j-1].Round; j-- {
			rounds[j], rounds[j-1] = rounds[j-1], rounds[j]
		}
	}
}

/* ---------------- 候选人档案 ---------------- */

// Candidate 是候选人档案。
//
// 两个字段刻意分开: Ref 是不可逆假名(主键, 用于一切关联), Name/Email/Phone
// 是受权限保护的个人信息。系统内部关联一律用 Ref, 只有明确需要展示的接口
// 才返回个人信息 —— 这样"误把候选人列表打进日志"不会泄露身份。
type Candidate struct {
	Ref      string   `json:"candidate_ref"`
	TenantID string   `json:"tenant_id"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Phone    string   `json:"phone"`
	Source   string   `json:"source"`
	Tags     []string `json:"tags"`
	// ResumeSource 是简历原文。存原文是招聘业务的硬需求(人工复核要看),
	// 因此它必须: 1) 只在有 report:read 的接口返回; 2) 随候选人一起被删除。
	ResumeSource string    `json:"-"`
	ResumeJSON   []byte    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

/* ---------------- 投递与管道 ---------------- */

// AppStage 是招聘管道阶段。
type AppStage string

const (
	// StageScreening 简历初筛。
	StageScreening AppStage = "screening"
	// StageAIInterview AI 面试中。
	StageAIInterview AppStage = "ai_interview"
	// StageHuman 人工面试。
	StageHuman AppStage = "human_interview"
	// StageOffer 已发 offer。
	StageOffer AppStage = "offer"
	// StageHired 已入职。
	StageHired AppStage = "hired"
	// StageRejected 已淘汰。
	StageRejected AppStage = "rejected"
	// StageWithdrawn 候选人主动退出。
	StageWithdrawn AppStage = "withdrawn"
)

// AppRound 是一次投递里的某一轮面试状态。
type AppRound struct {
	Round          int        `json:"round"`
	Status         string     `json:"status"` // pending/scheduled/running/finished/skipped
	SessionID      string     `json:"session_id,omitempty"`
	Score          int        `json:"score"`
	Recommendation string     `json:"recommendation,omitempty"`
	ScheduledAt    *time.Time `json:"scheduled_at,omitempty"`
	Interviewer    string     `json:"interviewer,omitempty"`
	Mode           string     `json:"mode,omitempty"`
}

// Application 是一次投递(职位 × 候选人), 也是招聘看板上的一张卡。
type Application struct {
	ID            string     `json:"application_id"`
	TenantID      string     `json:"tenant_id"`
	JobID         string     `json:"job_id"`
	JobTitle      string     `json:"job_title"`
	CandidateRef  string     `json:"candidate_ref"`
	CandidateName string     `json:"candidate_name"`
	Stage         AppStage   `json:"stage"`
	Status        string     `json:"status"` // active/hired/rejected/withdrawn
	CurrentRound  int        `json:"current_round"`
	Rounds        []AppRound `json:"rounds"`
	Owner         string     `json:"owner"`
	Source        string     `json:"source"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// RoundOf 返回某一轮的状态。
func (a Application) RoundOf(n int) (AppRound, bool) {
	for _, r := range a.Rounds {
		if r.Round == n {
			return r, true
		}
	}
	return AppRound{}, false
}

// NormalizeRounds 保证投递的轮次列表覆盖职位的全部轮次, 且顺序稳定。
func (a *Application) NormalizeRounds(rounds []RoundSpec) {
	existing := make(map[int]AppRound, len(a.Rounds))
	for _, r := range a.Rounds {
		existing[r.Round] = r
	}
	out := make([]AppRound, 0, len(rounds))
	for _, spec := range rounds {
		r, ok := existing[spec.Round]
		if !ok {
			r = AppRound{Round: spec.Round, Status: "pending"}
		}
		if r.Mode == "" {
			r.Mode = spec.Mode
		}
		out = append(out, r)
	}
	a.Rounds = out
}

/* ---------------- 题库 ---------------- */

// ReferencePoint 是参考答案里的一个要点, 也是 RAG 检索的最小单元。
type ReferencePoint struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// QuestionItem 是一道可复用的题目(题库条目)。
//
// 它同时是三个东西: 提问脚本、评分要点(rubric 输入)、检索语料。
// 三者共用一份数据是有意的 —— 如果"问的题"和"评的分"来自两处,
// 报告里的评分依据就会和实际问的问题对不上, 而这种错位几乎无法人工发现。
type QuestionItem struct {
	ID              string           `json:"question_id"`
	TenantID        string           `json:"tenant_id"`
	Stage           string           `json:"stage"`
	Competency      string           `json:"competency"`
	Text            string           `json:"text"`
	Keywords        []string         `json:"keywords"`
	AntiPatterns    []string         `json:"anti_patterns"`
	ReferencePoints []ReferencePoint `json:"reference_points"`
	Difficulty      string           `json:"difficulty"` // junior/mid/senior/staff
	Importance      string           `json:"importance"` // low/medium/high
	MaxProbe        int              `json:"max_probe"`
	Status          string           `json:"status"` // draft/reviewing/published/retired
	Version         int              `json:"version"`
	Tags            []string         `json:"tags"`
	// Rounds 记录这道题适用于哪些轮次, 用于按轮次抽题。
	Rounds    []int     `json:"rounds"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Published 表示这道题可以进入面试。
func (q QuestionItem) Published() bool { return q.Status == "published" }

/* ---------------- 面试安排 ---------------- */

// Schedule 是一场已排期的面试。
type Schedule struct {
	ID            string    `json:"schedule_id"`
	TenantID      string    `json:"tenant_id"`
	ApplicationID string    `json:"application_id"`
	JobID         string    `json:"job_id"`
	CandidateRef  string    `json:"candidate_ref"`
	Round         int       `json:"round"`
	Mode          string    `json:"mode"`
	ScheduledAt   time.Time `json:"scheduled_at"`
	DurationMin   int       `json:"duration_min"`
	Interviewer   string    `json:"interviewer"`
	Status        string    `json:"status"` // pending/confirmed/running/done/cancelled/no_show
	SessionID     string    `json:"session_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

/* ---------------- 录制件 ---------------- */

// Recording 是一场面试的音视频录制件元数据。
//
// 二进制内容不进数据库: 一场 45 分钟的视频是几百 MB, 放进 MySQL 会
// 把备份、主从复制和缓冲池全部拖垮。数据库只存元数据与存储键,
// 内容落在对象存储/文件系统, 两者用 StorageKey 关联。
type Recording struct {
	ID           string `json:"recording_id"`
	TenantID     string `json:"tenant_id"`
	SessionID    string `json:"session_id"`
	CandidateRef string `json:"candidate_ref"`
	Kind         string `json:"kind"` // video/audio/screen
	MimeType     string `json:"mime_type"`
	StorageKey   string `json:"-"`
	Chunks       int    `json:"chunks"`
	SizeBytes    int64  `json:"size_bytes"`
	DurationMS   int64  `json:"duration_ms"`
	Status       string `json:"status"` // uploading/complete/failed
	// DeleteAfter 是保留期到期时间。到期后由清理任务删除, 而不是"永久保留" ——
	// 面试录像是个人信息里最敏感的一类, 无期限保留本身就是风险。
	DeleteAfter time.Time `json:"delete_after,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

/* ---------------- 存储接口扩展 ---------------- */

// BusinessStore 是面试之外的业务域持久化接口。
//
// 单独成接口而不是继续往 SessionStore 里塞方法: 会话存储是热路径
// (每场面试几十次写入), 业务域是低频管理操作, 两者的容量规划、
// 索引设计、甚至数据保留策略都不同。分开之后, 想换掉其中一个
// (例如把题库换成 ES) 不会牵动另一个。
type BusinessStore interface {
	CreateJob(ctx context.Context, j Job) error
	UpdateJob(ctx context.Context, j Job) error
	GetJob(ctx context.Context, tenantID, jobID string) (Job, error)
	ListJobs(ctx context.Context, tenantID string) ([]Job, error)

	CreateCandidate(ctx context.Context, c Candidate) error
	UpdateCandidate(ctx context.Context, c Candidate) error
	GetCandidate(ctx context.Context, tenantID, ref string) (Candidate, error)
	ListCandidates(ctx context.Context, tenantID string, limit int) ([]Candidate, error)

	CreateApplication(ctx context.Context, a Application) error
	UpdateApplication(ctx context.Context, a Application) error
	GetApplication(ctx context.Context, tenantID, id string) (Application, error)
	ListApplications(ctx context.Context, tenantID, jobID string) ([]Application, error)
	ListApplicationsByCandidate(ctx context.Context, tenantID, candidateRef string) ([]Application, error)

	CreateQuestion(ctx context.Context, q QuestionItem) error
	UpdateQuestion(ctx context.Context, q QuestionItem) error
	DeleteQuestion(ctx context.Context, tenantID, id string) error
	GetQuestion(ctx context.Context, tenantID, id string) (QuestionItem, error)
	ListQuestions(ctx context.Context, tenantID string) ([]QuestionItem, error)

	CreateSchedule(ctx context.Context, s Schedule) error
	UpdateSchedule(ctx context.Context, s Schedule) error
	GetSchedule(ctx context.Context, tenantID, id string) (Schedule, error)
	ListSchedules(ctx context.Context, tenantID string, from, to time.Time) ([]Schedule, error)

	CreateRecording(ctx context.Context, r Recording) error
	UpdateRecording(ctx context.Context, r Recording) error
	GetRecording(ctx context.Context, tenantID, id string) (Recording, error)
	GetRecordingBySession(ctx context.Context, tenantID, sessionID, kind string) (Recording, error)
	ListRecordings(ctx context.Context, tenantID, sessionID string) ([]Recording, error)
	DeleteRecording(ctx context.Context, tenantID, id string) error
	// PurgeExpiredRecordings 返回保留期已过、应当删除的录制件。
	PurgeExpiredRecordings(ctx context.Context, now time.Time) ([]Recording, error)
}

// ValidStage 判断管道阶段取值是否合法。
func ValidStage(s AppStage) bool {
	switch s {
	case StageScreening, StageAIInterview, StageHuman, StageOffer, StageHired, StageRejected, StageWithdrawn:
		return true
	}
	return false
}

// NormalizeTenant 把租户限定在安全的字符集内。
//
// 租户 ID 会进入对象存储的 key 前缀, 未校验的取值可以让 ".." 一类
// 构造穿透到别人的目录。校验放在类型入口处, 而不是在每个拼接 key 的地方。
func NormalizeTenant(tenant string) string {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range tenant {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	return b.String()
}
