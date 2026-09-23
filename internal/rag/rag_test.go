package rag

import (
	"context"
	"testing"
)

func TestTokenizeMixesCJKAndASCII(t *testing.T) {
	got := Tokenize("用 ZSet 索引做排序")
	want := []string{"用", "zset", "索引", "做排", "序"}
	if len(got) != len(want) {
		t.Fatalf("分词结果数量不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个 token = %q, 期望 %q (全部: %v)", i, got[i], want[i], got)
		}
	}
}

func TestBM25PrefersRarerAndFrequentTerms(t *testing.T) {
	m := NewBM25(1.5, 0.75)
	m.Add(Doc{ID: "a", Text: "分布式锁 分布式锁 分布式锁"})
	m.Add(Doc{ID: "b", Text: "分布式锁"})
	m.Add(Doc{ID: "c", Text: "gRPC 拦截器"})

	got := m.Search("分布式锁", 10)
	if len(got) == 0 || got[0].ID != "a" {
		t.Fatalf("词频更高的文档应排第一: %+v", got)
	}
	if len(got) < 2 {
		t.Fatalf("应同时命中 a 与 b")
	}

	// 只有 c 包含 "gRPC", 稀有词应把它推到第一位
	got = m.Search("gRPC", 10)
	if len(got) == 0 || got[0].ID != "c" {
		t.Fatalf("稀有词应命中文档 c: %+v", got)
	}
}

func TestHashingEmbedderMeasuresLexicalSimilarity(t *testing.T) {
	emb := NewHashingEmbedder(256)
	ctx := context.Background()

	a, _ := emb.Embed(ctx, "用 ZSet 的 score 承载排序权重")
	b, _ := emb.Embed(ctx, "用 ZSet 的 score 承载排序权重")
	c, _ := emb.Embed(ctx, "三色标记与混合写屏障")

	if cosine(a, b) < 0.99 {
		t.Fatalf("相同文本应几乎完全相似: %.4f", cosine(a, b))
	}
	if cosine(a, c) >= cosine(a, b) {
		t.Fatalf("无关文本相似度应显著低于相同文本: same=%.4f diff=%.4f",
			cosine(a, b), cosine(a, c))
	}

	// 确定性: 两次计算结果必须一致
	a2, _ := emb.Embed(ctx, "用 ZSet 的 score 承载排序权重")
	if cosine(a, a2) != 1.0 {
		t.Fatalf("嵌入必须确定性: %.4f", cosine(a, a2))
	}
}

func TestRRFRewardsConsensusOverSoloChampion(t *testing.T) {
	// X 是 BM25 的冠军, Y 是向量的冠军, 但 C 同时出现在两路第二名:
	// RRF 里"共识"应该胜过"单路冠军" —— 这是它比简单加权更合理的地方。
	a := []Scored{{ID: "X"}, {ID: "C"}}
	b := []Scored{{ID: "Y"}, {ID: "C"}}

	got := RRF(60, a, b)
	if got[0].ID != "C" {
		t.Fatalf("被两路同时命中的文档应排第一, 实际 %+v", got)
	}
}

func TestRetrieverEndToEnd(t *testing.T) {
	emb := NewHashingEmbedder(256)
	r := NewRetriever(emb, NewLocalReranker(), WithTopK(3))
	ctx := context.Background()

	_ = r.AddDoc(ctx, Doc{ID: "q_zset::内存", Text: "内存增长与大 key 分片"})
	_ = r.AddDoc(ctx, Doc{ID: "q_zset::score", Text: "用 ZSet 的 score 承载排序权重"})
	_ = r.AddDoc(ctx, Doc{ID: "q_gc::三色", Text: "三色标记与混合写屏障"})

	got, err := r.Search(ctx, "排行榜怎么排序的 score 权重")
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(got) == 0 || got[0].ID != "q_zset::score" {
		t.Fatalf("最相关的应是 score 要点, 实际 %+v", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
