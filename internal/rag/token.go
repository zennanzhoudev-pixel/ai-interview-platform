// Package rag 实现参考题库的混合检索: BM25 + 向量 + RRF 融合 + 精排。
//
// 面试场景的检索有个明显特点: 大量查询里都是"分布式锁 / ZSet / Goroutine /
// 幂等"这类专有名词。纯向量检索对这些术语召回不稳定, 纯 BM25 又覆盖不了
// 语义泛化("怎么保证缓存一致性"和"缓存失效怎么处理"字面几乎不重叠)。
// 所以采用混合检索 —— 这正是设计方案里的结论, 这里把它实现出来。
package rag

import "strings"

// Tokenize 把一段文本切成检索用 token。
//
// 中英文混排时: 英文按词切(小写、保留字母数字与下划线), 中文按字二元组切。
// 中文单字分词会丢失"分布式锁"这类多字术语的边界, 而二元组是纯本地实现里
// 最划算的折中 —— 不需要词典, 又能把常见二字术语稳稳地命中。
func Tokenize(text string) []string {
	var out []string
	var ascii strings.Builder

	flush := func() {
		if ascii.Len() > 0 {
			out = append(out, strings.ToLower(ascii.String()))
			ascii.Reset()
		}
	}

	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if isASCIIWord(r) {
			ascii.WriteRune(r)
			continue
		}
		flush()
		if isCJK(r) && i+1 < len(runes) && isCJK(runes[i+1]) {
			out = append(out, string([]rune{r, runes[i+1]}))
			i++ // 跳过下一字, 保证二元组不重叠、覆盖完整边界
		} else if isCJK(r) {
			out = append(out, string(r))
		}
	}
	flush()
	return out
}

func isASCIIWord(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func isCJK(r rune) bool {
	// 中日韩统一表意文字主区 + 扩展 A, 覆盖面试语料里的绝大多数汉字。
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF)
}
