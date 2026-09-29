package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// HTTPMatcher 把特征提取交给一个真实的人脸模型服务。
//
// 为什么默认不这么做、却又必须留这个口子:
//
// 苹果的 Face ID 是"红外点阵投影 + 深度相机 + 训练好的神经网络 + 安全隔区"
// 的组合。浏览器里的普通摄像头拿不到深度信息, 而我们也没有可用的模型 ——
// 因此**本地匹配器永远不可能与其持平**, 这是硬件与模型的差距, 不是调参能补的。
//
// 真正能接近的做法只有一条: 把特征提取换成一个人脸模型(自建的 ArcFace /
// InsightFace 服务, 或云厂商的人脸 API)。本类型就是那条路 ——
// 它对上仍然只是一个 Matcher, 因此注册、登录、审计、权限这些逻辑一行都不改。
//
// 约定极简, 便于对接任意实现(自建服务写几十行即可):
//
//	POST {BaseURL}/embed
//	Header: Authorization: Bearer {APIKey}   (APIKey 为空时不发)
//	Body:   {"image": "<base64>", "format": "jpeg"}
//	200:    {"embedding": [0.12, -0.03, ...], "quality": 0.92, "dim": 512}
//
// 只要求"给一张图, 还一个特征向量": 比对留在本地。这样既避免把两张人脸
// 照片都上传(隐私), 也避免每次比对都产生一次网络往返(延迟)。
type HTTPMatcher struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
	// Timeout 是单次请求的上限。人脸提取偶尔会慢, 但登录不能无限等。
	Timeout time.Duration
}

// NewHTTPMatcher 构造远程匹配器。
func NewHTTPMatcher(baseURL, apiKey, model string) *HTTPMatcher {
	return &HTTPMatcher{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Model:   strings.TrimSpace(model),
		Client:  &http.Client{},
		Timeout: 8 * time.Second,
	}
}

// Name 返回匹配器名(用于日志与界面: 必须能看出"这次用的是哪个匹配器")。
func (m *HTTPMatcher) Name() string {
	if m.Model != "" {
		return "http:" + m.Model
	}
	return "http"
}

// Assurance 说明可信级别。
//
// 它取决于后端接的是什么模型, 因此在没有配置模型名时只敢声明 standard;
// 若接入的是认证级人脸服务, 把 Model 填成对应名称即可 —— 这个字段会被
// 原样透出到接口与界面, 让使用者知道自己依赖的是什么。
func (m *HTTPMatcher) Assurance() string {
	if m.Model != "" {
		return "standard:" + m.Model
	}
	return "standard"
}

// Embed 调用远程服务提取特征。
func (m *HTTPMatcher) Embed(img image.Image) ([]float32, float64, error) {
	if m.BaseURL == "" {
		return nil, 0, errors.New("account: 未配置人脸模型服务地址")
	}
	raw, err := EncodeJPEG(img, 90)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(map[string]any{
		"image":  base64.StdEncoding.EncodeToString(raw),
		"format": "jpeg",
	})
	if err != nil {
		return nil, 0, err
	}

	client := m.Client
	if client == nil {
		client = &http.Client{}
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("account: 人脸模型服务不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode/100 != 2 {
		// 401/403 与其它错误必须区分开: 前者是配置问题(密钥错了),
		// 后者可能是模型服务本身故障 —— 两者的处置完全不同。
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, 0, fmt.Errorf("account: 人脸模型服务拒绝访问(HTTP %d), 请检查 API Key", resp.StatusCode)
		default:
			return nil, 0, fmt.Errorf("account: 人脸模型服务返回 HTTP %d: %s",
				resp.StatusCode, truncateText(string(payload), 200))
		}
	}

	var out struct {
		Embedding []float32 `json:"embedding"`
		Quality   float64   `json:"quality"`
		Dim       int       `json:"dim"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, 0, fmt.Errorf("account: 无法解析人脸模型响应: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, 0, errors.New("account: 人脸模型返回了空特征向量")
	}
	if out.Dim > 0 && out.Dim != len(out.Embedding) {
		return nil, 0, fmt.Errorf("account: 人脸模型维度不符(声明 %d, 实际 %d)", out.Dim, len(out.Embedding))
	}
	quality := out.Quality
	if quality <= 0 {
		// 服务没有返回质量分时按"未评估"处理, 不因此拒绝 —— 质量门禁
		// 由模型服务自己把关更可靠(它看得见原始画面)。
		quality = 1
	}
	// 归一化: 后续的比对依赖单位向量。
	normalizeFloat32(out.Embedding)
	return out.Embedding, quality, nil
}

// Compare 用余弦相似度比对(与本地实现一致)。
func (m *HTTPMatcher) Compare(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	if dot < 0 {
		return 0
	}
	if dot > 1 {
		return 1
	}
	return dot
}

// CompareOne 与 Compare 相同: 真实模型不需要靠"多试几次对齐"来找补。
func (m *HTTPMatcher) CompareOne(sample, candidate []float32) float64 {
	return m.Compare(sample, candidate)
}

// CompareBest 在多个注册样本中取最高分。
func (m *HTTPMatcher) CompareBest(samples [][]float32, candidate []float32) float64 {
	best := 0.0
	for _, s := range samples {
		if score := m.Compare(s, candidate); score > best {
			best = score
		}
	}
	return best
}

// normalizeFloat32 把向量归一化为单位长度, 让后续的余弦相似度退化成点积。
func normalizeFloat32(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	norm := math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
}

func truncateText(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "..."
}
