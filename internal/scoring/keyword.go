package scoring

import (
	"fmt"
	"strings"
)

// KeywordScorer 是确定性的规则评分器。
//
// 它有明确的定位, 不是"简化版大模型":
//   - 作为离线自测基线: 在没有网络、没有模型额度时也能完整跑通全链路;
//   - 作为降级路径: LLM 供应商不可用时, 面试不能中断, 至少要能给出可解释的分数;
//   - 作为交叉验证的一侧: 规则分与大模型分分歧过大时, 往往意味着候选人的
//     表述绕开了关键点, 值得人工看一眼。
type KeywordScorer struct {
	model string
	bias  int
}

// maxLevel 是规则评分器能给出的最高等级。
//
// 关键词命中率只能证明"候选人说到了这些点", 证明不了他"能提出我们没想到的
// 跨系统方案" —— 而后者才是 L5 的定义。所以规则评分器封顶 L4,
// L5 只能由大模型评分器或人类面试官给出。
//
// 这不是妥协, 而是有意的能力边界: 规则拿不准的地方就不要装作拿得准。
const maxLevel = LevelAdvanced

// NewKeywordScorer 构造规则评分器。bias 为级别偏移, 负数表示更严格。
func NewKeywordScorer(model string, bias int) *KeywordScorer {
	return &KeywordScorer{model: model, bias: bias}
}

func (s *KeywordScorer) Name() string { return s.model }

func (s *KeywordScorer) Score(a Answer) Result {
	res := Result{Model: s.model}
	text := strings.TrimSpace(a.Text)

	if text == "" {
		res.Level = LevelNone
		res.Evidence = []Evidence{{
			QuestionID: a.QuestionID,
			Kind:       EvidenceAgainst,
			Quote:      "(未作答)",
		}}
		res.Rationale = "候选人未作答"
		res.Confidence = 0.9
		return res.Enforce()
	}

	lower := strings.ToLower(text)
	for _, kw := range a.Keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			res.Matched = append(res.Matched, kw)
			res.Evidence = append(res.Evidence, Evidence{
				QuestionID: a.QuestionID,
				Kind:       EvidenceSupport,
				Matched:    kw,
				Quote:      sentenceAround(text, kw),
			})
			continue
		}
		res.Missing = append(res.Missing, kw)
	}

	for _, ap := range a.AntiPatterns {
		if strings.Contains(lower, strings.ToLower(ap)) {
			res.AntiHits = append(res.AntiHits, ap)
			res.Evidence = append(res.Evidence, Evidence{
				QuestionID: a.QuestionID,
				Kind:       EvidenceAgainst,
				Matched:    ap,
				Quote:      sentenceAround(text, ap),
			})
		}
	}

	total := len(a.Keywords)
	res.Level = levelFromRatio(len(res.Matched), total)
	if len(res.AntiHits) > 0 && res.Level > LevelNone {
		res.Level = LevelNone
	}

	switch {
	case len(res.AntiHits) > 0:
		res.Rationale = fmt.Sprintf("命中断言反例 %v, 直接降级到 %s", res.AntiHits, res.Level)
	case total == 0:
		res.Rationale = "非考察项, 不参与评分"
	default:
		res.Rationale = fmt.Sprintf("命中 %d/%d 个判定要点: %s", len(res.Matched), total, res.Level)
	}

	if total > 0 {
		ratio := float64(len(res.Matched)) / float64(total)
		res.Confidence = 0.5 + 0.45*ratio
	}

	// 回答存在但一个要点都没碰到: 补一条反对证据, 保证"低分也有据可依"。
	if len(res.Evidence) == 0 && total > 0 {
		res.Evidence = append(res.Evidence, Evidence{
			QuestionID: a.QuestionID,
			Kind:       EvidenceAgainst,
			Quote:      truncate(text, 60),
		})
	}

	res.Level = res.Level.Bias(s.bias)
	if res.Level > maxLevel {
		res.Level = maxLevel
	}
	// 合并重复证据: 一句话命中多个要点时只留一条原话。
	res.Evidence = dedupEvidence(res.Evidence)
	return res.Enforce()
}

// levelFromRatio 把命中比例映射到等级。阈值取整是为了让面试官容易复核:
// 全中才是 L5, 八成是 L4, 一半是 L3。
func levelFromRatio(matched, total int) Level {
	if total == 0 {
		return LevelUnknown
	}
	switch r := float64(matched) / float64(total); {
	case r >= 1.0:
		return LevelExpert
	case r >= 0.8:
		return LevelAdvanced
	case r >= 0.5:
		return LevelProficient
	case r > 0:
		return LevelAware
	default:
		return LevelNone
	}
}

var sentenceBoundaries = "。！？；\n.!?;"

// sentenceAround 截取命中要点所在的那句话, 而不是整段回答。
// 证据越短, 人工复核越快。
func sentenceAround(text, kw string) string {
	rs := []rune(text)
	lrs := []rune(strings.ToLower(text))
	kr := []rune(strings.ToLower(kw))
	idx := indexRunes(lrs, kr)
	if idx < 0 {
		return truncate(text, 60)
	}

	start := idx
	for start > 0 && !isBoundary(rs[start-1]) {
		start--
	}
	end := idx + len(kr)
	for end < len(rs) && !isBoundary(rs[end]) {
		end++
	}
	return strings.TrimSpace(string(rs[start:end]))
}

func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func isBoundary(r rune) bool {
	return strings.ContainsRune(sentenceBoundaries, r)
}

func truncate(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "..."
}
