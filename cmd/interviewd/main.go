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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
)

func main() {
	round := flag.Int("round", 1, "面试轮次 (1..5)")
	minutes := flag.Int("minutes", 45, "本轮面试时长预算 (分钟)")
	maxTurns := flag.Int("max-turns", 60, "单场最大问答轮数 (安全上限)")
	out := flag.String("out", "", "报告 JSON 输出路径, 留空则只打印成绩单")
	flag.Parse()

	total := time.Duration(*minutes) * time.Minute
	plan := orchestrator.DefaultPlan(total)
	plan.Round = *round
	bank := orchestrator.DefaultBank()

	engine := orchestrator.NewEngine(plan, bank, total,
		orchestrator.WithScorers(
			// 主面试官模型与复核模型故意设置不同的严格度, 用来演示
			// "双模型分歧 -> 取保守值" 的链路。
			// 分歧超过容忍度时的三方仲裁由单元测试覆盖 (见 scoring_test.go)。
			scoring.NewKeywordScorer("interviewer-model-a", 0),
			scoring.NewKeywordScorer("reviewer-model-b", -1),
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
