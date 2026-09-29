// Command interviewd 既是面试服务的入口, 也是编排引擎的离线模拟器。
//
// 三种运行模式:
//   - 离线模拟(默认): 用脚本化回答跑完一场文本面试, 不依赖 ASR/LLM/数据库,
//     可以直接放进 CI 做回归;
//   - Web 服务(-serve): 起 HTTP/WebSocket 服务, 提供面试、报告、看板等接口;
//   - 联调自检(-selftest): 开面之前先 ping 一遍上游, 而不是让第一场面试替你冒烟。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/account"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/api"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/knowledge"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/observability"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/recording"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/sandbox"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

func main() {
	round := flag.Int("round", 1, "面试轮次 (1..5)")
	minutes := flag.Int("minutes", 45, "本轮面试时长预算 (分钟)")
	maxTurns := flag.Int("max-turns", 60, "单场最大问答轮数 (安全上限)")
	out := flag.String("out", "", "报告 JSON 输出路径, 留空则只打印成绩单")
	strict := flag.Bool("strict", false, "启用更严格的复核模型, 演示双模型分歧链路")
	serve := flag.String("serve", "", "启动 Web 服务, 例如 :8080; 留空则只跑离线模拟")
	mysqlDSN := flag.String("mysql-dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN; 留空使用内存存储")
	redisAddr := flag.String("redis-addr", os.Getenv("REDIS_ADDR"), "Redis 地址; 留空则不启用会话快照")
	selftest := flag.Bool("selftest", false, "联调自检: 探测已配置的 LLM/TTS/Embedding 是否可用")
	requireAuth := flag.Bool("require-auth", os.Getenv("MYSQL_DSN") != "",
		"强制 API Key 鉴权(默认: 配置了 MySQL 时开启; 内存演示模式下关闭)")
	appSecret := flag.String("app-secret", os.Getenv("APP_SECRET"),
		"会话令牌签名密钥; 开启鉴权时必填")
	tenantID := flag.String("tenant", defaultEnv("TENANT_ID", "default"), "默认租户 ID")
	otlpEndpoint := flag.String("otlp-endpoint", os.Getenv("OTLP_ENDPOINT"),
		"OTLP/HTTP 追踪接收地址(如 127.0.0.1:4318); 留空则关闭追踪导出")
	recordingDir := flag.String("recording-dir", defaultEnv("RECORDING_DIR", "data/recordings"),
		"面试录制件落盘目录; 设为 off 关闭录制功能")
	sandboxEngine := flag.String("sandbox-engine", defaultEnv("SANDBOX_ENGINE", "auto"),
		"判题沙箱引擎: auto/docker/local; local 不具备隔离能力, 仅限本地开发")
	iceServers := flag.String("ice-servers", os.Getenv("ICE_SERVERS"),
		"WebRTC ICE 服务器, 逗号分隔(如 stun:stun.example.com:3478,turn:turn.example.com:3478)")
	retentionDays := flag.Int("retention-days", envInt("RECORDING_RETENTION_DAYS", 90),
		"面试录制件保留天数, 到期后由清理任务删除")
	adminInvite := flag.String("admin-invite-code", os.Getenv("ADMIN_INVITE_CODE"),
		"企业成员(管理员/面试官)注册邀请码; 留空表示只能由管理员在后台创建账号")
	faceLogin := flag.Bool("face-login", defaultEnv("FACE_LOGIN", "on") != "off",
		"是否允许人脸登录(需要先在个人中心录入)")
	healthcheck := flag.String("healthcheck", "",
		"容器健康检查模式: 传入一个 URL, 探测成功则退出码 0")
	logLevel := flag.String("log-level", defaultEnv("LOG_LEVEL", "info"), "日志级别 debug/info/warn/error")
	logFormat := flag.String("log-format", defaultEnv("LOG_FORMAT", "text"), "日志格式 text/json")
	flag.Parse()

	cliLogger := log.New(os.Stderr, "[interviewd] ", log.LstdFlags)

	// distroless 镜像里没有 shell、没有 curl, 因此健康检查必须由二进制
	// 自己完成 —— 否则要么放弃健康检查, 要么为了让探针能跑而塞进一个
	// 完整的操作系统镜像, 那等于把攻击面又还回去。
	if *healthcheck != "" {
		runHealthcheck(*healthcheck)
		return
	}

	if *selftest {
		runSelfTest(cliLogger)
		return
	}

	if *serve != "" {
		if err := runServer(serverOptions{
			addr: *serve, mysqlDSN: *mysqlDSN, redisAddr: *redisAddr,
			requireAuth: *requireAuth, appSecret: *appSecret, tenantID: *tenantID,
			otlpEndpoint: *otlpEndpoint, logLevel: *logLevel, logFormat: *logFormat,
			recordingDir: *recordingDir, sandboxEngine: *sandboxEngine,
			iceServers: *iceServers, retentionDays: *retentionDays,
			adminInvite: *adminInvite, faceLogin: *faceLogin,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "服务启动失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	runSimulation(*round, *minutes, *maxTurns, *out, *strict)
}

/* ---------------- 离线模拟 ---------------- */

func runSimulation(round, minutes, maxTurns int, out string, strict bool) {
	total := time.Duration(minutes) * time.Minute
	plan := orchestrator.DefaultPlan(total)
	plan.Round = round
	bank := orchestrator.DefaultBank()

	// 复核模型的严格度。默认与主模型一致(演示正常链路);
	// -strict 让复核模型整体保守一级, 用来观察"双模型分歧"如何进入报告。
	// 分歧超过容忍度时的三方仲裁由 internal/scoring 的单元测试覆盖。
	reviewBias := 0
	if strict {
		reviewBias = -1
	}

	engine := orchestrator.NewEngine(plan, bank, total,
		orchestrator.WithScorers(
			scoring.NewKeywordScorer("interviewer-model-a", 0),
			scoring.NewKeywordScorer("reviewer-model-b", reviewBias),
			scoring.NewKeywordScorer("arbiter-model-c", 0),
			1,
		),
	)

	fmt.Println("AI 线上面试中台 · 编排引擎模拟")
	fmt.Printf("第 %d 面 | 时长预算 %d 分钟 | 题库 %d 题 | 阶段 %d 个\n",
		round, minutes, bank.Size(), len(plan.Specs))
	fmt.Println(strings.Repeat("-", 72))

	prevStage := orchestrator.Stage("")
	decision := engine.Start()
	for i := 0; i < maxTurns; i++ {
		if decision.Action == orchestrator.ActionFinish {
			break
		}
		if decision.Stage != prevStage {
			fmt.Printf("\n[阶段] %s\n", decision.Stage)
			prevStage = decision.Stage
		}
		label := "面试官"
		if decision.IsProbe {
			label = "追问  "
		}
		fmt.Printf("  %s> %s\n", label, decision.Question)

		answer, took := scriptedAnswer(decision.QuestionID)
		fmt.Printf("  候选人> %s\n", answer)
		fmt.Printf("           (用时 %s)\n", took)

		next, err := engine.Submit(answer, took)
		if err != nil {
			fmt.Fprintf(os.Stderr, "提交回答失败: %v\n", err)
			os.Exit(1)
		}
		decision = next
	}

	report := engine.Report()
	printScorecard(report)
	if out != "" {
		if err := writeReport(out, report); err != nil {
			fmt.Fprintf(os.Stderr, "写出报告失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n报告已写入 %s\n", out)
	}
}

func printScorecard(rep orchestrator.Report) {
	fmt.Println(strings.Repeat("-", 72))
	fmt.Println("能力维度")
	if len(rep.Dimensions) == 0 {
		fmt.Println("  (本场没有产生任何可评分的考察项)")
	}
	for _, d := range rep.Dimensions {
		fmt.Printf("  %-18s %-8s 置信度 %.2f | 命中 %d 轮\n",
			d.Competency, d.Level, d.Confidence, d.Turns)
		for _, e := range d.Evidence {
			mark := "+"
			if string(e.Kind) == "against" {
				mark = "-"
			}
			fmt.Printf("      %s [%s] %s\n", mark, e.TurnID, truncate(e.Quote, 44))
		}
		if len(d.Concerns) > 0 {
			fmt.Printf("      待确认要点: %s\n", strings.Join(d.Concerns, " / "))
		}
	}
	if len(rep.Gaps) > 0 {
		fmt.Printf("\n  未覆盖能力项: %s\n", strings.Join(rep.Gaps, " / "))
	}

	fmt.Println(strings.Repeat("-", 72))
	fmt.Println("质量与过程指标")
	fmt.Printf("  问答 %d 轮 | 计分 %d 轮 | 追问 %d 轮 | 最大追问深度 %d\n",
		rep.Stats.Turns, rep.Stats.ScoredTurns, rep.Stats.Probes, rep.Stats.MaxProbeDepth)
	fmt.Printf("  双模型分歧 %d 条 | 三方仲裁 %d 条 | 转人工复核 %d 条 | 降级 %d 条\n",
		rep.Stats.Disagreements, rep.Stats.Arbitrations, rep.Stats.HumanReviewItems, rep.Stats.Degraded)
	fmt.Printf("  平均置信度 %.2f | 耗时 %ds / 预算 %ds\n",
		rep.Stats.AvgConfidence, rep.DurationSec, rep.BudgetSec)

	fmt.Println(strings.Repeat("-", 72))
	fmt.Printf("结论: %s (综合分 %d, 置信度 %.2f)\n",
		rep.Recommendation, rep.Score, rep.Confidence)
	for _, f := range rep.Flags {
		fmt.Printf("  风险提示: %s\n", f)
	}
	fmt.Println("说明: 结论只是给面试官的参考意见, 真实系统里录用决策始终由人做。")
}

func writeReport(path string, rep orchestrator.Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// canned 是一条脚本化的回答。
type canned struct {
	text string
	took time.Duration
}

var rootAnswers = map[string]canned{
	"q_greet": {
		text: "面试官好, 我是主攻 Go 后端方向的候选人。最近一年在做 IM 对话平台, 主要负责 Redis 存储改造和分布式任务调度两块, 另外自己做过一个 AI-Ops 运维 Agent 平台。",
		took: 70 * time.Second,
	},
	"q_resume_zset": {
		text: "我们把小游戏排行榜从一个大 Hash 拆成了 ZSet 索引, 用 score 存权重, 排名查询直接走有序结构, 不用再全量排序, 查询 RT 降了 70% 左右。",
		took: 150 * time.Second,
	},
	"q_resume_wasm": {
		text: "我们选了 Extism 的 WASM 插件运行时, 主要是它能把编译产物复用起来, 避免每次首次对话都重新编译, 冷启动从 5 到 8 秒压到 1 秒内。另外 WASM 本身有沙箱隔离能力, 比直接跑原生插件安全。",
		took: 160 * time.Second,
	},
	"q_resume_flow": {
		text: "这条流水线我们做成了状态机, 每个阶段有独立状态, 失败只重试当前阶段, 不用整条重跑。写库的时候用 shot_id 做幂等键, 保证重试不会重复扣费; 每条任务都落了断点, 可以从失败阶段继续。极端情况下靠定时补偿任务兜底。",
		took: 150 * time.Second,
	},
	"q_go_gmp": {
		text: "GMP 里 G 是 goroutine, M 是系统线程, P 是处理器上下文。P 带着一个本地队列, 减少了全局锁竞争; 某个 P 的队列空了会去别的 P 那里 work stealing; Go 1.14 之后还加了基于信号的抢占。相比线程模型, 切换发生在用户态, 不用陷入内核。",
		took: 180 * time.Second,
	},
	"q_go_gc": {
		text: "Go 用的是三色标记加混合写屏障, 标记阶段和用户程序并发执行, 所以标记过程本身不需要 STW。STW 主要出现在开启写屏障和标记终止这两个很短的阶段。如果对象分配速率太高, 辅助标记跟不上, 或者栈扫描任务太重, STW 就会变长。",
		took: 150 * time.Second,
	},
	"q_dist_idem": {
		text: "消费端幂等我会用唯一键做去重, 把业务 ID 加消息 ID 拼成唯一索引, 写库时靠唯一约束兜底。跨服务场景下单靠本地去重不够, 通常引入一张状态机表记录消费状态, 让下游操作满足最终一致, 失败的再靠定时补偿。",
		took: 140 * time.Second,
	},
	"q_arch_limit": {
		text: "我会在入口做一层限流, 对热点接口单独配置阈值, 同时准备降级开关, 一旦限流组件本身故障就整体放行, 保证不拖垮主站。",
		took: 120 * time.Second,
	},
	"q_candidate_qa": {
		text: "想了解下团队现在的技术栈, 以及新人上手一般是什么节奏。",
		took: 45 * time.Second,
	},
	"q_wrap": {
		text: "好的, 谢谢面试官, 那我等结果。",
		took: 15 * time.Second,
	},
}

// probeAnswers 给出"第 n 层追问"的回答, 用来演示两条收敛路径:
//
//	q_resume_zset: 追问一层就补上了缺失要点 -> 立即收敛, 不浪费面试时间;
//	其它问题     : 连着两层都答不上来 -> 撞到最大追问深度上限, 换题。
var probeAnswers = map[string][]canned{
	"q_resume_zset": {
		{
			text: "内存这块我们做过估算, 单个 ZSet 大概几万个成员, 另外加了定期过期清理和按用户分片, 避免出现大 key。",
			took: 75 * time.Second,
		},
	},
}

func scriptedAnswer(questionID string) (string, time.Duration) {
	if a, ok := rootAnswers[questionID]; ok {
		return a.text, a.took
	}
	root, depth := splitProbeID(questionID)
	if depth > 0 {
		if list, ok := probeAnswers[root]; ok && depth-1 < len(list) {
			return list[depth-1].text, list[depth-1].took
		}
		return "这个……当时没深究, 主要是先保证功能可用, 细节确实记不太清了。", 40 * time.Second
	}
	return "这块我了解一些, 具体实现是照着文档做的。", 60 * time.Second
}

func splitProbeID(id string) (string, int) {
	i := strings.LastIndex(id, ".p")
	if i < 0 {
		return id, 0
	}
	depth, err := strconv.Atoi(id[i+2:])
	if err != nil {
		return id, 0
	}
	return id[:i], depth
}

func truncate(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "..."
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt 读取整数环境变量, 非法或缺失时用默认值。
func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// runHealthcheck 探测一个 URL, 用于容器 HEALTHCHECK。
//
// 只接受 http/https, 并且超时固定 3 秒: 健康检查本身不能变成一个新的
// 故障点(例如 DNS 卡住导致探针永远挂着)。
func runHealthcheck(rawURL string) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		fmt.Fprintln(os.Stderr, "healthcheck: 只支持 http/https URL")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "healthcheck: 状态码 %d\n", resp.StatusCode)
		os.Exit(1)
	}
}

/* ---------------- Web 服务 ---------------- */

type serverOptions struct {
	addr          string
	mysqlDSN      string
	redisAddr     string
	requireAuth   bool
	appSecret     string
	tenantID      string
	otlpEndpoint  string
	logLevel      string
	logFormat     string
	recordingDir  string
	sandboxEngine string
	iceServers    string
	retentionDays int
	adminInvite   string
	faceLogin     bool
}

// runServer 启动 Web 服务。
//
// 存储与评分器都是"可选增强": 没有 MySQL 就用内存, 没有 Redis 就跳过快照,
// 没有大模型就用规则评分。这样在一台干净机器上 `-serve :8080` 就能跑起完整流程,
// 而不是先让人去配一堆中间件 —— 后者通常会变成"项目看起来不错, 但从没跑起来过"。
func runServer(opts serverOptions) error {
	cliLogger := log.New(os.Stderr, "[interviewd] ", log.LstdFlags)
	logger := platform.NewLogger(platform.LogConfig{Level: opts.logLevel, Format: opts.logFormat})
	slog.SetDefault(logger)

	// 语音链路的 goroutine 也要有 panic 兜底; 把它接到日志与指标上。
	media.SetPanicLogger(logger)

	metrics := observability.NewMetrics()
	media.SetPanicHook(func(string) { metrics.Panics.WithLabelValues("media.goroutine").Inc() })

	// 链路追踪: 未配置端点时退化为 no-op, 调用方代码零改动。
	shutdownTracing, _, err := observability.InitTracing(context.Background(), observability.TraceConfig{
		ServiceName: "interviewd",
		Endpoint:    opts.otlpEndpoint,
		Insecure:    true,
		SampleRatio: 1,
	})
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()
	if opts.otlpEndpoint == "" {
		logger.Info("未配置 OTLP 端点, 链路追踪已关闭")
	} else {
		logger.Info("链路追踪已启用", slog.String("endpoint", opts.otlpEndpoint))
	}

	sessionStore, closeStore, err := openStore(opts.mysqlDSN, cliLogger)
	if err != nil {
		return err
	}
	defer closeStore()

	ckpt, err := openCheckpoint(opts.redisAddr, cliLogger)
	if err != nil {
		return err
	}

	// 签名密钥: 开启鉴权时必须有稳定密钥, 否则重启后所有候选人令牌失效、
	// 候选人假名引用全部改变。这种情况宁可启动失败, 也不能默默用临时密钥跑。
	secret := []byte(opts.appSecret)
	if opts.requireAuth && len(secret) == 0 {
		return errors.New("开启鉴权时必须通过 -app-secret 或 APP_SECRET 指定签名密钥")
	}
	if len(secret) == 0 {
		secret = randomSecret()
		logger.Warn("演示模式: 使用临时签名密钥, 重启后已发出的候选人链接会失效")
	}

	keys, err := openKeyStore(sessionStore, opts.requireAuth, opts.tenantID, cliLogger)
	if err != nil {
		return err
	}

	// 账号体系: 人用的登录入口。
	//
	// 账号库优先复用 MySQL 连接(有 MySQL 时账号才会在重启后保留);
	// 没有 MySQL 时退化为内存实现, 并明确告警 —— "注册完重启就登不上"
	// 是很容易被忽略、但用户一定会遇到的问题。
	accounts := openAccountService(sessionStore, secret, opts, logger)
	if !opts.requireAuth {
		logger.Warn("鉴权已关闭(演示模式): 任何调用方都能读取本租户的数据。" +
			"生产环境请配置 MYSQL_DSN 并设置 APP_SECRET, 或显式开启 -require-auth")
	}

	scorerFactory, err := buildScorerFactory(cliLogger)
	if err != nil {
		return err
	}
	// 知识库: 提问、追问、检索可视化三者共用同一份索引。
	// 题库改动能立刻生效(rebuild 在请求内同步完成)。
	corpus, embedder, err := buildKnowledge(context.Background(), sessionStore, opts.tenantID, cliLogger)
	if err != nil {
		return err
	}
	asr, tts := buildMediaProviders(cliLogger)

	// 判题沙箱: 有容器运行时就用容器, 否则降级到本机并显式告警。
	runner := sandbox.AutoRunner(context.Background(), opts.sandboxEngine, func(msg string) {
		logger.Warn(msg)
	})
	if runner.Isolated() {
		logger.Info("判题沙箱已启用", slog.String("engine", runner.Name()), slog.Bool("isolated", true))
	} else {
		logger.Warn("判题沙箱运行在非隔离模式, 只能用于本地开发",
			slog.String("engine", runner.Name()))
	}

	// 录制存储: 简历原文与面试录像是最敏感的两类数据, 都有保留期。
	var blobs recording.BlobStore
	if strings.EqualFold(strings.TrimSpace(opts.recordingDir), "off") {
		logger.Warn("面试录制已关闭(-recording-dir=off): 候选人界面上不会出现录制开关")
	} else {
		fs, err := recording.NewFSStore(opts.recordingDir)
		if err != nil {
			return err
		}
		blobs = fs
		logger.Info("面试录制已启用",
			slog.String("dir", fs.Root()), slog.Int("retention_days", opts.retentionDays))
	}

	srv := api.NewServer(api.Config{
		Store:        sessionStore,
		Checkpoint:   ckpt,
		Keys:         keys,
		Secret:       secret,
		Scorers:      scorerFactory,
		ProbePlanner: corpus.Planner(),
		ASR:          asr,
		TTS:          tts,
		KnowledgeFor: func(ctx context.Context, tenant string) (*knowledge.Corpus, error) {
			next, _, err := buildKnowledge(ctx, sessionStore, tenant, cliLogger)
			return next, err
		},
		Sandbox:            runner,
		Blobs:              blobs,
		RecordingRetention: time.Duration(opts.retentionDays) * 24 * time.Hour,
		ICEServers:         parseICEServers(opts.iceServers),
		Pingers:            buildPingers(tts, asr, embedder, llm.FromEnv()),
		StoreKind:          storeKind(sessionStore),
		Accounts:           accounts,
		Logger:             logger,
		Metrics:            metrics,
		TenantID:           opts.tenantID,
		RequireAuth:        opts.requireAuth,
	})

	httpSrv := &http.Server{
		Addr:              opts.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 写超时留空: 面试是长连接场景, 设了写超时会把长面试从服务端切断。
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("收到退出信号, 正在优雅关闭(等待在途面试结束)")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// 长连接会让 Shutdown 一直等到面试结束, 因此必须有上限。
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("优雅关闭超时, 强制退出", slog.Any("err", err))
		}
	}()

	logger.Info("面试服务已启动",
		slog.String("url", "http://localhost"+opts.addr),
		slog.String("tenant", opts.tenantID),
		slog.Bool("require_auth", opts.requireAuth))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("已停止")
	return nil
}

// openKeyStore 构造 API Key 库。首次启用鉴权且没有任何密钥时,
// 引导一个 admin 密钥并打印一次 —— 否则没人能调用管理接口。
func openKeyStore(sessionStore store.SessionStore, requireAuth bool, tenantID string, logger *log.Logger) (auth.KeyStore, error) {
	var ks auth.KeyStore
	if my, ok := sessionStore.(*store.MySQLStore); ok {
		myKS := auth.NewMySQLKeyStore(my.DB())
		count, err := myKS.CountForTenant(tenantID)
		if err != nil {
			return nil, err
		}
		if requireAuth && count == 0 {
			raw, _, err := myKS.Add(tenantID, "引导密钥(请尽快吊销并换成正式密钥)", auth.RoleAdmin)
			if err != nil {
				return nil, err
			}
			logger.Printf("已生成引导用 API Key(仅显示这一次): %s", raw)
		} else if count > 0 {
			logger.Printf("已加载 %d 个 API Key", count)
		}
		ks = myKS
	} else {
		mem := auth.NewMemoryKeyStore()
		if requireAuth {
			raw, _, err := mem.Add(tenantID, "引导密钥", auth.RoleAdmin)
			if err != nil {
				return nil, err
			}
			logger.Printf("内存密钥库已生成引导用 API Key(仅显示这一次): %s", raw)
		}
		ks = mem
	}
	return ks, nil
}

// openAccountService 组装账号服务。
//
// 三条规则:
//  1. 有 MySQL 就用它 —— 账号必须跨重启存活;
//  2. 企业成员注册必须有邀请码; 演示模式下若没配, 就生成一个并打印一次,
//     否则本地根本没人能进工作台;
//  3. 候选人注册始终开放(他们通常是通过面试链接过来的), 不需要邀请码。
func openAccountService(
	sessionStore store.SessionStore,
	secret []byte,
	opts serverOptions,
	logger *slog.Logger,
) *account.Service {
	var accStore account.Store
	if my, ok := sessionStore.(*store.MySQLStore); ok {
		accStore = account.NewMySQLStore(my.DB())
		logger.Info("账号存储: MySQL(重启后保留)")
	} else {
		accStore = account.NewMemoryStore()
		logger.Warn("账号存储: 内存(重启后账号会丢失, 仅适合本地演示); 配置 MYSQL_DSN 可保留")
	}

	invite := strings.TrimSpace(opts.adminInvite)
	if invite == "" && !opts.requireAuth {
		// 演示模式: 没有邀请码就没法注册管理员, 那本地连工作台都进不去。
		// 生成一个并打印, 与"引导用 API Key"的处理方式一致。
		token, err := account.RandomToken(4)
		if err == nil {
			invite = "HR-" + strings.ToUpper(token)
			fmt.Fprintf(os.Stderr,
				"\n[interviewd] 演示模式: 企业成员注册邀请码(仅本次进程有效): %s\n"+
					"[interviewd] 用它注册管理员账号即可进入招聘工作台; 生产请设置 ADMIN_INVITE_CODE\n\n",
				invite)
		}
	}

	matcher := account.Matcher(account.NewLocalMatcher())
	if !opts.faceLogin {
		logger.Info("人脸登录已关闭(-face-login=off): 仍可用密码登录")
	}
	svc := account.New(account.Config{
		Store:           accStore,
		TenantID:        opts.tenantID,
		AdminInviteCode: invite,
		Secret:          secret,
		Matcher:         matcher,
		Logger:          logger,
	})
	info := svc.MatcherInfo()
	logger.Info("人脸匹配器已就绪",
		slog.String("matcher", fmt.Sprint(info["name"])),
		slog.String("assurance", fmt.Sprint(info["assurance"])),
		slog.Float64("threshold", toFloat(info["threshold"])))
	if fmt.Sprint(info["assurance"]) != "certified" {
		logger.Warn("当前人脸能力是开发级(图像相似度), 不是认证级人脸识别; " +
			"生产请接入云厂商人脸 API 或本地 SDK(实现 account.Matcher 即可)")
	}
	return svc
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}

func randomSecret() []byte {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return []byte(hex.EncodeToString(buf))
}

func openStore(mysqlDSN string, logger *log.Logger) (store.SessionStore, func(), error) {
	if mysqlDSN == "" {
		logger.Print("未配置 MYSQL_DSN, 使用内存存储(重启后数据丢失, 仅适合本地演示)")
		return store.NewMemoryStore(), func() {}, nil
	}
	my, err := store.OpenMySQL(mysqlDSN)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := my.Migrate(ctx); err != nil {
		_ = my.Close()
		return nil, nil, err
	}
	logger.Print("已连接 MySQL, 表结构已就绪")
	return my, func() { _ = my.Close() }, nil
}

func openCheckpoint(redisAddr string, logger *log.Logger) (store.CheckpointStore, error) {
	if redisAddr == "" {
		// 快照只是热路径缓存: 没有它, 断线重连仍然能靠重放问答记录完成,
		// 只是恢复时多读一次数据库。功能不受影响, 所以这里不当作失败。
		logger.Print("未配置 REDIS_ADDR, 会话快照已禁用(断线重连仍然可用)")
		return nil, nil
	}
	client := store.NewRedisClient(redisAddr, os.Getenv("REDIS_PASSWORD"), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Printf("Redis 不可用(%v), 已退化为不使用会话快照", err)
		return nil, nil
	}
	logger.Printf("会话快照已启用: %s", redisAddr)
	return store.NewRedisCheckpointStore(client, "interview:checkpoint:"), nil
}

// buildScorerFactory 组装评分器组合。
//
// 三种情形, 从便宜到贵:
//  1. 没配大模型: 两个规则评分器(离线可跑, 也是降级底线);
//  2. 配了一个模型: 主评分 = 大模型(失败自动降级到规则), 复核 = 规则;
//  3. 配了两个模型: 主与复核都是大模型, 分歧超过容忍度时由仲裁器取中位数。
func buildScorerFactory(logger *log.Logger) (func() api.Scorers, error) {
	cfg := llm.FromEnv()
	if !cfg.Enabled() {
		logger.Print("未配置 LLM_API_KEY, 使用规则评分器(报告里会标注评分来源)")
		return api.DefaultScorers, nil
	}

	primary := scoring.NewChainScorer(
		scoring.NewLLMScorer(llm.NewClient(cfg)),
		scoring.NewKeywordScorer("rule-fallback", 0),
	)

	secondary := scoring.NewChainScorer(scoring.NewKeywordScorer("rule-reviewer", 0))
	if modelB := os.Getenv("LLM_MODEL_B"); modelB != "" {
		cfgB := cfg
		cfgB.Model = modelB
		secondary = scoring.NewChainScorer(
			scoring.NewLLMScorer(llm.NewClient(cfgB)),
			scoring.NewKeywordScorer("rule-reviewer", 0),
		)
		logger.Printf("主评分模型 %s, 复核模型 %s", cfg.Model, modelB)
	} else {
		logger.Printf("主评分模型 %s, 复核使用规则评分器(设置 LLM_MODEL_B 可启用双模型交叉)", cfg.Model)
	}

	return func() api.Scorers {
		return api.Scorers{
			Primary:   primary,
			Secondary: secondary,
			Arbiter:   scoring.NewKeywordScorer("rule-arbiter", 0),
			Tolerance: 1,
		}
	}, nil
}

// buildProbePlanner 构造检索驱动的追问规划器。
//
// 默认用本地特征哈希嵌入(词面相似度, 无需密钥); 配了 EMBEDDING_API_KEY
// 后换成 OpenAI 兼容的语义嵌入。检索管道(BM25 + 向量 + RRF + 精排)不变。
// buildKnowledge 从数据库里的题库 + 内置题库构造知识库。
//
// 返回的 Corpus 同时供三处使用: 抽题(引擎)、追问(规划器)、检索(接口)。
// 另外返回 embedding 实现, 供系统自检页探测向量链路是否真的通。
func buildKnowledge(ctx context.Context, sessionStore store.SessionStore, tenant string, logger *log.Logger) (*knowledge.Corpus, rag.Embedder, error) {
	// 题库读取失败不应阻断开面: 内置题库已经能撑起一场完整面试,
	// 而"数据库里暂时读不到自定义题目"不应该变成"服务起不来"。
	items, err := sessionStore.ListQuestions(ctx, tenant)
	if err != nil {
		logger.Printf("读取租户题库失败(%v), 本次仅使用内置题库", err)
		items = nil
	}

	var emb rag.Embedder = rag.NewHashingEmbedder(256)
	if key := os.Getenv("EMBEDDING_API_KEY"); key != "" {
		emb = rag.NewOpenAIEmbedder(os.Getenv("EMBEDDING_BASE_URL"), key, os.Getenv("EMBEDDING_MODEL"))
		logger.Print("检索向量使用 OpenAI 兼容嵌入模型(语义召回)")
	} else {
		logger.Print("未配置 EMBEDDING_API_KEY, 检索向量使用本地特征哈希(词面相似度)")
	}

	corpus, err := knowledge.Build(ctx, items, emb, rag.NewLocalReranker())
	if err != nil {
		return nil, nil, fmt.Errorf("构建知识库失败: %w", err)
	}
	stats := corpus.Stats()
	logger.Printf("知识库就绪: %d 道题 / %d 条参考要点 / %d 篇检索文档 (%s, %s)",
		stats.Questions, stats.Points, stats.Documents, stats.Source, stats.Embedder)
	if missing := corpus.MissingStructuralStages(); len(missing) > 0 {
		logger.Printf("警告: 以下阶段缺少题目, 面试会跳过它们: %v", missing)
	}
	return corpus, emb, nil
}

// parseICEServers 解析逗号分隔的 ICE 服务器列表。
//
// STUN/TURN 地址因部署环境而异, 因此必须可配置。默认给一个公共 STUN,
// 让本地演示也能建立 P2P 视频; 生产应当换成自己的 TURN —— 公共 STUN
// 在有企业防火墙的环境里大概率不通。
func parseICEServers(raw string) []map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var urls []string
	for _, part := range strings.Split(raw, ",") {
		if u := strings.TrimSpace(part); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return []map[string]any{{"urls": urls}}
}

// storeKind 返回存储后端的展示名。
func storeKind(sessionStore store.SessionStore) string {
	if _, ok := sessionStore.(*store.MySQLStore); ok {
		return "MySQL"
	}
	return "内存(重启丢数据, 仅本地演示)"
}

// buildMediaProviders 从环境变量组装语音识别与合成提供方。
//
// 语音模式是"显式 opt-in": 只有配了真实密钥才启用服务端 ASR/TTS,
// 否则前端回落到浏览器自带的识别与合成。
func buildMediaProviders(logger *log.Logger) (media.ASRProvider, media.TTSProvider) {
	var asr media.ASRProvider
	var tts media.TTSProvider

	if key := os.Getenv("ASR_API_KEY"); key != "" {
		asr = &media.OpenAIASR{
			BaseURL: os.Getenv("ASR_BASE_URL"), APIKey: key, Model: os.Getenv("ASR_MODEL"),
		}
		logger.Print("语音识别已启用(OpenAI 兼容 /audio/transcriptions)")
	}
	if key := os.Getenv("TTS_API_KEY"); key != "" {
		tts = &media.OpenAITTS{
			BaseURL: os.Getenv("TTS_BASE_URL"), APIKey: key, Model: os.Getenv("TTS_MODEL"),
		}
		logger.Print("语音合成已启用(OpenAI 兼容 /audio/speech)")
	}
	if asr == nil || tts == nil {
		logger.Print("语音模式未启用(需同时配置 ASR_API_KEY 与 TTS_API_KEY); 前端回落到浏览器识别/合成")
	}
	return asr, tts
}

// runSelfTest 探测已配置的外部能力, 输出一份联调报告。
//
// 它解决的是"配了密钥却不知道通没通"这个最常见的联调痛点:
// 在正式开面之前先把每个上游 ping 一遍, 而不是让第一场面试替你做冒烟。
func runSelfTest(logger *log.Logger) {
	logger.Print("联调自检开始")

	if cfg := llm.FromEnv(); cfg.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		_, err := llm.NewClient(cfg).Chat(ctx,
			[]llm.Message{{Role: "user", Content: "ping"}}, llm.WithMaxTokens(1))
		cancel()
		if err != nil {
			logger.Printf("LLM(%s) 失败: %v", cfg.Model, err)
		} else {
			logger.Printf("LLM(%s) 正常, 耗时 %s", cfg.Model, time.Since(start).Round(time.Millisecond))
		}
	} else {
		logger.Print("LLM 未配置(LLM_API_KEY)")
	}

	if key := os.Getenv("EMBEDDING_API_KEY"); key != "" {
		emb := rag.NewOpenAIEmbedder(os.Getenv("EMBEDDING_BASE_URL"), key, os.Getenv("EMBEDDING_MODEL"))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		v, err := emb.Embed(ctx, "ping")
		cancel()
		if err != nil {
			logger.Printf("Embedding(%s) 失败: %v", emb.Name(), err)
		} else {
			logger.Printf("Embedding(%s) 正常, 维度 %d", emb.Name(), len(v))
		}
	} else {
		logger.Print("Embedding 未配置(EMBEDDING_API_KEY)")
	}

	if key := os.Getenv("TTS_API_KEY"); key != "" {
		tts := &media.OpenAITTS{BaseURL: os.Getenv("TTS_BASE_URL"), APIKey: key, Model: os.Getenv("TTS_MODEL")}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := time.Now()
		stream, err := tts.Speak(ctx, "你好", media.Voice{})
		if err != nil {
			logger.Printf("TTS 失败: %v", err)
		} else {
			first := <-stream.Chunks()
			_ = stream.Close()
			logger.Printf("TTS 正常, 首块 %d 字节, 耗时 %s",
				len(first.PCM), time.Since(start).Round(time.Millisecond))
		}
		cancel()
	} else {
		logger.Print("TTS 未配置(TTS_API_KEY)")
	}

	if os.Getenv("ASR_API_KEY") != "" {
		logger.Print("ASR 已配置; 转写接口需要真实音频, 请在正式面试中验证")
	} else {
		logger.Print("ASR 未配置(ASR_API_KEY)")
	}

	logger.Print("自检结束")
}
