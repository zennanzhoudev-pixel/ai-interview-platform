package account

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // 注册 png 解码器: 浏览器可能给出 png 帧
	"math"
	"strings"
	"time"
)

// 人脸登录的完整链路在这里, 但**识别本身是可插拔的**, 这一点是这个文件
// 最重要的设计决定, 必须说清楚:
//
//	浏览器采集 -> 服务端解码 -> 质量校验 -> 生成模板 -> 存库
//	登录时: 同样采集 -> 与已存模板做 1:1 比对 -> 通过则签发登录会话
//
// 默认的 LocalMatcher 做的是"图像相似度", **不是人脸识别**: 它把画面
// 降采样成灰度向量再做余弦相似度, 因此:
//   - 同一个人换姿势/换光线会明显掉分;
//   - 一张本人照片可以骗过它;
//   - 它没有任何活体检测能力。
//
// 之所以还这么实现, 是因为生产级人脸能力来自第三方(云厂商 API 或
// 本地 SDK), 而把"管线"做真、把"算法"留成接口, 是唯一诚实的做法:
// 接入方换一个 Matcher 就能拿到真正的识别能力, 上层(注册、登录、
// 审计、权限)一行都不用改。
//
// 因此接口与界面都会明确标注 assurance=development-only,
// 并且**密码始终是主凭证** —— 人脸只是可选的便捷登录方式。

// faceMatchThreshold 是 1:1 比对通过的余弦相似度阈值。
//
// 这个值来自实测(见 face_test.go 与开发时的对照测量), 实测结果是:
//
//	同一个人, 姿态不变, 仅噪声不同 : 1.00
//	同一个人, 整体平移 4 像素      : 0.92
//	不同的人(五官结构不同)         : 0.63 / 0.63 / 0.89
//
// 也就是说**分离间距很窄**: 最像的"别人"能到 0.89, 而本人轻微移位只有 0.92。
// 取 0.92 是在"别把人认成别人"(安全) 与 "本人一次就能过"(可用) 之间的折中,
// 但 0.03 的余量显然不能算可靠 —— 这正是 LocalMatcher 只敢声明
// assurance=development-only 的原因, 也是生产必须换真 SDK 的原因。
const faceMatchThreshold = 0.92

// faceEnrollStability 要求注册时的多帧之间也足够相似。
//
// 为什么注册要求多帧: 单帧注册会把"一次抖动/一次遮挡"固化成模板,
// 之后本人都很难通过。多帧一致才能说明"画面稳定地是同一张脸"。
const faceEnrollStability = 0.85

const (
	// faceEmbeddingSide 与采样方式一起决定了"模板保留多少细节"。
	//
	// 一开始用的是 32×32 且每个目标像素取源图整块平均 —— 结果是高频信息
	// 被平均掉了, **不同的人也能拿到 0.99 的相似度**(测试里真实出现过)。
	// 这暴露了本地匹配器的本质: 它不是人脸识别, 而是图像相似度。
	// 既然要做到"至少不能把明显不同的人算成同一个人", 就必须保留细节:
	// 48×48 网格 + 每点只读 2×2 源像素(点采样而非块平均)。
	faceEmbeddingSide = 48
	// faceSampleBlock 是每个采样点覆盖的源像素边长。
	// 1 会让模板对 1 像素位移过于敏感(本人轻微移动就掉分);
	// 过大则又退化成块平均(丢细节)。2 是这两者之间的折中。
	faceSampleBlock   = 2
	minFaceSidePixels = 160
)

// FaceProfile 是一个账号的人脸模板。
type FaceProfile struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	// Template 是归一化后的特征向量, 不是原始照片 ——
	// 存原始照片等于多存一份生物特征原始数据, 风险与收益完全不成比例。
	Template   []float32 `json:"-"`
	Dim        int       `json:"dim"`
	Matcher    string    `json:"matcher"`
	Assurance  string    `json:"assurance"`
	Quality    float64   `json:"quality"`
	Frames     int       `json:"frames"`
	EnrolledAt time.Time `json:"enrolled_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Matcher 是人脸模板的生成与比对能力。
//
// 换厂商(云 API / 本地 SDK)只需要实现这个接口: 上层不需要知道
// "特征从哪来", 只需要知道"给两张图, 它们像不像"。
type Matcher interface {
	Name() string
	// Assurance 说明可信级别: development-only / standard / certified。
	// 它会原样出现在接口响应与界面上, 让使用者知道自己在依赖什么。
	Assurance() string
	// Embed 生成特征向量, 并返回质量分(0..1)。
	Embed(img image.Image) ([]float32, float64, error)
	// Compare 返回两个模板的相似度(0..1)。
	Compare(a, b []float32) float64
}

// ErrUnsupportedImage 表示图片格式不受支持。
var ErrUnsupportedImage = errors.New("account: 图片格式不受支持")

// DecodeDataURL 解析浏览器传来的 `data:image/...;base64,...`。
//
// 浏览器侧统一用 canvas 的 toDataURL('image/jpeg', 0.8): JPEG 体积小, 且标准库自带解码器, 不必为这点差异引入图片库。
func DecodeDataURL(raw string) (image.Image, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: 图片为空", ErrFaceLowQuality)
	}
	if idx := strings.Index(raw, ","); strings.HasPrefix(raw, "data:") && idx > 0 {
		meta, payload := raw[:idx], raw[idx+1:]
		if !strings.Contains(meta, "base64") {
			return nil, fmt.Errorf("%w: 只支持 base64 编码的图片", ErrUnsupportedImage)
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			// 部分客户端会给出 URL 安全的 base64。
			decoded, err = base64.RawURLEncoding.DecodeString(payload)
			if err != nil {
				return nil, fmt.Errorf("%w: base64 解码失败", ErrUnsupportedImage)
			}
		}
		return decodeImage(decoded)
	}
	// 也接受裸 base64(方便用 curl 联调)。
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return decodeImage(decoded)
	}
	return nil, fmt.Errorf("%w: 需要 data URL 或 base64", ErrUnsupportedImage)
}

func decodeImage(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	return img, nil
}

// EncodeJPEG 把图片重新编码为 JPEG(自检与测试使用)。
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if quality <= 0 || quality > 100 {
		quality = 80
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// LocalMatcher 是默认的本地匹配器。
//
// 它做三件事: 灰度化 -> 32x32 降采样 -> 局部对比度归一化 + L2 归一化,
// 然后用余弦相似度比对。它衡量的是"画面看起来像不像",
// 不是"是不是同一个人" —— 这一点它自己也如实声明(Assurance)。
type LocalMatcher struct{}

// NewLocalMatcher 构造本地匹配器。
func NewLocalMatcher() *LocalMatcher { return &LocalMatcher{} }

// Name 返回匹配器名。
func (m *LocalMatcher) Name() string { return "local-image-similarity" }

// Assurance 如实声明可信级别。
func (m *LocalMatcher) Assurance() string { return "development-only" }

// Embed 生成特征向量与质量分。
func (m *LocalMatcher) Embed(img image.Image) ([]float32, float64, error) {
	bounds := img.Bounds()
	if bounds.Dx() < minFaceSidePixels || bounds.Dy() < minFaceSidePixels {
		return nil, 0, fmt.Errorf("%w: 分辨率过低(至少 %dx%d, 实际 %dx%d)",
			ErrFaceLowQuality, minFaceSidePixels, minFaceSidePixels, bounds.Dx(), bounds.Dy())
	}

	// 采样: 每个目标像素在源图对应位置读一小块(faceSampleBlock×faceSampleBlock)
	// 并取均值。用"点采样 + 小块平均"而不是"整块平均", 是为了保住细节 ——
	// 否则不同的人也会因为细节被抹平而算出极高的相似度。
	gray := make([]float64, faceEmbeddingSide*faceEmbeddingSide)
	stepX := float64(bounds.Dx()) / faceEmbeddingSide
	stepY := float64(bounds.Dy()) / faceEmbeddingSide
	for gy := 0; gy < faceEmbeddingSide; gy++ {
		for gx := 0; gx < faceEmbeddingSide; gx++ {
			x0 := bounds.Min.X + int(float64(gx)*stepX)
			y0 := bounds.Min.Y + int(float64(gy)*stepY)
			x1 := x0 + faceSampleBlock
			y1 := y0 + faceSampleBlock
			if x1 > bounds.Max.X {
				x1 = bounds.Max.X
			}
			if y1 > bounds.Max.Y {
				y1 = bounds.Max.Y
			}
			var sum, count float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, _ := img.At(x, y).RGBA()
					// 亮度用 Rec.601 权重: 对肤色差异的保真度足够,
					// 又不需要引入色彩空间转换。
					sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
					count++
				}
			}
			if count > 0 {
				gray[gy*faceEmbeddingSide+gx] = sum / count
			}
		}
	}

	// 质量: 亮度与对比度都要落在可用区间内。
	var mean float64
	for _, v := range gray {
		mean += v
	}
	mean /= float64(len(gray))
	var variance float64
	for _, v := range gray {
		d := v - mean
		variance += d * d
	}
	stddev := math.Sqrt(variance / float64(len(gray)))
	quality := 1.0
	if mean < 25 || mean > 235 {
		return nil, 0, fmt.Errorf("%w: 画面过暗或过曝(平均亮度 %.0f)", ErrFaceLowQuality, mean)
	}
	if stddev < 8 {
		// 几乎是纯色: 大概率遮住了摄像头, 或者对着一面白墙。
		return nil, 0, fmt.Errorf("%w: 画面缺少细节(标准差 %.1f)", ErrFaceLowQuality, stddev)
	}
	if stddev < 20 {
		quality = 0.6
	}

	// 局部对比度归一化: 减去均值、除以标准差, 让模板对整体明暗不敏感。
	vec := make([]float32, len(gray))
	var norm float64
	for i, v := range gray {
		n := (v - mean) / stddev
		vec[i] = float32(n)
		norm += n * n
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil, 0, fmt.Errorf("%w: 特征退化", ErrFaceLowQuality)
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec, quality, nil
}

// Compare 用余弦相似度比对两个模板。
func (m *LocalMatcher) Compare(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	// 两个向量都已 L2 归一化, 因此点积即余弦相似度。
	if dot < 0 {
		return 0
	}
	if dot > 1 {
		return 1
	}
	return dot
}

// AverageTemplates 把多帧模板平均成一个(再归一化)。
func AverageTemplates(vecs [][]float32) ([]float32, error) {
	if len(vecs) == 0 {
		return nil, errors.New("account: 没有可用的特征帧")
	}
	dim := len(vecs[0])
	out := make([]float32, dim)
	for _, v := range vecs {
		if len(v) != dim {
			return nil, errors.New("account: 特征维度不一致")
		}
		for i := range v {
			out[i] += v[i]
		}
	}
	var norm float64
	for i := range out {
		out[i] /= float32(len(vecs))
		norm += float64(out[i]) * float64(out[i])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil, errors.New("account: 平均后特征退化")
	}
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out, nil
}
