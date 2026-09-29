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

// defaultFaceMatchThreshold 是 1:1 比对通过的余弦相似度默认阈值。
//
// 这个值来自实测(见 face_test.go 与开发时的对照测量), 实测结果是:
//
//	同一个人, 姿态不变, 仅噪声不同 : 1.00
//	同一个人, 整体平移 4 像素      : 0.92
//	不同的人(五官结构不同)         : 0.63 / 0.63 / 0.89
//
// 也就是说**分离间距很窄**: 最像的"别人"能到 0.89, 而本人轻微移位只有 0.92。
// 实测还包含一组更关键的对照(见 service_test.go 的 Posture 用例):
//
//	同一个人, 整体平移 12 像素 : 0.75
//	不同的人(最像的一对)      : 0.91
//
// **这两者是重叠的** —— 本人换个姿势比最像的"别人"还低。也就是说
// 没有任何阈值能同时做到"本人都能过"与"别人都过不了"。因此这里把阈值
// 定在安全侧(0.95): 宁可让本人多试一次密码, 也不要把别人放进来。
// 姿态鲁棒性只能靠人脸模型解决, 不是靠调参 —— 这就是 LocalMatcher
// 只敢声明 assurance=development-only 的原因。
const defaultFaceMatchThreshold = 0.95

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
	Template []float32 `json:"-"`
	// Dim 是单个样本的维度; Template 里连着放 Samples 个样本。
	Dim int `json:"dim"`
	// Samples 是注册时保存的样本数(多帧 + 平均)。
	// 多样本是为了覆盖"不同光线/距离/姿态"下的同一张脸 —— 只存一张平均模板时,
	// 跨越会话的差异会把分数压到阈值以下, 表现就是"录入了却登不上"。
	Samples    int       `json:"samples"`
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
	// CompareBest 把候选模板与"多个已录入样本"逐一比较, 返回最高分。
	//
	// 为什么需要多样本: 同一个人在两次登录之间, 光线、距离、头部角度
	// 都会变。只存一张平均模板时, 这些变化会把分数压到阈值以下 ——
	// 表现就是"我明明录入了却登不进去"。真实的人脸系统普遍采用
	// 多样本注册(注册时多存几张), 就是为了换这一点鲁棒性。
	CompareBest(samples [][]float32, candidate []float32) float64
	// CompareOne 比较"一个注册样本"与候选模板。
	// 本地实现会带上平移容忍; 换成厂商 SDK 时直接实现为普通比对即可。
	CompareOne(sample, candidate []float32) float64
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

// CompareOne 实现 Matcher 接口。
//
// 这里**故意不做平移容忍**。曾经加过 ±2 格的平移容忍, 结果是:
// 同一个人的姿势变化确实能过了, 但"别人"的分数也从 0.63-0.89 涨到了
// 0.92 —— 直接越过阈值。原因是它在 25 种对齐里取最大值, 对任何一对
// 画面都会虚高(典型的多重比较效应)。
//
// 结论: 用一个更宽松的相似度去换姿势鲁棒性是错的 —— 它同时放宽了
// 安全性。姿势差异应该由"多样本注册"来覆盖(注册时多存几张),
// 而不是靠匹配时多试几次。这个取舍值得写在这里, 免得以后有人再试一遍。
func (m *LocalMatcher) CompareOne(sample, candidate []float32) float64 {
	return m.Compare(sample, candidate)
}

// CompareBest 在多个注册样本中取最高分。
func (m *LocalMatcher) CompareBest(samples [][]float32, candidate []float32) float64 {
	best := 0.0
	for _, s := range samples {
		if score := m.CompareOne(s, candidate); score > best {
			best = score
		}
	}
	return best
}

// UnpackSamples 把存储形态的模板拆成多个样本。
//
// 存储形态是一段连续 float32: 共 Samples 段, 每段 Dim 维。用一个 blob
// 而不是多行, 是因为"注册样本"永远是一起读写的, 拆表只会多出一堆 JOIN。
func UnpackSamples(flat []float32, dim, samples int) [][]float32 {
	if dim <= 0 || samples <= 0 || len(flat) < dim*samples {
		if len(flat) == 0 {
			return nil
		}
		// 兜底: 当成单个样本, 避免历史数据(只存过平均模板)读不出来。
		return [][]float32{flat}
	}
	out := make([][]float32, 0, samples)
	for i := 0; i < samples; i++ {
		part := make([]float32, dim)
		copy(part, flat[i*dim:(i+1)*dim])
		out = append(out, part)
	}
	return out
}

// PackSamples 把多个样本压成存储形态。
func PackSamples(samples [][]float32) (flat []float32, dim, count int) {
	if len(samples) == 0 {
		return nil, 0, 0
	}
	dim = len(samples[0])
	if dim == 0 {
		return nil, 0, 0
	}
	flat = make([]float32, 0, dim*len(samples))
	for _, s := range samples {
		if len(s) != dim {
			continue
		}
		flat = append(flat, s...)
		count++
	}
	return flat, dim, count
}

// maxShift 是匹配时允许的网格平移量(格)。
//
// 48 格的网格覆盖整张画面, 因此 2 格大约相当于画面宽度的 4% ——
// 足以覆盖"这次坐得离摄像头近一点/偏一点", 又不足以把别人匹配进来。
// 平移容忍的意义在于: 它消掉的是**构图差异**, 而不是身份差异。
const maxShift = 2

// CompareShiftTolerant 允许候选模板在网格上平移若干格后取最高分。
//
// 为什么需要: 本地匹配器衡量的是"画面看起来像不像", 而人的坐姿与
// 距离每次都不一样。不做平移容忍时, 同一个人的两次画面可能因为
// 整体偏移而掉到阈值以下 —— 这是"录入了却登不上"的另一个来源。
// 平移容忍只解决构图差异, 不改变"是不是同一个人"的判断依据。
func (m *LocalMatcher) CompareShiftTolerant(sample, candidate []float32) float64 {
	if len(sample) != len(candidate) || len(sample) == 0 {
		return 0
	}
	side := faceEmbeddingSide
	if side*side != len(sample) {
		// 维度不是网格的平方(例如测试里的小向量): 退回普通比较。
		return m.Compare(sample, candidate)
	}
	best := 0.0
	for dy := -maxShift; dy <= maxShift; dy++ {
		for dx := -maxShift; dx <= maxShift; dx++ {
			score := shiftedCosine(sample, candidate, side, dx, dy)
			if score > best {
				best = score
			}
		}
	}
	return best
}

// shiftedCosine 计算 sample 与"平移后的 candidate"的余弦相似度。
// 两个向量都已 L2 归一化, 因此只算点积; 越界的部分按 0 处理(相当于补零),
// 并重新归一化重叠区域的能量, 避免"平移越多分数越低"变成人为惩罚。
func shiftedCosine(sample, candidate []float32, side, dx, dy int) float64 {
	var dot, norm float64
	for y := 0; y < side; y++ {
		sy := y + dy
		if sy < 0 || sy >= side {
			continue
		}
		for x := 0; x < side; x++ {
			sx := x + dx
			if sx < 0 || sx >= side {
				continue
			}
			c := candidate[sy*side+sx]
			dot += float64(sample[y*side+x]) * float64(c)
			norm += float64(c) * float64(c)
		}
	}
	if norm == 0 {
		return 0
	}
	// sample 已是单位向量, 因此除以重叠部分能量的平方根即可。
	score := dot / math.Sqrt(norm)
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
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
