package media

import (
	"context"
	"errors"
)

// errStreamClosed 表示流已经关闭。
var errStreamClosed = errors.New("media: stream closed")

// AudioChunk 是一块音频数据。
type AudioChunk struct {
	PCM  []byte
	Seq  int
	AtMS int64
}

// ASRResult 是一次识别结果。
//
// Partial 结果只用于实时上屏(让候选人感到系统在听), Final 结果才参与评分。
// 拿 partial 去评分会导致"候选人还在补充, 系统已经打完分"的误判 ——
// 这是语音面试系统里代价最高的一类 bug, 因为它会直接改变录用结论。
type ASRResult struct {
	Text  string
	Final bool
	AtMS  int64
}

// ASRConfig 是识别配置。
type ASRConfig struct {
	Language   string
	SampleRate int
	// HotWords 是热词表。
	//
	// 面试场景下这一项对准确率影响极大: Goroutine、ZSet、Extism、
	// 幂等、分库分表这类词, 通用模型基本都会听错, 而它们恰恰是评分要点。
	HotWords []string
}

// ASRStream 是一次识别会话。
type ASRStream interface {
	// Push 送入一帧音频。返回非 nil 表示流已经结束。
	Push(chunk AudioChunk) error
	// Results 返回识别结果流, 在 Close 或 ctx 取消后关闭。
	Results() <-chan ASRResult
	// Close 结束本次识别并产出最终结果。
	Close() error
}

// ASRProvider 是可插拔的语音识别提供方。
// 换厂商只需要换一个实现, 上层的编排与打断逻辑完全不动。
type ASRProvider interface {
	Name() string
	Open(ctx context.Context, cfg ASRConfig) (ASRStream, error)
}

// Voice 描述音色。
type Voice struct {
	ID       string
	Language string
	Speed    float64
}

// TTSStream 是一次语音合成会话。
//
// 它是"流"而不是"一次性返回整段音频", 这一点是刻意的:
// 整段合成会让首字延迟等于整句的合成时间, 是语音面试体验崩掉
// 最常见、也最容易被忽略的原因。
type TTSStream interface {
	Chunks() <-chan AudioChunk
	Close() error
}

// TTSProvider 是可插拔的语音合成提供方。
type TTSProvider interface {
	Name() string
	Speak(ctx context.Context, text string, voice Voice) (TTSStream, error)
}
