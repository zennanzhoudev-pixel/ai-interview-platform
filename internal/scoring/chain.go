package scoring

import "fmt"

// ChainScorer 按顺序尝试多个评分器, 返回第一个产出有效结果的结果。
//
// 这是"降级"的落地方式: 主评分器是大模型, 它超时、被限流、输出不合规时,
// 自动回落到规则评分器, 而不是让这场面试没有分数、让候选人干等。
//
// 降级必须可见: 结果里会带上降级来源, 报告、看板、告警都能看到。
// 悄悄降级比不降级更危险 —— 它会让整批面试的评分标准在无人察觉的
// 情况下从"大模型判断"变成"关键词匹配", 而业务方还以为一切正常。
type ChainScorer struct {
	scorers    []Scorer
	OnFallback func(primary, fallback, cause string)
}

// NewChainScorer 构造降级链。
func NewChainScorer(scorers ...Scorer) *ChainScorer {
	return &ChainScorer{scorers: scorers}
}

func (c *ChainScorer) Name() string {
	if len(c.scorers) == 0 {
		return "chain(empty)"
	}
	return "chain:" + c.scorers[0].Name()
}

// Score 依次尝试各评分器。
func (c *ChainScorer) Score(a Answer) Result {
	if len(c.scorers) == 0 {
		return Result{Model: "chain(empty)", Rationale: "没有配置任何评分器"}
	}

	var cause string
	for i, scorer := range c.scorers {
		res := scorer.Score(a)
		if res.Valid() {
			if i == 0 {
				return res
			}
			if cause == "" {
				cause = "主评分器未产出有效结果"
			}
			res.DegradedFrom = cause
			res.Rationale = fmt.Sprintf("已降级到 %s: %s。%s",
				scorer.Name(), cause, res.Rationale)
			if c.OnFallback != nil {
				c.OnFallback(c.scorers[0].Name(), scorer.Name(), cause)
			}
			return res.Enforce()
		}
		if cause == "" {
			reason := res.Rationale
			if reason == "" {
				reason = "无评分理由"
			}
			cause = fmt.Sprintf("%s 未产出有效评分(%s)", scorer.Name(), reason)
		}
	}

	// 全部失败: 返回无证据结果, 由 Enforce 作废, 让上层看到"这条没有分"。
	return Result{
		Model:     c.Name(),
		Rationale: "所有评分器都未能产出有效结果: " + cause,
	}
}
