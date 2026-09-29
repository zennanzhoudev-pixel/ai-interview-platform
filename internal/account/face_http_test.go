package account

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport 让 HTTP 匹配器可以在**不绑定端口**的情况下被完整测到。
//
// 用 httptest.NewServer 更"真实", 但它需要一个可监听的端口 —— 而受限环境
// (CI 沙箱、无网络权限的开发机)不允许。把 http.Client 的 Transport 换掉,
// 请求仍然走完整的编码、鉴权头、状态码处理与响应解析, 只是不经过网络。
type fakeTransport struct {
	status  int
	body    string
	inspect func(*http.Request, []byte)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	if f.inspect != nil {
		f.inspect(req, raw)
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func testFaceImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 240, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 128, A: 255})
		}
	}
	return img
}

func TestHTTPMatcherSendsImageAndAuthHeader(t *testing.T) {
	var gotAuth string
	var gotImage string
	tr := &fakeTransport{
		body: `{"embedding":[3,4],"quality":0.9,"dim":2}`,
		inspect: func(req *http.Request, raw []byte) {
			gotAuth = req.Header.Get("Authorization")
			var payload struct {
				Image  string `json:"image"`
				Format string `json:"format"`
			}
			_ = json.Unmarshal(raw, &payload)
			gotImage = payload.Image
			if payload.Format != "jpeg" {
				t.Errorf("应声明 jpeg 格式, 实际 %q", payload.Format)
			}
			if req.URL.Path != "/embed" {
				t.Errorf("应请求 /embed, 实际 %s", req.URL.Path)
			}
		},
	}
	m := NewHTTPMatcher("https://face.example.com/", "secret-key", "arcface-r100")
	m.Client = &http.Client{Transport: tr}

	vec, quality, err := m.Embed(testFaceImage())
	if err != nil {
		t.Fatalf("远程特征提取失败: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("应带上 API Key, 实际 %q", gotAuth)
	}
	if _, err := base64.StdEncoding.DecodeString(gotImage); err != nil {
		t.Fatalf("图片应以 base64 传输: %v", err)
	}
	if quality != 0.9 {
		t.Fatalf("质量分应透传, 实际 %v", quality)
	}
	// 归一化: [3,4] -> [0.6,0.8], 这样比对才是纯余弦。
	if math.Abs(float64(vec[0])-0.6) > 1e-6 || math.Abs(float64(vec[1])-0.8) > 1e-6 {
		t.Fatalf("特征向量应被归一化, 实际 %v", vec)
	}
	// 名字与可信级别必须能看出"用的是哪个模型"。
	if m.Name() != "http:arcface-r100" || !strings.Contains(m.Assurance(), "arcface-r100") {
		t.Fatalf("应如实标注模型: %s / %s", m.Name(), m.Assurance())
	}
}

func TestHTTPMatcherDistinguishesAuthFailureFromServiceFailure(t *testing.T) {
	m := NewHTTPMatcher("https://face.example.com", "bad-key", "")
	m.Client = &http.Client{Transport: &fakeTransport{status: http.StatusUnauthorized, body: "unauthorized"}}
	_, _, err := m.Embed(testFaceImage())
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("401 应提示检查密钥, 实际 %v", err)
	}

	m2 := NewHTTPMatcher("https://face.example.com", "k", "")
	m2.Client = &http.Client{Transport: &fakeTransport{status: http.StatusInternalServerError, body: "boom"}}
	_, _, err = m2.Embed(testFaceImage())
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "API Key") {
		t.Fatalf("500 应作为服务故障上报(而不是密钥问题), 实际 %v", err)
	}
}

func TestHTTPMatcherRejectsBadResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"不是 JSON", "not-json", "无法解析"},
		{"空特征", `{"embedding":[]}`, "空特征向量"},
		{"维度不符", `{"embedding":[1,2,3],"dim":512}`, "维度不符"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewHTTPMatcher("https://face.example.com", "", "")
			m.Client = &http.Client{Transport: &fakeTransport{body: c.body}}
			if _, _, err := m.Embed(testFaceImage()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报 %q, 实际 %v", c.want, err)
			}
		})
	}
}

func TestHTTPMatcherRequiresURL(t *testing.T) {
	m := NewHTTPMatcher("", "", "")
	if _, _, err := m.Embed(testFaceImage()); err == nil {
		t.Fatal("未配置服务地址时应报错, 而不是静默成功")
	}
}

// TestHTTPMatcherIsDropInReplacement 保证它可以原样替换本地匹配器。
func TestHTTPMatcherIsDropInReplacement(t *testing.T) {
	var _ Matcher = NewHTTPMatcher("https://x", "", "")
	var _ Matcher = NewLocalMatcher()
	// 比对语义与本地一致: 余弦相似度、多个样本取最高分。
	m := NewHTTPMatcher("https://x", "", "")
	a := []float32{1, 0}
	b := []float32{1, 0}
	c := []float32{0, 1}
	if m.Compare(a, b) != 1 || m.Compare(a, c) != 0 {
		t.Fatalf("余弦比对不正确: %v %v", m.Compare(a, b), m.Compare(a, c))
	}
	if best := m.CompareBest([][]float32{c, b}, a); best != 1 {
		t.Fatalf("多样本应取最高分, 实际 %v", best)
	}
	if m.Compare(a, []float32{1}) != 0 {
		t.Fatal("维度不一致应返回 0, 而不是 panic 或错分")
	}
}

func TestServiceUsesConfiguredThreshold(t *testing.T) {
	s := New(Config{Store: NewMemoryStore(), TenantID: "t", Hasher: NewPasswordHasher(1000), FaceThreshold: 0.8})
	if s.Threshold() != 0.8 {
		t.Fatalf("阈值应可配置, 实际 %v", s.Threshold())
	}
	// 非法取值要退回默认值, 而不是让 0 变成"谁都能过"。
	s2 := New(Config{Store: NewMemoryStore(), TenantID: "t", Hasher: NewPasswordHasher(1000), FaceThreshold: 0})
	if s2.Threshold() != defaultFaceMatchThreshold {
		t.Fatalf("非法阈值应退回默认, 实际 %v", s2.Threshold())
	}
	if errors.Is(nil, ErrFaceMismatch) {
		t.Fatal("占位断言: 保证 errors 包被使用")
	}
}
