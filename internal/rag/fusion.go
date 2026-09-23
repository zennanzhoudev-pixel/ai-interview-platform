package rag

import "sort"

// Fused 是 RRF 融合后的结果。
type Fused struct {
	ID    string
	Doc   Doc
	Score float64
}

// RRF 融合多路召回: score = Σ 1/(k + rank_i)。
//
// 为什么不把 BM25 的 3.2 和余弦的 0.8 直接相加? 因为两路的分数量纲
// 完全不同, 相加意味着某一路会"独裁"。RRF 只关心排名, 与量纲无关,
// 是混合检索里最省事也最稳的融合方式。k 通常取 60。
func RRF(k int, lists ...[]Scored) []Fused {
	if k <= 0 {
		k = 60
	}

	type acc struct {
		doc   Doc
		score float64
	}
	index := make(map[string]*acc)
	var order []string

	for _, list := range lists {
		for rank, item := range list {
			a, ok := index[item.ID]
			if !ok {
				a = &acc{doc: item.Doc}
				index[item.ID] = a
				order = append(order, item.ID)
			}
			a.score += 1.0 / float64(k+rank+1)
		}
	}

	out := make([]Fused, 0, len(order))
	for _, id := range order {
		out = append(out, Fused{ID: id, Doc: index[id].doc, Score: index[id].score})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score == out[b].Score {
			return out[a].ID < out[b].ID
		}
		return out[a].Score > out[b].Score
	})
	return out
}
