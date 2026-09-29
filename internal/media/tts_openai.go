package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// OpenAITTS 通过 OpenAI 兼容的 /audio/speech 接口做流式合成。
//
// 这里最关键的一点是请求用 ctx 绑定。打断时 context 取消会直接中断
// 底层 HTTP 请求, 连接被关闭, 后面的音频不会再传回来 —— 这就是
// "级联取消必须传导到外部服务"的落地方式。
//
// 如果只在本地置一个 cancel 标志位, 连接会一直挂着把剩余音频传完,
// 几百路并发下连接池和带宽都会被无效流量打满。这类问题在压测时
// 才暴露, 但根因在写代码的第一天就埋下了。
type OpenAITTS struct {
	BaseURL    string // 默认 https://api.openai.com/v1
	APIKey     string
	Model      string // 默认 tts-1
	Format     string // pcm | mp3 | opus, 默认 pcm(免解码, 便于精确计算播放进度)
	ChunkBytes int    // 默认 640 字节(20ms)
	HTTPClient *http.Client
}

func (p *OpenAITTS) Name() string { return "openai-tts" }

func (p *OpenAITTS) Speak(ctx context.Context, text string, voice Voice) (TTSStream, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("openai-tts: 缺少 APIKey")
	}
	voiceID := voice.ID
	if voiceID == "" {
		voiceID = "alloy"
	}

	payload, err := json.Marshal(map[string]any{
		"model":           p.model(),
		"input":           text,
		"voice":           voiceID,
		"response_format": p.format(),
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL()+"/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("openai-tts: HTTP %d: %s", resp.StatusCode, truncateBytes(body, 200))
	}

	streamCtx, cancel := context.WithCancel(ctx)
	stream := &openaiTTSStream{
		ctx:        streamCtx,
		cancel:     cancel,
		body:       resp.Body,
		ch:         make(chan AudioChunk, 8),
		chunkBytes: p.chunkBytes(),
	}
	safeGo(streamCtx, "media.openai_tts", stream.run)
	return stream, nil
}

type openaiTTSStream struct {
	ctx        context.Context
	cancel     context.CancelFunc
	body       io.ReadCloser
	ch         chan AudioChunk
	chunkBytes int
	once       sync.Once
}

func (s *openaiTTSStream) Chunks() <-chan AudioChunk { return s.ch }

func (s *openaiTTSStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		// 关闭 response body 会立刻释放底层连接,
		// 而不是等传输自然结束。
		_ = s.body.Close()
	})
	return nil
}

func (s *openaiTTSStream) run() {
	defer close(s.ch)
	defer s.Close()

	buf := make([]byte, s.chunkBytes)
	seq := 0
	var atMS int64
	for {
		n, err := io.ReadFull(s.body, buf)
		if n > 0 {
			chunk := AudioChunk{PCM: append([]byte(nil), buf[:n]...), Seq: seq, AtMS: atMS}
			seq++
			atMS += PCMDurationMS(chunk.PCM)
			select {
			case s.ch <- chunk:
			case <-s.ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *OpenAITTS) chunkBytes() int {
	if p.ChunkBytes <= 0 {
		return FrameBytes
	}
	return p.ChunkBytes
}

func (p *OpenAITTS) model() string {
	if p.Model == "" {
		return "tts-1"
	}
	return p.Model
}

func (p *OpenAITTS) format() string {
	if p.Format == "" {
		return "pcm"
	}
	return p.Format
}

func (p *OpenAITTS) baseURL() string {
	if p.BaseURL == "" {
		return "https://api.openai.com/v1"
	}
	return strings.TrimRight(p.BaseURL, "/")
}

func (p *OpenAITTS) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}
