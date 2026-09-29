package knowledge

import (
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// SeedItems 返回内置题库。
//
// 内置题库的价值不是"演示数据", 而是**保证流程骨架永远完整**:
// 开场、反问、收尾这三个阶段如果没题可问, 面试会静默跳过整段流程,
// 而使用者只会觉得"这场面试怪怪的"。所以内置题永远参与构建。
//
// 内容来源严格复用 orchestrator 的默认题库与默认参考答案 ——
// 抄一份出来意味着以后改一处、忘一处, 两份数据开始漂移。
func SeedItems() []store.QuestionItem {
	bank := orchestrator.DefaultBank()
	refs := orchestrator.DefaultReferenceBank()
	index := make(map[string]orchestrator.ReferenceAnswer)
	for _, a := range refs.All() {
		index[a.QuestionID] = a
	}

	out := make([]store.QuestionItem, 0, bank.Size()+len(extraSeeds))
	for _, q := range bank.All() {
		item := store.QuestionItem{
			ID:           q.ID,
			TenantID:     "", // 内置题不属于任何租户
			Stage:        string(q.Stage),
			Competency:   q.Competency,
			Text:         q.Text,
			Keywords:     append([]string(nil), q.Keywords...),
			AntiPatterns: append([]string(nil), q.AntiPatterns...),
			Difficulty:   "senior",
			Importance:   importanceName(q.Importance),
			MaxProbe:     q.MaxProbe,
			Status:       "published",
			Version:      1,
			Tags:         []string{"内置"},
			Rounds:       roundsFor(q.Stage),
		}
		if a, ok := index[q.ID]; ok {
			for _, p := range a.Points {
				item.ReferencePoints = append(item.ReferencePoints, store.ReferencePoint{Key: p.Key, Text: p.Text})
			}
		}
		out = append(out, item)
	}
	out = append(out, extraSeeds...)
	return out
}

// importanceName 把引擎的重要性枚举转成题库里的字符串。
func importanceName(i orchestrator.Importance) string {
	switch i {
	case orchestrator.ImportanceHigh:
		return "high"
	case orchestrator.ImportanceLow:
		return "low"
	default:
		return "medium"
	}
}

// roundsFor 给出某阶段题目适用的轮次。
func roundsFor(stage orchestrator.Stage) []int {
	switch stage {
	case orchestrator.StageGreeting, orchestrator.StageCandidateQA, orchestrator.StageWrapUp:
		return []int{1, 2, 3, 4, 5}
	case orchestrator.StageResumeDeepDive:
		return []int{1, 2, 3}
	case orchestrator.StageTechFundamental:
		return []int{1, 2, 3}
	case orchestrator.StageScenarioDesign:
		return []int{3, 4}
	default:
		return []int{1}
	}
}

// extraSeeds 是内置题库的补充题。
//
// 默认题库只覆盖"分布式缓存 / 运行时 / 幂等 / 系统设计"四个方向,
// 高轮次(三面、四面)常用的"取舍与权衡""故障演练""协作与冲突"在这里补齐,
// 否则高阶候选人的面试会显得单薄。
var extraSeeds = []store.QuestionItem{
	{
		ID: "q_cache_consistency", TenantID: "", Stage: string(orchestrator.StageTechFundamental),
		Competency: "distributed_system", Text: "缓存与数据库的一致性你怎么保证? 先删缓存还是先写库, 为什么?",
		Keywords:     []string{"延迟双删", "失效", "版本号", "最终一致", "写穿透"},
		AntiPatterns: []string{"设置过期时间就好了"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "写后失效", Text: "先写库再删缓存, 用失效而不是更新"},
			{Key: "二次删除", Text: "延迟双删覆盖删缓存失败的窗口"},
			{Key: "版本号", Text: "用版本号/逻辑过期避免旧值覆盖新值"},
			{Key: "穿透击穿", Text: "区分缓存穿透、击穿与雪崩的应对方式"},
		},
		Difficulty: "senior", Importance: "high", MaxProbe: 2, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{1, 3},
	},
	{
		ID: "q_observe_prod", TenantID: "", Stage: string(orchestrator.StageTechFundamental),
		Competency: "distributed_system", Text: "线上接口 RT 突然翻倍, 你的排查路径是什么?",
		Keywords:     []string{"指标", "链路追踪", "日志", "分级定位", "复现"},
		AntiPatterns: []string{"重启试试"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "先看指标", Text: "先看 QPS/RT/错误率与依赖的黄金指标, 判断是自身还是依赖"},
			{Key: "链路归因", Text: "用链路追踪定位到具体 span, 把问题收缩到一个组件"},
			{Key: "对比变更", Text: "对比最近变更与容量变化, 优先怀疑变更和依赖限流"},
			{Key: "止血再根因", Text: "先止血(降级/限流/回滚)再定位根因, 不并发两件事"},
		},
		Difficulty: "senior", Importance: "high", MaxProbe: 2, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{1, 2, 3},
	},
	{
		ID: "q_arch_tradeoff", TenantID: "", Stage: string(orchestrator.StageScenarioDesign),
		Competency: "architecture", Text: "设计一个千万级用户的在线面试系统。你会先定哪些边界, 又会明确不做什么?",
		Keywords:     []string{"边界", "容量估算", "读写比例", "降级", "成本"},
		AntiPatterns: []string{"上微服务就能扛"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "先划边界", Text: "先明确功能边界与非目标, 避免把系统设计成什么问题都要解"},
			{Key: "容量估算", Text: "按峰值在线场次估算带宽、连接数与存储增速"},
			{Key: "长连接", Text: "长连接网关与有状态连接的调度策略"},
			{Key: "降级预案", Text: "给出分级降级方案与成本上限"},
		},
		Difficulty: "staff", Importance: "high", MaxProbe: 2, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{3, 4},
	},
	{
		ID: "q_failure_drill", TenantID: "", Stage: string(orchestrator.StageScenarioDesign),
		Competency: "architecture", Text: "如果你负责的服务在高峰期被依赖拖死, 你会怎么设计故障隔离?",
		Keywords:     []string{"熔断", "隔离", "舱壁", "限流", "超时预算"},
		AntiPatterns: []string{"加机器就好了"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "超时预算", Text: "给每个下游设超时预算, 让整体耗时可控"},
			{Key: "熔断降级", Text: "熔断 + 兜底返回值, 故障时不放大"},
			{Key: "舱壁隔离", Text: "线程池/连接池按依赖隔离, 避免单个依赖耗尽全部资源"},
			{Key: "验收方式", Text: "用故障演练验证隔离是否真的生效"},
		},
		Difficulty: "staff", Importance: "medium", MaxProbe: 2, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{3, 4},
	},
	{
		ID: "q_team_conflict", TenantID: "", Stage: string(orchestrator.StageResumeDeepDive),
		Competency: "communication", Text: "讲一次你和同事在技术方案上分歧较大的经历, 最后是怎么收敛的?",
		Keywords:     []string{"事实", "可验证", "权衡", "决策记录", "对齐目标"},
		AntiPatterns: []string{"谁级别高听谁的"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "回到目标", Text: "先把分歧收敛到共同目标与可验证的判据上"},
			{Key: "实验裁决", Text: "用压测/小流量实验代替争论"},
			{Key: "决策留痕", Text: "把结论与理由写进决策记录, 便于后来人理解"},
		},
		Difficulty: "mid", Importance: "medium", MaxProbe: 1, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{5},
	},
	{
		ID: "q_motivation", TenantID: "", Stage: string(orchestrator.StageCandidateQA),
		Competency: "communication", Text: "在上一段经历里, 最有成就感的一件事是什么? 为什么是它?",
		Keywords:     []string{"具体", "难度", "影响", "复盘"},
		AntiPatterns: []string{"没什么特别的"},
		ReferencePoints: []store.ReferencePoint{
			{Key: "具体事件", Text: "讲的是具体事件而不是泛泛的感受"},
			{Key: "难度与影响", Text: "说清难点与对业务/团队的实际影响"},
			{Key: "复盘", Text: "能说出如果重来会怎么做得更好"},
		},
		Difficulty: "mid", Importance: "low", MaxProbe: 1, Status: "published", Version: 1,
		Tags: []string{"内置"}, Rounds: []int{4, 5},
	},
}
