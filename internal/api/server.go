// Package api 提供面试的 HTTP / WebSocket 接入层。
//
// 接入层刻意保持"薄": 它只做五件事 —— 认证与授权、合规留痕、把请求翻译成
// 引擎调用、把引擎状态推给前端、把结果落库并审计。业务规则一条都不放在这里,
// 否则状态机、预算调度和评分策略会被拆散到各个 handler 里, 再也无法回归。
package api

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/trace"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/account"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/knowledge"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/observability"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/practice"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/recording"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/sandbox"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// Scorers 是一组用于交叉评分的评分器。
type Scorers struct {
	Primary   scoring.Scorer
	Secondary scoring.Scorer
	Arbiter   scoring.Scorer
	Tolerance int
}

// Config 配置接入层。
type Config struct {
	Store store.SessionStore
	// Checkpoint 为 nil 时退化为只用 MySQL: 快照只是热路径缓存,
	// 问答记录本身已经足够恢复会话, 缺少它不影响正确性。
	Checkpoint store.CheckpointStore
	// Keys 是 API Key 库。RequireAuth=true 时必须提供。
	Keys auth.KeyStore
	// Secret 用于签名候选人会话令牌与派生候选人假名。生产必须显式配置:
	// 换成随机值会导致重启后所有令牌失效、假名引用全变。
	Secret []byte

	Scorers      func() Scorers
	ProbePlanner orchestrator.ProbePlanner
	ASR          media.ASRProvider
	TTS          media.TTSProvider

	// KnowledgeFor 按租户构建知识库(题库 + 内置题)。
	//
	// 按租户而不是全局构建, 因为题库是租户数据: 一家公司的题目出现在
	// 另一家公司的面试里, 既是数据泄漏, 也是不可解释的面试结果。
	// 传 nil 时退化为纯内置题库。
	KnowledgeFor func(ctx context.Context, tenant string) (*knowledge.Corpus, error)

	// Sandbox 执行候选人提交的代码。为 nil 时判题接口返回 503。
	Sandbox sandbox.Runner
	// Blobs 保存面试录制件内容。为 nil 时上传接口返回 503。
	Blobs recording.BlobStore
	// RecordingRetention 是录制件保留期, 默认 90 天。
	RecordingRetention time.Duration
	// ICEServers 是 WebRTC 的 STUN/TURN 配置。
	ICEServers []map[string]any
	// ObserverTicketTTL 是旁听票据有效期, 默认 2 小时。
	ObserverTicketTTL time.Duration
	// Pingers 是可被 /system/providers 真实探测的上游能力。
	// key 约定: llm / llm_secondary / embedding / asr / tts。
	Pingers map[string]ProviderPing
	// StoreKind 是存储后端的展示名(用于自检面板如实说明"数据存在哪")。
	StoreKind string
	// AccountStorage 说明账号存在哪: "mysql" 或 "memory"。
	// 内存存储意味着**重启后账号与人脸模板都会消失**, 而这件事必须让
	// 用户看见 —— 否则"我明明录入过人脸"会变成一个查不出原因的问题。
	AccountStorage string
	// Accounts 是账号体系(注册/登录/人脸/个人中心)。
	// 为 nil 时这些接口返回 503, 而基于 API Key 与面试会话令牌的
	// 既有能力不受影响 —— 账号是"给人用的入口", 不是系统运行的前提。
	Accounts *account.Service
	// Practice 是真人双向对练的配对管理器。
	// 为 nil 时对练接口返回 503, 其它能力不受影响。
	Practice *practice.Manager

	Logger     *slog.Logger
	Metrics    *observability.Metrics
	TracerProv trace.TracerProvider

	// TenantID 仅用于 RequireAuth=false 的本地演示模式。
	TenantID string
	// RequireAuth 在生产必须为 true。关掉它意味着任何人都能读任意租户的数据。
	RequireAuth bool

	// StoreTimeout 是单次存储操作的超时上限。默认 5 秒。
	StoreTimeout time.Duration
	// SessionTokenTTL 是候选人令牌有效期, 默认 6 小时。
	SessionTokenTTL time.Duration

	RateLimitPerSecond float64
	RateLimitBurst     float64
}

// Server 是面试的 HTTP / WebSocket 接入层。
type Server struct {
	cfg       Config
	mux       *http.ServeMux
	upgrader  websocket.Upgrader
	logger    *slog.Logger
	metrics   *observability.Metrics
	tracer    trace.Tracer
	limiter   *limiter
	rooms     *roomHub
	knowledge *knowledgeCache

	// knowledgeRebuildMu 串行化知识库重建。
	// 题库可能被连续提交多次(批量导入), 并发重建会让最后一次写覆盖前一次,
	// 出现"改了 10 道题, 只生效了 3 道"。
	knowledgeRebuildMu sync.Mutex
}

// NewServer 构造接入层。
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	if cfg.Scorers == nil {
		cfg.Scorers = DefaultScorers
	}
	if cfg.StoreTimeout <= 0 {
		cfg.StoreTimeout = 5 * time.Second
	}
	if cfg.SessionTokenTTL <= 0 {
		cfg.SessionTokenTTL = 6 * time.Hour
	}
	if cfg.Metrics == nil {
		cfg.Metrics = observability.NewMetrics()
	}
	if cfg.RecordingRetention <= 0 {
		cfg.RecordingRetention = 90 * 24 * time.Hour
	}
	if cfg.ObserverTicketTTL <= 0 {
		cfg.ObserverTicketTTL = 2 * time.Hour
	}
	// 签名密钥: 生产由命令行强制要求(见 cmd/interviewd);
	// 这里兜底生成进程内临时密钥, 让本地演示与测试不必先配密钥。
	// 代价是重启后已发出的候选人链接失效 —— 因此必须显式告警。
	if len(cfg.Secret) == 0 {
		buf := make([]byte, 32)
		_, _ = rand.Read(buf)
		cfg.Secret = buf
		cfg.Logger.Warn("未提供签名密钥, 已生成进程内临时密钥; 重启后候选人链接会失效(仅适用于本地演示)")
	}

	s := &Server{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		logger:  cfg.Logger,
		metrics: cfg.Metrics,
		tracer:  observability.Tracer("interviewd.api"),
		limiter: newLimiter(cfg.RateLimitPerSecond, cfg.RateLimitBurst),
		rooms:   newRoomHub(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 同源部署: 浏览器来的连接必须来自本机 host。
			// 空 Origin 放行是为了让 curl 与测试客户端也能连。
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == "" || sameHost(origin, r.Host)
			},
		},
	}
	s.rooms.onJoin = func(sessionID, role string) {
		s.metrics.VideoPeers.WithLabelValues(role).Inc()
	}
	s.rooms.onLeave = func(sessionID, role string) {
		s.metrics.VideoPeers.WithLabelValues(role).Dec()
	}
	s.knowledge = newKnowledgeCache(cfg.KnowledgeFor)
	s.routes()
	return s
}

// DefaultScorers 返回默认的评分器组合: 两个同源的规则评分器加一个仲裁器。
func DefaultScorers() Scorers {
	return Scorers{
		Primary:   scoring.NewKeywordScorer("rule-baseline-a", 0),
		Secondary: scoring.NewKeywordScorer("rule-baseline-b", 0),
		Arbiter:   scoring.NewKeywordScorer("rule-arbiter-c", 0),
		Tolerance: 1,
	}
}

// Handler 返回 HTTP 处理器(含限流、请求 ID、panic 兜底、访问日志)。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.withRecovery(h)
	h = s.withAccessLog(h)
	h = s.withRequestID(h)
	h = s.withRateLimit(h)
	return h
}

func (s *Server) routes() {
	// 运维端点: 不需要业务鉴权, 但也不该对外暴露(由部署层限制访问来源)。
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.Handle("GET /metrics", s.metrics.Handler())

	// 候选人接口: 用一次性会话令牌, 只能访问属于自己的那一场面试。
	s.mux.HandleFunc("GET /api/v1/candidate/session", s.handleCandidateSession)
	s.mux.HandleFunc("GET /ws/interview/{id}", s.handleInterview)
	s.mux.HandleFunc("POST /api/v1/candidate/events", s.handleCandidateEvents)

	// 管理接口: 用 API Key + 角色权限。
	s.mux.Handle("POST /api/v1/sessions", s.withPermission(auth.PermSessionCreate, s.handleCreateSession))
	s.mux.Handle("GET /api/v1/sessions", s.withPermission(auth.PermReportRead, s.handleListSessions))
	s.mux.Handle("GET /api/v1/sessions/{id}", s.withPermission(auth.PermReportRead, s.handleGetSession))
	s.mux.Handle("GET /api/v1/sessions/{id}/report", s.withPermission(auth.PermReportRead, s.handleGetReport))
	s.mux.Handle("GET /api/v1/sessions/{id}/consents", s.withPermission(auth.PermReportRead, s.handleGetConsents))
	s.mux.Handle("POST /api/v1/sessions/{id}/override", s.withPermission(auth.PermScoreOverride, s.handleOverrideScore))
	s.mux.Handle("POST /api/v1/resume/parse", s.withPermission(auth.PermSessionCreate, s.handleParseResume))

	s.mux.Handle("GET /api/v1/keys", s.withPermission(auth.PermKeyAdmin, s.handleListKeys))
	s.mux.Handle("POST /api/v1/keys", s.withPermission(auth.PermKeyAdmin, s.handleCreateKey))
	s.mux.Handle("DELETE /api/v1/keys/{id}", s.withPermission(auth.PermKeyAdmin, s.handleRevokeKey))
	s.mux.Handle("GET /api/v1/audit", s.withPermission(auth.PermAuditRead, s.handleListAudit))
	s.mux.Handle("GET /api/v1/analytics/overview", s.withPermission(auth.PermAnalyticsRead, s.handleAnalytics))
	s.mux.Handle("GET /api/v1/candidates/{ref}/export", s.withPermission(auth.PermDataErase, s.handleExportCandidate))
	s.mux.Handle("DELETE /api/v1/candidates/{ref}", s.withPermission(auth.PermDataErase, s.handleEraseCandidate))

	// 招聘业务域: 职位 / 候选人 / 投递管道 / 题库 / 面试安排。
	// 读走 recruit:read(所有角色), 写走各自权限点, 全部写操作进审计。
	s.mux.Handle("GET /api/v1/jobs", s.withPermission(auth.PermRecruitRead, s.handleListJobs))
	s.mux.Handle("POST /api/v1/jobs", s.withPermission(auth.PermJobWrite, s.handleCreateJob))
	s.mux.Handle("GET /api/v1/jobs/{id}", s.withPermission(auth.PermRecruitRead, s.handleGetJob))
	s.mux.Handle("PATCH /api/v1/jobs/{id}", s.withPermission(auth.PermJobWrite, s.handleUpdateJob))

	s.mux.Handle("GET /api/v1/candidates", s.withPermission(auth.PermRecruitRead, s.handleListCandidates))
	s.mux.Handle("POST /api/v1/candidates", s.withPermission(auth.PermCandidateWrite, s.handleUpsertCandidate))
	s.mux.Handle("GET /api/v1/candidates/{ref}", s.withPermission(auth.PermRecruitRead, s.handleGetCandidate))

	s.mux.Handle("GET /api/v1/applications", s.withPermission(auth.PermRecruitRead, s.handleListApplications))
	s.mux.Handle("POST /api/v1/applications", s.withPermission(auth.PermCandidateWrite, s.handleCreateApplication))
	s.mux.Handle("GET /api/v1/applications/{id}", s.withPermission(auth.PermRecruitRead, s.handleGetApplication))
	s.mux.Handle("PATCH /api/v1/applications/{id}", s.withPermission(auth.PermCandidateWrite, s.handlePatchApplication))

	s.mux.Handle("GET /api/v1/questions", s.withPermission(auth.PermRecruitRead, s.handleListQuestions))
	s.mux.Handle("POST /api/v1/questions", s.withPermission(auth.PermQuestionWrite, s.handleCreateQuestion))
	s.mux.Handle("PATCH /api/v1/questions/{id}", s.withPermission(auth.PermQuestionWrite, s.handleUpdateQuestion))
	s.mux.Handle("DELETE /api/v1/questions/{id}", s.withPermission(auth.PermQuestionWrite, s.handleDeleteQuestion))

	s.mux.Handle("GET /api/v1/knowledge/stats", s.withPermission(auth.PermRecruitRead, s.handleKnowledgeStats))
	s.mux.Handle("GET /api/v1/retrieval/search", s.withPermission(auth.PermRecruitRead, s.handleRetrievalSearch))

	s.mux.Handle("GET /api/v1/schedules", s.withPermission(auth.PermRecruitRead, s.handleListSchedules))
	s.mux.Handle("POST /api/v1/schedules", s.withPermission(auth.PermScheduleWrite, s.handleCreateSchedule))
	// 开始面试会创建真实会话并把链接发给候选人, 因此要求 session:create,
	// 而不只是 schedule:write —— 能排期不等于能对外发起面试。
	s.mux.Handle("POST /api/v1/schedules/{id}/start",
		s.withPermission(auth.PermSessionCreate, s.handleStartSchedule))
	s.mux.Handle("GET /api/v1/analytics/pipeline", s.withPermission(auth.PermAnalyticsRead, s.handlePipelineAnalytics))

	// 编程判题: 在隔离沙箱里执行候选人代码。
	s.mux.Handle("POST /api/v1/code/run", s.withPermission(auth.PermCodeRun, s.handleCodeRun))

	// 视频旁听: 用 API Key 换一张短期票据, 避免把密钥放进 WebSocket URL。
	s.mux.Handle("POST /api/v1/sessions/{id}/observer-ticket",
		s.withPermission(auth.PermInterviewObserve, s.handleObserverTicket))
	s.mux.Handle("GET /api/v1/sessions/{id}/recordings",
		s.withPermission(auth.PermRecordingRead, s.handleListRecordings))
	// AI 日志: 把逐轮问答、评分依据、降级、防作弊与审计串成时间线。
	s.mux.Handle("GET /api/v1/sessions/{id}/ai-log",
		s.withPermission(auth.PermReportRead, s.handleAILog))
	s.mux.Handle("GET /api/v1/sessions/{id}/recordings/{kind}",
		s.withPermission(auth.PermRecordingRead, s.handleDownloadRecording))
	s.mux.Handle("DELETE /api/v1/sessions/{id}/recordings/{kind}",
		s.withPermission(auth.PermDataErase, s.handleDeleteRecording))

	s.mux.Handle("GET /api/v1/system/providers", s.withPermission(auth.PermKeyAdmin, s.handleProviderDiagnostics))
	s.mux.Handle("GET /api/v1/system/ice", s.withPermission(auth.PermRecruitRead, s.handleICEServers))

	// 候选人侧: 用会话令牌访问属于自己的那一场面试。
	s.mux.HandleFunc("POST /api/v1/candidate/resume", s.handleCandidateResume)
	s.mux.HandleFunc("POST /api/v1/candidate/consent", s.handleCandidateConsent)
	s.mux.HandleFunc("GET /api/v1/candidate/report", s.handleCandidateReport)
	s.mux.HandleFunc("POST /api/v1/candidate/recording/chunks", s.handleCandidateRecordingChunk)
	s.mux.HandleFunc("POST /api/v1/candidate/recording/finalize", s.handleCandidateRecordingFinalize)
	s.mux.HandleFunc("POST /api/v1/candidate/code/run", s.handleCandidateCodeRun)

	// 账号体系: 注册、登录、人脸、个人中心。
	//
	// 这些接口**不走 withPermission**: 它们本身就是"取得身份"的动作,
	// 用 API Key 才能调用的话, 人就没法登录了。
	s.mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/face/login", s.handleFaceLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/auth/me", s.handleMe)
	s.mux.Handle("POST /api/v1/auth/password",
		s.withAccount(auth.PermSelfService, s.handleChangePassword))
	s.mux.Handle("POST /api/v1/auth/face/enroll",
		s.withAccount(auth.PermSelfService, s.handleFaceEnroll))
	s.mux.Handle("DELETE /api/v1/auth/face",
		s.withAccount(auth.PermSelfService, s.handleFaceDelete))
	// 人脸自检: 只返回相似度与阈值, 不签发会话 —— 用来诊断"录入了却登不上"。
	s.mux.Handle("POST /api/v1/auth/face/check",
		s.withAccount(auth.PermSelfService, s.handleFaceCheck))
	s.mux.Handle("GET /api/v1/auth/accounts",
		s.withPermission(auth.PermKeyAdmin, s.handleAccounts))
	// 候选人的历史面试记录: 只能看到与自己引用值匹配的那些会话。
	s.mux.Handle("GET /api/v1/candidate/history",
		s.withAccount(auth.PermSelfService, s.handleCandidateHistory))

	// 真人双向对练: 配对 + 房间实时通道。
	s.mux.Handle("POST /api/v1/practice/join",
		s.withAccount(auth.PermSelfService, s.handlePracticeJoin))
	s.mux.Handle("POST /api/v1/practice/leave",
		s.withAccount(auth.PermSelfService, s.handlePracticeLeave))
	s.mux.Handle("GET /api/v1/practice/room/{id}",
		s.withAccount(auth.PermSelfService, s.handlePracticeRoom))
	s.mux.Handle("GET /api/v1/practice/rooms",
		s.withAccount(auth.PermSelfService, s.handlePracticeRooms))
	// WebSocket 自己解析登录会话(浏览器握手会自动带同源 Cookie)。
	s.mux.HandleFunc("GET /ws/practice/{id}", s.handlePracticeSocket)

	// 静态资源挂在根路径。Go 1.22 的 ServeMux 优先匹配更具体的模式,
	// 所以 /api 与 /ws 不会被这里吞掉。
	// 走 StaticHandler 而不是裸 FileServer: 内嵌前端必须带 ETag 与
	// 强制重校验, 否则浏览器会拿旧 HTML 去请求已经不存在的旧资源名。
	s.mux.Handle("/", StaticHandler())
}

// storeCtx 给单次存储操作套上超时。
//
// 数据库抖动(慢查询、锁等待)时, 没有超时的调用会一直挂到客户端断开,
// 在高并发下直接把连接池耗尽 —— 一个租户的慢查询会拖垮所有租户。
func (s *Server) storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.StoreTimeout)
}

func sameHost(origin, host string) bool {
	if origin == "" {
		return true
	}
	trimmed := origin
	for _, prefix := range []string{"https://", "http://"} {
		if len(trimmed) > len(prefix) && trimmed[:len(prefix)] == prefix {
			trimmed = trimmed[len(prefix):]
			break
		}
	}
	return trimmed == host
}
