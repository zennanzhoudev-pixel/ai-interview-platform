// Package observability 提供指标与链路追踪。
//
// 面试系统对可观测的要求比普通 CRUD 服务高得多, 原因有三个:
//
//  1. **延迟是体验本身**: 首字响应从 1.2s 劣化到 2.5s, 候选人会觉得"对面在走神",
//     而这必须能按链路段归因, 不能只看一个端到端数字;
//  2. **质量会漂移**: 评分分布、双模型分歧率、降级比例都需要持续监控,
//     否则"模型悄悄变了"这件事永远没人发现;
//  3. **成本按场次发生**: 一场面试的 token 消耗要能拆到租户与模型,
//     不然账单来了只能干瞪眼。
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics 聚合全部指标。集中定义而不是各处散落 prometheus.NewCounter:
// 指标名与标签一旦散落, 迟早会同时存在 interview_x 和 interview_xx 两个近义指标。
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests  *prometheus.CounterVec
	HTTPDuration  *prometheus.HistogramVec
	WSConnections prometheus.Gauge
	WSMessages    *prometheus.CounterVec
	Panics        *prometheus.CounterVec
	StoreErrors   *prometheus.CounterVec
	AuthFailures  *prometheus.CounterVec

	SessionsStarted   *prometheus.CounterVec
	SessionsFinished  *prometheus.CounterVec
	SessionDuration   *prometheus.HistogramVec
	Turns             *prometheus.CounterVec
	FirstResponse     *prometheus.HistogramVec
	ScoreDistribution *prometheus.CounterVec
	DegradedScores    *prometheus.CounterVec
	HumanReviews      prometheus.Counter

	Retrievals  *prometheus.CounterVec
	RetrieveMS  *prometheus.HistogramVec
	SandboxRuns *prometheus.CounterVec
	SandboxMS   *prometheus.HistogramVec
	Recordings  *prometheus.CounterVec
	RecordingMB *prometheus.CounterVec
	VideoPeers  *prometheus.GaugeVec
	WebRTCSig   *prometheus.CounterVec

	LLMTokens *prometheus.CounterVec
	LLMCalls  *prometheus.CounterVec
}

// NewMetrics 构造指标集并注册默认 Go 运行时采集器。
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_http_requests_total",
		Help: "HTTP 请求总数",
	}, []string{"route", "method", "code"})

	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "interview_http_request_duration_seconds",
		Help:    "HTTP 请求耗时",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"route"})

	m.WSConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "interview_ws_connections",
		Help: "当前在线面试连接数",
	})

	m.WSMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_ws_messages_total",
		Help: "WebSocket 消息数(按类型)",
	}, []string{"type"})

	m.Panics = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_goroutine_panics_total",
		Help: "被兜底捕获的 goroutine panic 次数",
	}, []string{"goroutine"})

	m.StoreErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_store_errors_total",
		Help: "存储操作错误数",
	}, []string{"op"})

	m.AuthFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_auth_failures_total",
		Help: "认证失败次数",
	}, []string{"reason"})

	m.SessionsStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_sessions_started_total",
		Help: "发起的面试场次",
	}, []string{"tenant", "round"})

	m.SessionsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_sessions_finished_total",
		Help: "结束的面试场次",
	}, []string{"tenant", "recommendation"})

	m.SessionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "interview_session_duration_seconds",
		Help:    "面试实际用时",
		Buckets: []float64{60, 300, 600, 900, 1200, 1800, 2700, 3600},
	}, []string{"tenant"})

	m.Turns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_turns_total",
		Help: "问答轮次总数",
	}, []string{"tenant", "stage", "scored"})

	// 首字延迟是语音面试的生命线, 单独用细粒度分桶。
	m.FirstResponse = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "interview_first_response_seconds",
		Help:    "从候选人说完到 AI 开始响应的耗时(首字延迟)",
		Buckets: []float64{.2, .4, .6, .8, 1, 1.2, 1.5, 2, 3, 5},
	}, []string{"tenant"})

	m.ScoreDistribution = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_scores_total",
		Help: "能力项评分分布(按等级)",
	}, []string{"tenant", "competency", "level"})

	m.DegradedScores = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_degraded_scores_total",
		Help: "由备用评分器产出的评分条数",
	}, []string{"tenant"})

	m.HumanReviews = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "interview_human_overrides_total",
		Help: "人工改分次数",
	})

	m.Retrievals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_rag_retrievals_total",
		Help: "参考题库检索次数",
	}, []string{"result"})

	m.RetrieveMS = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "interview_rag_retrieval_seconds",
		Help:    "混合检索耗时(BM25 + 向量 + RRF + 精排)",
		Buckets: []float64{.002, .005, .01, .02, .05, .1, .25, .5, 1},
	}, []string{"result"})

	m.SandboxRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_sandbox_runs_total",
		Help: "编程沙箱执行次数",
	}, []string{"language", "status", "isolated"})

	m.SandboxMS = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "interview_sandbox_duration_seconds",
		Help:    "编程沙箱执行耗时",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
	}, []string{"language"})

	m.Recordings = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_recordings_total",
		Help: "录制的面试音视频分片数",
	}, []string{"kind", "result"})

	m.RecordingMB = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_recording_bytes_total",
		Help: "面试录制写入字节数",
	}, []string{"kind"})

	m.VideoPeers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interview_video_peers",
		Help: "面试间视频链路在线端数(候选人 + 人类面试官)",
	}, []string{"role"})

	m.WebRTCSig = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_webrtc_signaling_total",
		Help: "WebRTC 信令消息数",
	}, []string{"kind"})

	m.LLMTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_llm_tokens_total",
		Help: "大模型 token 消耗",
	}, []string{"tenant", "model", "kind"})

	m.LLMCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "interview_llm_calls_total",
		Help: "大模型调用次数",
	}, []string{"tenant", "model", "result"})

	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.WSConnections, m.WSMessages, m.Panics,
		m.StoreErrors, m.AuthFailures, m.SessionsStarted, m.SessionsFinished,
		m.SessionDuration, m.Turns, m.FirstResponse, m.ScoreDistribution,
		m.DegradedScores, m.HumanReviews, m.Retrievals, m.RetrieveMS,
		m.SandboxRuns, m.SandboxMS, m.Recordings, m.RecordingMB,
		m.VideoPeers, m.WebRTCSig, m.LLMTokens, m.LLMCalls,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	return m
}

// Prime 预创建某个租户的固定标签序列。
//
// 直方图/Counter 在第一次打点前不会出现在 /metrics 里 —— 对 Prometheus
// 这是正确行为, 对值班的人却是个坑: 面试刚开始时"首字延迟"面板是空的,
// 看上去像指标没接上, 于是有人去改已经正确的代码。会话创建时就把序列
// 建好, 面板从 0 开始, 语义也更诚实: "暂无数据"和"数据是 0"不是一回事。
func (m *Metrics) Prime(tenant string) {
	if tenant == "" {
		return
	}
	m.FirstResponse.WithLabelValues(tenant)
	m.SessionDuration.WithLabelValues(tenant)
	m.DegradedScores.WithLabelValues(tenant)
	m.SessionsStarted.WithLabelValues(tenant, "1")
	m.RetrieveMS.WithLabelValues("hit")
	m.RetrieveMS.WithLabelValues("miss")
	m.Retrievals.WithLabelValues("hit")
	m.Retrievals.WithLabelValues("miss")
}

// Handler 返回 /metrics 端点处理器。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
