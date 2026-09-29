package media

import (
	"context"
	"sync"
	"time"
)

// ScriptedASR 是一个确定性的离线 ASR。
//
// 它存在的意义不是"凑数", 而是让整条实时链路可以在 CI 里被真实验证:
// partial 与 final 的时序、打断时的音频截断、端点判定与 ASR 的交互,
// 这些逻辑在没有真实 ASR、没有网络、没有密钥的情况下同样必须可回归。
type ScriptedASR struct {
	// NextText 返回下一轮"候选人说的话", 每次识别开始时调用一次。
	NextText func() string
	// FramesPerPartial 是每隔多少帧产出一个 partial 结果, 默认 10 帧(200ms)。
	FramesPerPartial int
	// RunesPerPartial 是每次 partial 追加的字符数, 默认 4 个。
	RunesPerPartial int
}

func (p *ScriptedASR) Name() string { return "scripted" }

func (p *ScriptedASR) Open(ctx context.Context, cfg ASRConfig) (ASRStream, error) {
	text := ""
	if p.NextText != nil {
		text = p.NextText()
	}
	per := p.FramesPerPartial
	if per <= 0 {
		per = 10
	}
	runes := p.RunesPerPartial
	if runes <= 0 {
		runes = 4
	}
	return &scriptedStream{
		ctx:              ctx,
		results:          make(chan ASRResult, 64),
		text:             []rune(text),
		framesPerPartial: per,
		runesPerPartial:  runes,
	}, nil
}

type scriptedStream struct {
	ctx              context.Context
	results          chan ASRResult
	text             []rune
	framesPerPartial int
	runesPerPartial  int

	mu        sync.Mutex
	frames    int
	sent      int
	elapsedMS int64
	closed    bool
	closeOnce sync.Once
}

func (s *scriptedStream) Push(chunk AudioChunk) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStreamClosed
	}
	s.frames++
	s.elapsedMS += PCMDurationMS(chunk.PCM)

	var out *ASRResult
	if s.frames%s.framesPerPartial == 0 && s.sent < len(s.text) {
		s.sent += s.runesPerPartial
		if s.sent > len(s.text) {
			s.sent = len(s.text)
		}
		out = &ASRResult{Text: string(s.text[:s.sent]), AtMS: s.elapsedMS}
	}
	s.mu.Unlock()

	if out != nil {
		s.emit(*out)
	}
	return nil
}

func (s *scriptedStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		full := string(s.text)
		elapsed := s.elapsedMS
		s.mu.Unlock()

		s.emit(ASRResult{Text: full, Final: true, AtMS: elapsed})
		close(s.results)
	})
	return nil
}

func (s *scriptedStream) Results() <-chan ASRResult { return s.results }

func (s *scriptedStream) emit(r ASRResult) {
	select {
	case s.results <- r:
	case <-s.ctx.Done():
	}
}

// LocalTTS 是离线语音合成: 产出静音 PCM, 但保持真实的时长与产出节奏。
//
// 它的用途是本地演示与测试。打断、级联取消、播放进度计算这些逻辑
// 与音频内容完全无关, 用静音就能完整验证, 而不需要任何密钥或网络。
type LocalTTS struct {
	// ChunkMS 是每块音频的时长(即合成产出粒度), 默认 100ms。
	ChunkMS int
	// Realtime 为 true 时按真实时间节奏产出, 便于在本地观察打断效果;
	// 为 false 时立即产出全部音频, 保证测试的确定性。
	Realtime bool
}

func (p *LocalTTS) Name() string { return "local" }

func (p *LocalTTS) Speak(ctx context.Context, text string, voice Voice) (TTSStream, error) {
	chunkMS := p.ChunkMS
	if chunkMS <= 0 {
		chunkMS = 100
	}
	ctx, cancel := context.WithCancel(ctx)
	stream := &localStream{
		ctx:      ctx,
		cancel:   cancel,
		totalMS:  EstimateSpeechMS(text),
		chunkMS:  chunkMS,
		realtime: p.Realtime,
		ch:       make(chan AudioChunk, 8),
	}
	safeGo(ctx, "media.local_tts", stream.run)
	return stream, nil
}

type localStream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	totalMS  int
	chunkMS  int
	realtime bool
	ch       chan AudioChunk
	once     sync.Once
}

func (s *localStream) run() {
	defer close(s.ch)
	seq := 0
	for elapsed := 0; elapsed < s.totalMS; elapsed += s.chunkMS {
		if s.ctx.Err() != nil {
			return
		}
		ms := s.chunkMS
		if elapsed+ms > s.totalMS {
			ms = s.totalMS - elapsed
		}
		select {
		case s.ch <- AudioChunk{PCM: SynthSilence(ms), Seq: seq, AtMS: int64(elapsed)}:
		case <-s.ctx.Done():
			return
		}
		seq++
		if !s.realtime {
			continue
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *localStream) Chunks() <-chan AudioChunk { return s.ch }

func (s *localStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

// EstimateSpeechMS 估算一段文本的播报时长。
// 中文按每秒约 5 个字估算, 这是面试官语速的常用参考值。
func EstimateSpeechMS(text string) int {
	ms := runeLen(text) * 200
	if ms < 300 {
		return 300
	}
	return ms
}
