package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAIASR 通过 OpenAI 兼容的 /audio/transcriptions 接口做识别。
//
// 这是"分段增量流式"而不是"真流式": 音频按 SegmentMS 切片, 每片独立转写,
// 结果拼接后作为 partial 上屏。代价是 partial 有 SegmentMS 粒度的延迟;
// 收益是可以直接走标准 HTTP 接口 —— 不需要维护 WebSocket 音频分片协议,
// 也不会被单一厂商的长连接实现绑死。
//
// 要压到 200ms 以内的真流式, 换成 Realtime API 或厂商 SDK 即可,
// ASRProvider 接口不用改。这正是把 provider 抽象出来的意义:
// 延迟预算是可以按成本逐步优化的, 但架构不该为某一个厂商的协议定型。
type OpenAIASR struct {
	BaseURL        string // 默认 https://api.openai.com/v1
	APIKey         string
	Model          string // 默认 whisper-1
	SegmentMS      int    // 默认 2000
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

func (p *OpenAIASR) Name() string { return "openai-asr" }

func (p *OpenAIASR) Open(ctx context.Context, cfg ASRConfig) (ASRStream, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("openai-asr: 缺少 APIKey")
	}
	ctx, cancel := context.WithCancel(ctx)
	stream := &openaiASRStream{
		provider:  p,
		cfg:       cfg,
		ctx:       ctx,
		cancel:    cancel,
		results:   make(chan ASRResult, 32),
		audio:     make(chan []byte, 64),
		segmentMS: p.segmentMS(),
		closed:    make(chan struct{}),
	}
	go stream.loop()
	return stream, nil
}

type openaiASRStream struct {
	provider  *OpenAIASR
	cfg       ASRConfig
	ctx       context.Context
	cancel    context.CancelFunc
	results   chan ASRResult
	audio     chan []byte
	segmentMS int
	closed    chan struct{}

	mu        sync.Mutex
	elapsedMS int64
	confirmed []string
	closeOnce sync.Once
}

func (s *openaiASRStream) Push(chunk AudioChunk) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-s.closed:
		return errStreamClosed
	default:
	}
	s.mu.Lock()
	s.elapsedMS += PCMDurationMS(chunk.PCM)
	s.mu.Unlock()

	select {
	case s.audio <- chunk.PCM:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-s.closed:
		return errStreamClosed
	}
}

func (s *openaiASRStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *openaiASRStream) Results() <-chan ASRResult { return s.results }

func (s *openaiASRStream) loop() {
	defer close(s.results)
	var buf []byte
	var bufMS int64

	flush := func() {
		if len(buf) == 0 {
			return
		}
		segment := buf
		buf = nil
		bufMS = 0

		text, err := s.provider.transcribe(s.ctx, segment, s.cfg)
		if err != nil {
			// 单段转写失败不终止整条链路: 已确认的部分照常上屏,
			// 让上层能拿到"部分回答"而不是彻底没有输入。
			s.emit(ASRResult{Text: s.joined(), AtMS: s.elapsed()})
			return
		}
		if t := strings.TrimSpace(text); t != "" {
			s.mu.Lock()
			s.confirmed = append(s.confirmed, t)
			s.mu.Unlock()
		}
		s.emit(ASRResult{Text: s.joined(), AtMS: s.elapsed()})
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.closed:
			for {
				select {
				case pcm := <-s.audio:
					buf = append(buf, pcm...)
					bufMS += PCMDurationMS(pcm)
					continue
				default:
				}
				break
			}
			flush()
			s.emit(ASRResult{Text: s.joined(), Final: true, AtMS: s.elapsed()})
			return
		case pcm := <-s.audio:
			buf = append(buf, pcm...)
			bufMS += PCMDurationMS(pcm)
			if bufMS >= int64(s.segmentMS) {
				flush()
			}
		}
	}
}

func (s *openaiASRStream) emit(r ASRResult) {
	select {
	case s.results <- r:
	case <-s.ctx.Done():
	}
}

func (s *openaiASRStream) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.confirmed, "")
}

func (s *openaiASRStream) elapsed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.elapsedMS
}

func (p *OpenAIASR) segmentMS() int {
	if p.SegmentMS <= 0 {
		return 2000
	}
	return p.SegmentMS
}

func (p *OpenAIASR) model() string {
	if p.Model == "" {
		return "whisper-1"
	}
	return p.Model
}

func (p *OpenAIASR) baseURL() string {
	if p.BaseURL == "" {
		return "https://api.openai.com/v1"
	}
	return strings.TrimRight(p.BaseURL, "/")
}

func (p *OpenAIASR) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

func (p *OpenAIASR) transcribe(ctx context.Context, pcm []byte, cfg ASRConfig) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	// 音频链路上传的是裸 PCM, 而转写接口要求带容器头,
	// 所以在这里补一个 44 字节 WAV 头, 而不是让前端去拼。
	fw, err := mw.CreateFormFile("file", "segment.wav")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(wavHeader(len(pcm))); err != nil {
		return "", err
	}
	if _, err := fw.Write(pcm); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", p.model())
	if cfg.Language != "" {
		_ = mw.WriteField("language", cfg.Language)
	}
	if len(cfg.HotWords) > 0 {
		// 热词通过 prompt 注入, 这是兼容接口上最通用的做法。
		_ = mw.WriteField("prompt", strings.Join(cfg.HotWords, "、"))
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	reqCtx := ctx
	if p.RequestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, p.RequestTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		p.baseURL()+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("openai-asr: HTTP %d: %s", resp.StatusCode, truncateBytes(payload, 200))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("openai-asr: 解析响应失败: %w", err)
	}
	return out.Text, nil
}

// wavHeader 生成 44 字节的 WAV 头(单声道 16bit PCM)。
func wavHeader(pcmBytes int) []byte {
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+pcmBytes))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], SampleRate)
	binary.LittleEndian.PutUint32(h[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(pcmBytes))
	return h
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
