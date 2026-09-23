package orchestrator

// Question 是问题 DAG 的一个节点。
//
// Keywords 是本题的"判定要点"。它有三个用途:
//   - 评分: 命中比例映射到 rubric 等级;
//   - 追问: 未命中的要点就是追问方向(Missing -> probeFor);
//   - 覆盖度: 面试官可以直接看到哪些要点没被触及。
//
// 真实系统里 Keywords 由题目评审流程产出, 不交给模型现编。
type Question struct {
	ID           string
	Stage        Stage
	Competency   string
	Text         string
	Keywords     []string
	AntiPatterns []string
	Importance   Importance
	MaxProbe     int // 最大追问深度。超过 2 层很容易把候选人问死, 也会拖垮节奏。
}

// Bank 是题库, 对应库表 question_bank。
// 生产环境由 ES(BM25) + 向量检索替代, 这里用内存实现保证离线可跑。
type Bank struct {
	questions []Question
}

// NewBank 构造题库。
func NewBank(qs ...Question) *Bank { return &Bank{questions: qs} }

// Next 返回该阶段下一个尚未使用的问题。
func (b *Bank) Next(stage Stage, used map[string]bool) (Question, bool) {
	for _, q := range b.questions {
		if q.Stage != stage || used[q.ID] {
			continue
		}
		return q, true
	}
	return Question{}, false
}

// ByID 按 ID 取题。
func (b *Bank) ByID(id string) (Question, bool) {
	for _, q := range b.questions {
		if q.ID == id {
			return q, true
		}
	}
	return Question{}, false
}

// Size 返回题目总数。
func (b *Bank) Size() int { return len(b.questions) }

// DefaultBank 是一份 Go 后端岗位一面的演示题库。
//
// 题目围绕"分布式缓存 / 运行时 / 幂等 / 系统设计"四个方向,
// 每题的 Keywords 就是该题的能力判定要点。
func DefaultBank() *Bank {
	return NewBank(
		Question{
			ID:         "q_greet",
			Stage:      StageGreeting,
			Text:       "你好, 我是本轮的 AI 面试官。开始前请用一分钟做个简单的自我介绍, 重点讲一下你最有代表性的项目。",
			Importance: ImportanceMedium,
			MaxProbe:   1,
		},
		Question{
			ID:           "q_resume_zset",
			Stage:        StageResumeDeepDive,
			Competency:   "project_depth",
			Text:         "简历里提到你用 ZSet 做索引把查询 RT 降低了 70%, 具体是怎么做的?",
			Keywords:     []string{"ZSet", "score", "排序", "内存", "70%"},
			AntiPatterns: []string{"就是把数据都放内存里"},
			Importance:   ImportanceHigh,
			MaxProbe:     2,
		},
		Question{
			ID:         "q_resume_wasm",
			Stage:      StageResumeDeepDive,
			Competency: "tech_choice",
			Text:       "你提到用 WASM 插件运行时做编译产物复用, 把首响从 5-8 秒降到 1 秒。为什么选这个方案?",
			Keywords:   []string{"WASM", "编译", "复用", "冷启动", "隔离"},
			Importance: ImportanceHigh,
			MaxProbe:   2,
		},
		Question{
			ID:           "q_resume_flow",
			Stage:        StageResumeDeepDive,
			Competency:   "project_depth",
			Text:         "AI 视频生成那条四阶段流水线, 如果第二阶段失败了怎么处理? 怎么保证不会重复扣费?",
			Keywords:     []string{"状态机", "幂等", "重试", "断点", "补偿"},
			AntiPatterns: []string{"失败了重新跑一遍就行"},
			Importance:   ImportanceHigh,
			MaxProbe:     2,
		},
		Question{
			ID:           "q_go_gmp",
			Stage:        StageTechFundamental,
			Competency:   "language_core",
			Text:         "讲讲 Go 的 GMP 调度模型, 以及它相对线程模型在高并发长连接场景下省在哪。",
			Keywords:     []string{"GMP", "本地队列", "抢占", "work stealing", "用户态"},
			AntiPatterns: []string{"goroutine 就是轻量级线程, 没了"},
			Importance:   ImportanceMedium,
			MaxProbe:     2,
		},
		Question{
			ID:         "q_go_gc",
			Stage:      StageTechFundamental,
			Competency: "language_core",
			Text:       "Go 的 GC 是怎么做的? 什么情况下会造成 STW 变长?",
			Keywords:   []string{"三色", "写屏障", "STW", "并发标记", "混合写屏障"},
			Importance: ImportanceMedium,
			MaxProbe:   2,
		},
		Question{
			ID:           "q_dist_idem",
			Stage:        StageTechFundamental,
			Competency:   "distributed_system",
			Text:         "MQ 消费端怎么保证幂等? 如果是跨服务的场景呢?",
			Keywords:     []string{"幂等", "去重", "唯一键", "状态机", "最终一致"},
			AntiPatterns: []string{"不用管重复消费"},
			Importance:   ImportanceHigh,
			MaxProbe:     2,
		},
		Question{
			ID:           "q_arch_limit",
			Stage:        StageScenarioDesign,
			Competency:   "architecture",
			Text:         "设计一个支撑招聘旺季的限流系统: 峰值每秒 5 万请求, 要求故障时不拖垮主站。你会怎么拆?",
			Keywords:     []string{"令牌桶", "滑动窗口", "热点", "降级", "容量估算"},
			AntiPatterns: []string{"Redis 计数就够了"},
			Importance:   ImportanceHigh,
			MaxProbe:     2,
		},
		Question{
			ID:         "q_candidate_qa",
			Stage:      StageCandidateQA,
			Text:       "我的问题问完了, 你有什么想了解的吗?",
			Importance: ImportanceLow,
			MaxProbe:   0,
		},
		Question{
			ID:         "q_wrap",
			Stage:      StageWrapUp,
			Text:       "今天我们就聊到这里, 后续结果会在 3 个工作日内同步给你, 感谢你的时间。",
			Importance: ImportanceLow,
			MaxProbe:   0,
		},
	)
}
