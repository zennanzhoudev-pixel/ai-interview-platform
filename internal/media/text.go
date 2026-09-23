package media

import "strings"

// 句末标点。中英文都要覆盖 —— 候选人混说中英技术名词是常态。
const sentenceEnders = "。！？!?;\n"

// maxSentenceRunes 是单句长度上限。超过就按逗号再切一次,
// 避免出现"第一句 60 个字"导致首字延迟被第一句拖长。
const maxSentenceRunes = 48

// SplitSentences 把话术切成句子。
//
// 逐句合成是首字延迟的关键: 整段合成必须等全部文本推理完才能出声,
// 逐句合成只要第一句推理完就能开口。同时它让"已播报到第几句"
// 变成可精确计算的量 —— 打断时只有知道 AI 到底被听到了什么,
// 才能避免下一轮重复说同一句话。
func SplitSentences(text string) []string {
	var out []string
	for _, raw := range splitAny(text, sentenceEnders) {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		out = append(out, hardWrap(s, "，,、")...)
	}
	return out
}

func splitAny(text, seps string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune(seps, r)
	})
}

// hardWrap 把过长的句子按次级标点再切开, 尽量保持停顿自然。
func hardWrap(s, seps string) []string {
	if runeLen(s) <= maxSentenceRunes {
		return []string{s}
	}

	var out []string
	var cur []rune
	rs := []rune(s)
	for _, r := range rs {
		cur = append(cur, r)
		if !strings.ContainsRune(seps, r) || len(cur) < maxSentenceRunes/2 {
			continue
		}
		out = append(out, strings.TrimSpace(string(cur)))
		cur = nil
	}
	if rest := strings.TrimSpace(string(cur)); rest != "" {
		out = append(out, rest)
	}
	return out
}

func runeLen(s string) int { return len([]rune(s)) }
