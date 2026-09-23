package orchestrator

import "time"

// Importance 表示一个问题的重要性, 用于时间紧张时决定取舍。
type Importance int

const (
	ImportanceLow Importance = iota
	ImportanceMedium
	ImportanceHigh
)

func (i Importance) String() string {
	switch i {
	case ImportanceHigh:
		return "high"
	case ImportanceMedium:
		return "medium"
	default:
		return "low"
	}
}

// Budget 管理一场面试的时间预算。
//
// 这是整个引擎里最"不性感"但最有价值的一段代码。没有它, AI 面试官会在
// 候选人的第一个项目上追问六层, 聊掉 30 分钟, 最后系统设计题一个字没问,
// 报告出来能力项覆盖度只有 40% —— 这场面试等于白做。
type Budget struct {
	Total   time.Duration
	Elapsed time.Duration
}

// NewBudget 构造时间预算。
func NewBudget(total time.Duration) *Budget {
	if total < 0 {
		total = 0
	}
	return &Budget{Total: total}
}

// Remaining 返回剩余时间, 不会为负。
func (b *Budget) Remaining() time.Duration {
	if b.Elapsed >= b.Total {
		return 0
	}
	return b.Total - b.Elapsed
}

// Consume 累加已消耗时间。
func (b *Budget) Consume(d time.Duration) {
	if d > 0 {
		b.Elapsed += d
	}
}

// ShouldProbe 判断是否还值得继续追问。
//
// 规则: 剩余时间不足总预算的 20% 时, 只对高重要性的考点继续追问,
// 其余一律进入下一题。这样既保住了追问深度(核心考点该问透),
// 又保住了提纲覆盖度(次要考点不恋战)。
func (b *Budget) ShouldProbe(importance Importance) bool {
	remaining := b.Remaining()
	if remaining <= 0 || b.Total <= 0 {
		return false
	}
	if remaining*5 < b.Total && importance < ImportanceHigh {
		return false
	}
	return true
}

// StageDone 判断当前阶段是否该收口。
//
// 两个触发条件:
//  1. 阶段时间用尽;
//  2. 已问够最少题数, 且剩余时间不足以再问完一轮(90 秒) —— 提前收口,
//     避免出现"开了个头就要结束"的尴尬体验。
func StageDone(spec StageSpec, asked int, stageElapsed time.Duration) bool {
	if spec.Budget > 0 && stageElapsed >= spec.Budget {
		return true
	}
	remaining := spec.Budget - stageElapsed
	return asked >= spec.MinQuestions && remaining < 90*time.Second
}
