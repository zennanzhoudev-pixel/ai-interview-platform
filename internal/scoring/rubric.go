// Package scoring 负责面试评分: rubric 等级判定、证据绑定、双模型交叉仲裁。
//
// 三条核心原则(也是企业级 AI 面试系统与"套个大模型聊天"的分界线):
//
//  1. 证据绑定(Evidence Binding): 任何一个等级分都必须绑定候选人原话片段,
//     没有证据的分数一律作废。这是抑制 LLM 幻觉最有效的手段。
//  2. 单模型分数不可信: 双模型交叉评分, 分歧超过阈值必须仲裁或转人工复核。
//  3. 命中反例直接降级: 表达流畅不能掩盖认知错误。
package scoring

import (
	"encoding/json"
	"strings"
	"time"
)

// Level 是 rubric 的五级能力等级。LevelUnknown 表示"未评分"或"无证据"。
type Level int

const (
	LevelUnknown    Level = iota // L0 未评分 / 无证据
	LevelNone                    // L1 未掌握
	LevelAware                   // L2 了解, 知道概念但无法落地
	LevelProficient              // L3 熟练, 能设计并说明权衡
	LevelAdvanced                // L4 精通, 能预判瓶颈并给出降级方案
	LevelExpert                  // L5 专家, 能提出跨系统权衡的额外方案
)

var levelNames = [...]string{
	LevelUnknown:    "L0 未评分",
	LevelNone:       "L1 未掌握",
	LevelAware:      "L2 了解",
	LevelProficient: "L3 熟练",
	LevelAdvanced:   "L4 精通",
	LevelExpert:     "L5 专家",
}

var levelLabels = [...]string{
	LevelUnknown:    "未评分",
	LevelNone:       "未掌握",
	LevelAware:      "了解",
	LevelProficient: "熟练",
	LevelAdvanced:   "精通",
	LevelExpert:     "专家",
}

// levelScores 把等级换算成 0..100 分。
//
// 换算规则是显式且单调的, 而且刻意不采用 0/20/40/60/80/100 的均分:
// 面试评价不是考试, 把"未掌握"映射到 0 分会让报告失去区分度,
// 也会让候选人觉得被"判了死刑"。用人方真正关心的是"能不能用",
// 所以 L1 给 40、L4 给 88 —— 下限不至于羞辱人, 上限仍留出差距。
//
// 这套映射必须写死在代码里并接受评审。让模型决定"L3 到底是多少分"
// 是评分系统里最不该出现的自由度。
var levelScores = [...]int{
	LevelUnknown:    0,
	LevelNone:       40,
	LevelAware:      58,
	LevelProficient: 74,
	LevelAdvanced:   88,
	LevelExpert:     96,
}

// Score 返回该等级对应的 0..100 分。
func (l Level) Score() int {
	if l < 0 || int(l) >= len(levelScores) {
		return 0
	}
	return levelScores[l]
}

// Label 返回等级的中文名(不含 L 编号), 用于注入评分 prompt。
func (l Level) Label() string {
	if l < 0 || int(l) >= len(levelLabels) {
		return "未知"
	}
	return levelLabels[l]
}

// Number 返回 1..5 的展示序号, LevelUnknown 返回 0。
func (l Level) Number() int {
	if l <= LevelUnknown {
		return 0
	}
	if l > LevelExpert {
		return 5
	}
	return int(l)
}

func (l Level) String() string {
	if l < 0 || int(l) >= len(levelNames) {
		return "L? 未知"
	}
	return levelNames[l]
}

// LevelFromNumber 把 1..5 的序号还原成 Level, 超出范围会被夹紧。
func LevelFromNumber(n int) Level {
	switch {
	case n <= 1:
		return LevelNone
	case n == 2:
		return LevelAware
	case n == 3:
		return LevelProficient
	case n == 4:
		return LevelAdvanced
	default:
		return LevelExpert
	}
}

// Bias 按级别偏移 Level, 用于刻画不同模型的严格程度(评测与仲裁场景)。
func (l Level) Bias(delta int) Level {
	if l == LevelUnknown {
		return LevelUnknown
	}
	return LevelFromNumber(l.Number() + delta)
}

// EvidenceKind 区分支持性证据与反对性证据。
type EvidenceKind string

const (
	// EvidenceSupport 支撑该能力项得分的候选人原话。
	EvidenceSupport EvidenceKind = "support"
	// EvidenceAgainst 命中反例, 或回答完全未触及判定要点的证据。
	EvidenceAgainst EvidenceKind = "against"
)

// Evidence 是一条可回溯到原始问答的证据片段。
//
// 它是整份报告的基石: 面试官点开一个维度分, 能直接跳到对应录音位置,
// 听到候选人当时说了什么。
type Evidence struct {
	TurnID     string        `json:"turn_id"`
	QuestionID string        `json:"question_id"`
	Kind       EvidenceKind  `json:"kind"`
	Matched    string        `json:"matched,omitempty"` // 命中的判定要点
	Quote      string        `json:"quote"`             // 候选人原话
	At         time.Duration `json:"at_ms"`             // 距面试开始的偏移, 用于录音回放定位
}

// Answer 是一次待评分的问答。
type Answer struct {
	QuestionID   string
	Competency   string
	Question     string
	Text         string   // 候选人回答(真实链路上来自流式 ASR)
	Keywords     []string // 判定要点: 命中越多等级越高
	AntiPatterns []string // 反例: 命中即降级到 L1
}

// Result 是单个评分模型对一次回答的评分结果。
type Result struct {
	Model      string     `json:"model"`
	Level      Level      `json:"-"`
	LevelName  string     `json:"level"`
	Matched    []string   `json:"matched,omitempty"`
	Missing    []string   `json:"missing,omitempty"`
	AntiHits   []string   `json:"anti_hits,omitempty"`
	Evidence   []Evidence `json:"evidence"`
	Confidence float64    `json:"confidence"`
	Rationale  string     `json:"rationale"`
	// DegradedFrom 非空表示这条分数来自备用评分器, 内容记录降级原因。
	// 降级必须是可见的: 悄悄降级会让整批面试的评分标准在无人察觉时改变。
	DegradedFrom string `json:"degraded_from,omitempty"`
}

// Valid 判断该结果是否可采纳: 必须有等级且有证据。
func (r Result) Valid() bool {
	return len(r.Evidence) > 0 && r.Level > LevelUnknown
}

// Number 返回 0..5, 便于做一致性指标计算(MAE / Kappa 输入的原子)。
func (r Result) Number() int { return r.Level.Number() }

// Enforce 执行证据绑定规则: 没有任何证据的评分不允许带等级。
//
// 这是"抗幻觉"的最后一道闸门 —— 无论上游是规则引擎还是大模型,
// 只要拿不出证据, 分数就不作数。
func (r Result) Enforce() Result {
	r.LevelName = r.Level.String()
	if len(r.Evidence) > 0 {
		return r
	}
	r.Level = LevelUnknown
	r.LevelName = LevelUnknown.String()
	r.Confidence = 0
	if r.Rationale == "" {
		r.Rationale = "无证据绑定, 评分作废"
	}
	return r
}

// Scorer 是可插拔的评分器。规则评分器、大模型评分器、人工评分器实现同一接口,
// 因此"双模型交叉"里的两个模型可以是任意组合。
type Scorer interface {
	Name() string
	Score(a Answer) Result
}

// UnmarshalJSON 从 JSON 恢复 Result, 并把展示名还原成等级枚举。
//
// Level 本身不参与序列化(用 LevelName 更直观、也更稳定), 但恢复时
// 它必须被还原 —— 否则断线重连后的报告等级会全部变成 0,
// 而且不会有任何报错, 只会静悄悄地给出一份"全员未评分"的报告。
func (r *Result) UnmarshalJSON(data []byte) error {
	type alias Result
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = Result(aux)
	r.Level = parseLevelName(r.LevelName)
	return nil
}

// parseLevelName 把 "L3 熟练" / "L3" / "3" 解析回等级。
func parseLevelName(name string) Level {
	trimmed := strings.TrimSpace(name)
	for i, n := range levelNames {
		if n == trimmed {
			return Level(i)
		}
	}
	for _, r := range trimmed {
		if r >= '1' && r <= '5' {
			return LevelFromNumber(int(r - '0'))
		}
	}
	return LevelUnknown
}
