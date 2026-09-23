package scoring

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Verdict 是双模型交叉评分的完整结论, 也是落库时 evaluation_dim 的来源。
type Verdict struct {
	Primary          Result  `json:"primary"`
	Secondary        Result  `json:"secondary"`
	Arbiter          *Result `json:"arbiter,omitempty"`
	Gap              int     `json:"gap"`        // 两个模型的级别差
	Arbitrated       bool    `json:"arbitrated"` // 是否触发了仲裁
	NeedsHumanReview bool    `json:"needs_human_review"`
	Final            Result  `json:"final"`
}

// CrossCheck 执行双模型交叉评分。
//
// 这是"评分一致性"的第一道防线, 规则很简单但很关键:
//
//	分歧 <= tolerance  -> 取保守值(较低等级)并合并证据
//	分歧 >  tolerance  -> 有仲裁模型则取三方中位数; 没有则标记转人工复核
//
// 为什么分歧时取保守值而不是平均值? 因为招聘场景下, 误判一个不合格候选人
// 的成本远低于漏掉一个合格候选人带来的连锁面试成本 —— 但这个取舍必须显式
// 写出来并可配置, 不能藏在代码里。
func CrossCheck(a Answer, primary, secondary, arbiter Scorer, tolerance int) Verdict {
	p := primary.Score(a).Enforce()
	s := secondary.Score(a).Enforce()

	v := Verdict{Primary: p, Secondary: s}
	v.Gap = absInt(p.Number() - s.Number())

	if v.Gap <= tolerance {
		v.Final = conservativeMerge(p, s,
			fmt.Sprintf("双模型分歧 %d 级, 在容忍范围内, 取保守值", v.Gap))
		return v
	}

	if arbiter == nil {
		v.NeedsHumanReview = true
		v.Final = conservativeMerge(p, s,
			fmt.Sprintf("双模型分歧 %d 级且无仲裁模型, 已转人工复核", v.Gap))
		return v
	}

	third := arbiter.Score(a).Enforce()
	v.Arbiter = &third
	v.Arbitrated = true
	v.Final = arithmeticMedian(p, s, third)
	return v
}

// conservativeMerge 取两个结果中较低的等级, 并合并全部证据与缺失要点。
//
// 合并 Missing 很关键: 追问 Agent 依赖 Missing 决定追问方向,
// 保守合并能保证"两个模型都认为缺失"的点不会因为另一个模型的宽松而丢失。
func conservativeMerge(a, b Result, rationale string) Result {
	level := a.Level
	if b.Level.Number() < level.Number() {
		level = b.Level
	}
	conf := math.Min(a.Confidence, b.Confidence)
	if !a.Valid() || !b.Valid() {
		conf = 0
	}
	out := Result{
		Model:        a.Model + " + " + b.Model,
		Matched:      union(a.Matched, b.Matched),
		Missing:      union(a.Missing, b.Missing),
		AntiHits:     union(a.AntiHits, b.AntiHits),
		Evidence:     dedupEvidence(concatEvidence(a.Evidence, b.Evidence)),
		Confidence:   conf,
		Rationale:    rationale,
		DegradedFrom: firstNonEmpty(a.DegradedFrom, b.DegradedFrom),
	}
	out.Level = level
	return out.Enforce()
}

// arithmeticMedian 在三方评分中取中位数: 既不偏袒严格模型, 也不偏袒宽松模型。
// 证据与要点取三方并集, 保证报告信息不因仲裁而丢失。
func arithmeticMedian(a, b, c Result) Result {
	rs := []Result{a, b, c}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Number() < rs[j].Number() })
	mid := rs[1]

	out := Result{
		Model: fmt.Sprintf("arbitration(%s | %s | %s)",
			a.Model, b.Model, c.Model),
		Matched:      unionAll(a.Matched, b.Matched, c.Matched),
		Missing:      unionAll(a.Missing, b.Missing, c.Missing),
		AntiHits:     unionAll(a.AntiHits, b.AntiHits, c.AntiHits),
		Evidence:     dedupEvidence(concatEvidence(a.Evidence, b.Evidence, c.Evidence)),
		Confidence:   math.Min(math.Min(a.Confidence, b.Confidence), c.Confidence),
		DegradedFrom: firstNonEmpty(a.DegradedFrom, b.DegradedFrom, c.DegradedFrom),
		Rationale: fmt.Sprintf("双模型分歧 %d 级, 仲裁模型取中位数 %s",
			absInt(a.Number()-b.Number()), mid.Level),
	}
	// 仲裁结论的置信度取三方下界; 只要任意一方命中了反例, 等级直接压到 L1。
	level := mid.Level
	if len(out.AntiHits) > 0 {
		level = LevelNone
	}
	out.Level = level
	return out.Enforce()
}

func union(a, b []string) []string { return unionAll(a, b) }

func unionAll(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, g := range groups {
		for _, s := range g {
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// dedupEvidence 合并重复证据: 同一句原话不论命中几个要点、被几个模型命中,
// 都只保留一条, 命中的要点合并展示。
//
// 为什么要合并: 候选人一句话里常常同时命中 4 个要点, 如果每个要点占一条证据,
// 报告里就会出现 4 行一模一样的原话。人工复核的时间是稀缺资源,
// 证据列表必须一眼能扫完。
func dedupEvidence(in []Evidence) []Evidence {
	index := make(map[string]int, len(in))
	var out []Evidence
	var matched [][]string

	for _, e := range in {
		key := string(e.Kind) + "|" + e.Quote
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, e)
			matched = append(matched, nil)
		}
		if e.Matched == "" {
			continue
		}
		dup := false
		for _, m := range matched[i] {
			if m == e.Matched {
				dup = true
				break
			}
		}
		if !dup {
			matched[i] = append(matched[i], e.Matched)
		}
	}

	for i := range out {
		if len(matched[i]) == 0 {
			continue
		}
		sort.Strings(matched[i])
		out[i].Matched = strings.Join(matched[i], " / ")
	}
	return out
}

func concatEvidence(groups ...[]Evidence) []Evidence {
	var out []Evidence
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
