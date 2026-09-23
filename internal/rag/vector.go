package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
)

// Embedder 把文本映射成向量。
//
// 这是整个检索里唯一"质量取决于外部能力"的一环。语义向量需要嵌入模型,
// 但检索管道(BM25、RRF、精排)与它解耦 —— 换一个 Embedder 实现即可。
type Embedder interface {
	Name() string
	Dim() int
	Embed(ctx context.Context, text string) ([]float32, error)
}

// HashingEmbedder 是本地确定性的"嵌入", 用特征哈希(feature hashing)。
//
// 老实说: 它不是语义模型, 衡量的是词面相似度, 召回质量接近 BM25。
// 它的价值在于让"向量 + RRF"这条链路在没有嵌入模型时也能真实跑起来、
// 可回归、可压测。要语义召回, 换 OpenAIEmbedder。
type HashingEmbedder struct {
	dim int
}

// NewHashingEmbedder 构造特征哈希嵌入器。dim 建议 256 或 512。
func NewHashingEmbedder(dim int) *HashingEmbedder {
	if dim <= 0 {
		dim = 256
	}
	return &HashingEmbedder{dim: dim}
}

func (e *HashingEmbedder) Name() string { return "hashing" }
func (e *HashingEmbedder) Dim() int     { return e.dim }

// Embed 把每个 token 哈希到向量的一维, 符号由第二个哈希决定,
// 最后做 L2 归一化。这是特征哈希的标准做法, 稀疏词面特征由此被压缩成稠密向量。
func (e *HashingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, e.dim)
	for _, tok := range Tokenize(text) {
		h1 := fnv1a(tok)
		h2 := fnv1a(tok + "#")
		idx := int(h1 % uint64(e.dim))
		if h2&1 == 1 {
			v[idx]--
		} else {
			v[idx]++
		}
	}
	normalize(v)
	return v, nil
}

// OpenAIEmbedder 调用 OpenAI 兼容的 /embeddings 接口。
type OpenAIEmbedder struct {
	BaseURL string
	APIKey  string
	Model   string
	client  *http.Client
}

// NewOpenAIEmbedder 构造嵌入器。model 建议 text-embedding-3-small。
func NewOpenAIEmbedder(baseURL, apiKey, model string) *OpenAIEmbedder {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &OpenAIEmbedder{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Model: model, client: &http.Client{}}
}

func (e *OpenAIEmbedder) Name() string { return "openai:" + e.Model }
func (e *OpenAIEmbedder) Dim() int     { return 1536 } // text-embedding-3-small

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embedding HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding 响应解析失败: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("embedding 响应为空")
	}
	return out.Data[0].Embedding, nil
}

// VectorIndex 是内存向量索引(暴力搜索)。
//
// 参考题库规模很小(几百篇), 暴力余弦搜索完全够用, 不需要 ANN 库。
// 这个决定是有意为之: 引入 HNSW 之类的库会大幅增加部署复杂度,
// 而题库规模根本用不上 —— 架构上保留 Embedder 接口, 需要时再换索引。
type VectorIndex struct {
	emb     Embedder
	ids     []string
	docs    []Doc
	vectors [][]float32
}

// NewVectorIndex 构造向量索引。
func NewVectorIndex(emb Embedder) *VectorIndex {
	return &VectorIndex{emb: emb}
}

// Add 为文档计算向量并入库。
func (v *VectorIndex) Add(ctx context.Context, doc Doc) error {
	vec, err := v.emb.Embed(ctx, doc.Text)
	if err != nil {
		return fmt.Errorf("计算向量失败(%s): %w", doc.ID, err)
	}
	v.ids = append(v.ids, doc.ID)
	v.docs = append(v.docs, doc)
	v.vectors = append(v.vectors, vec)
	return nil
}

// Search 按余弦相似度返回 top-k。
func (v *VectorIndex) Search(ctx context.Context, query string, k int) []Scored {
	if k <= 0 {
		k = 10
	}
	q, err := v.emb.Embed(ctx, query)
	if err != nil {
		return nil
	}

	type scored struct {
		idx   int
		score float64
	}
	var results []scored
	for i, vec := range v.vectors {
		s := cosine(q, vec)
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
		out = append(out, Scored{ID: v.ids[r.idx], Doc: v.docs[r.idx], Score: r.score})
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
}

func fnv1a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "..."
}
