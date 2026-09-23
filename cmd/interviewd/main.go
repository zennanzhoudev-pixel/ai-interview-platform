// Command interviewd 是面试编排引擎的离线模拟入口。
//
// 它用脚本化的候选人回答完整跑一场文本面试, 打印可读成绩单, 并可选输出 JSON 报告。
// 整条链路不依赖 ASR / LLM / 数据库, 所以可以直接放进 CI 做回归:
// 评分策略、追问逻辑、预算调度一旦被改坏, 跑一次就能发现。
//
// 真实部署时, 这里的 scriptedAnswer 换成流式 ASR 的输出,
// scoring.Scorer 换成大模型评分器, Engine 保持不变。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/api"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
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
	flag.Parse()

	if *serve != "" {
		if err := runServer(*serve, *mysqlDSN, *redisAddr); err != nil {
			fmt.Fprintf(os.Stderr, "服务启动失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	total := time.Duration(*minutes) * time.Minute
	plan := orchestrator.DefaultPlan(total)
	plan.Round = *round
	bank := orchestrator.DefaultBank()

	// 复核模型的严格度。默认与主模型一致(演示正常链路);
	// -strict 让复核模型整体保守一级, 用来观察"双模型分歧"如何进入报告。
	// 分歧超过容忍度时的三方仲裁由 internal/scoring 的单元测试覆盖。
	reviewBias := 0
	if *strict {
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
		*round, *minutes, bank.Size(), len(plan.Specs))
	fmt.Println(strings.Repeat("-", 72))

	prevStage := orchestrator.Stage("")
	decision := engine.Start()
	for i := 0; i < *maxTurns; i++ {
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

	if *out != "" {
		if err := writeReport(*out, report); err != nil {
			fmt.Fprintf(os.Stderr, "写出报告失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n报告已写入 %s\n", *out)
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
	fmt.Printf("  双模型分歧 %d 条 | 三方仲裁 %d 条 | 转人工复核 %d 条\n",
		rep.Stats.Disagreements, rep.Stats.Arbitrations, rep.Stats.HumanReviewItems)
	fmt.Printf("  平均置信度 %.2f | 耗时 %ds / 预算 %ds\n",
		rep.Stats.AvgConfidence, rep.DurationSec, rep.BudgetSec)

	fmt.Println(strings.Repeat("-", 72))
	fmt.Printf("结论: %s (置信度 %.2f)\n", rep.Recommendation, rep.Confidence)
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

// rootAnswers 是"主问题"的脚本化回答。
//
// 注意这些回答是刻意设计过的: 有的只差一个要点, 有的几乎全中,
// 用来观察引擎在不同质量回答下的追问行为。
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
		// 追问也答不到要点: 引擎应该在最大深度处停下并换题,
		// 而不是无限深挖同一个点。
		return "这个……当时没深究, 主要是先保证功能可用, 细节确实记不太清了。", 40 * time.Second
	}
	return "这块我了解一些, 具体实现是照着文档做的。", 60 * time.Second
}

// splitProbeID 把 "q_resume_zset.p2" 拆成 ("q_resume_zset", 2)。
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

// runServer 启动 Web 服务。
//
// 存储与评分器都是"可选增强": 没有 MySQL 就用内存, 没有 Redis 就跳过快照,
// 没有大模型就用规则评分。这样在一台干净的机器上
// `go run ./cmd/interviewd -serve :8080` 就能跑起完整流程,
// 而不是先让人去配一堆中间件 —— 后者通常会变成
// "项目看起来不错, 但从没真正跑起来过"。
func runServer(addr, mysqlDSN, redisAddr string) error {
	logger := log.New(os.Stderr, "[interviewd] ", log.LstdFlags)

	sessionStore, closeStore, err := openStore(mysqlDSN, logger)
	if err != nil {
		return err
	}
	defer closeStore()

	ckpt, err := openCheckpoint(redisAddr, logger)
	if err != nil {
		return err
	}

	scorerFactory, err := buildScorerFactory(logger)
	if err != nil {
		return err
	}
	planner, err := buildProbePlanner(logger)
	if err != nil {
		return err
	}

	srv := api.NewServer(api.Config{
		Store:        sessionStore,
		Checkpoint:   ckpt,
		Scorers:      scorerFactory,
		ProbePlanner: planner,
		Logger:       logger,
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 写超时留空: 面试是长连接场景, 设了写超时会把长面试从服务端切断。
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Print("收到退出信号, 正在优雅关闭(等待在途面试结束)…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// 长连接会让 Shutdown 一直等到面试结束, 因此必须有上限。
		// 生产环境还应主动向客户端广播"服务即将重启", 让前端立刻重连 ——
		// 重连后会自动从已落库的问答恢复进度, 候选人几乎无感。
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("优雅关闭超时, 强制退出: %v", err)
		}
	}()

	logger.Printf("面试服务已启动: http://localhost%s", addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Print("已停止")
	return nil
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
		logger.Printf("主评分模型 %s, 复核使用规则评分器(设置 LLM_MODEL_B 可启用双模型交叉)",
			cfg.Model)
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
// 后换成 OpenAI 兼容的语义嵌入。检索管道(BM25 + 向量 + RRF + 精排)不变,
// 换的只是向量质量。
func buildProbePlanner(logger *log.Logger) (orchestrator.ProbePlanner, error) {
	bank := orchestrator.DefaultReferenceBank()

	var emb rag.Embedder = rag.NewHashingEmbedder(256)
	if key := os.Getenv("EMBEDDING_API_KEY"); key != "" {
		emb = rag.NewOpenAIEmbedder(
			os.Getenv("EMBEDDING_BASE_URL"),
			key,
			os.Getenv("EMBEDDING_MODEL"),
		)
		logger.Print("检索向量使用 OpenAI 兼容嵌入模型(语义召回)")
	} else {
		logger.Print("未配置 EMBEDDING_API_KEY, 检索向量使用本地特征哈希(词面相似度)")
	}

	retriever, err := bank.BuildRetriever(context.Background(), emb, rag.NewLocalReranker())
	if err != nil {
		return nil, fmt.Errorf("构建参考题库检索器失败: %w", err)
	}
	return orchestrator.NewRAGProbePlanner(bank, retriever), nil
}
