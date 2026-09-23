package rag

import (
	"math"
	"sort"
)

// Doc 是一篇可检索文档(参考答案)。
type Doc struct {
	ID     string
	Text   string
	Meta   map[string]string
	Tokens []string // 预分词结果; 为空时按 Text 现切
}

// Scored 是带分数的检索结果。
type Scored struct {
	ID    string
	Doc   Doc
	Score float64
}

// BM25 是词频-逆文档频率的经典实现(k1、b 取标准默认值)。
type BM25 struct {
	k1, b float64

	docs   []Doc
	df     map[string]int
	length []int
	avgLen float64
}

// NewBM25 构造 BM25 索引。
func NewBM25(k1, b float64) *BM25 {
	if k1 <= 0 {
		k1 = 1.5
	}
	if b < 0 || b > 1 {
		b = 0.75
	}
	return &BM25{k1: k1, b: b, df: make(map[string]int)}
}

// Add 加入一篇文档。
func (m *BM25) Add(doc Doc) {
	tokens := doc.Tokens
	if len(tokens) == 0 {
		tokens = Tokenize(doc.Text)
	}
	cp := Doc{ID: doc.ID, Text: doc.Text, Meta: doc.Meta, Tokens: tokens}

	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			m.df[t]++
		}
	}
	m.docs = append(m.docs, cp)
	m.length = append(m.length, len(tokens))

	sum := 0
	for _, n := range m.length {
		sum += n
	}
	m.avgLen = float64(sum) / float64(len(m.length))
}

// DocCount 返回索引中的文档数。
func (m *BM25) DocCount() int { return len(m.docs) }

// Search 返回与 query 最相关的 k 篇文档, 按分数降序。
func (m *BM25) Search(query string, k int) []Scored {
	qt := Tokenize(query)
	if k <= 0 {
		k = 10
	}

	type scored struct {
		idx   int
		score float64
	}
	var results []scored
	for i, doc := range m.docs {
		s := m.score(doc, qt)
		if s > 0 {
			results = append(results, scored{idx: i, score: s})
		}
	}
	sort.Slice(results, func(a, b int) bool { return results[a].score > results[b].score })
	if len(results) > k {
		results = results[:k]
	}

	out := make([]Scored, 0, len(results))
	for _, r := range results {
		out = append(out, Scored{ID: m.docs[r.idx].ID, Doc: m.docs[r.idx], Score: r.score})
	}
	return out
}

func (m *BM25) score(doc Doc, queryTokens []string) float64 {
	freq := make(map[string]int, len(doc.Tokens))
	for _, t := range doc.Tokens {
		freq[t]++
	}

	n := float64(len(m.docs))
	var sum float64
	for _, q := range queryTokens {
		f := float64(freq[q])
		if f == 0 {
			continue
		}
		df := float64(m.df[q])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		norm := f + m.k1*(1-m.b+m.b*float64(len(doc.Tokens))/m.avgLen)
		sum += idf * (f * (m.k1 + 1)) / norm
	}
	return sum
}
