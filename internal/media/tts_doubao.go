package media

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// DoubaoTTS 通过火山引擎语音技术的 /api/v3/tts/unidirectional 做流式合成。
//
// 与 OpenAI 兼容的 /audio/speech 不同: 火山语音走 openspeech.bytedance.com,
// 鉴权是 X-Api-Key(语音技术控制台的 API Key, 不是方舟的 ark- key),
// 响应是 NDJSON, 每行一段 base64 音频。
type DoubaoTTS struct {
	APIKey     string
	ResourceID string // 默认 "seed-tts-2.0"(豆包语音合成 2.0)
	Voice      string // 音色 speaker, 默认 "zh_female_vv_uranus_bigtts"
	BaseURL    string // 默认 https://openspeech.bytedance.com/api/v3/tts/unidirectional
	HTTPClient *http.Client
}

const (
	doubaoTTSDefaultURL        = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
	doubaoTTSDefaultResourceID = "seed-tts-2.0"
	doubaoTTSDefaultVoice      = "zh_female_vv_uranus_bigtts"
)

// Name 返回提供方标识。
func (p *DoubaoTTS) Name() string { return "doubao-tts" }

func (p *DoubaoTTS) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return doubaoTTSDefaultURL
}

func (p *DoubaoTTS) resourceID() string {
	if p.ResourceID != "" {
		return p.ResourceID
	}
	return doubaoTTSDefaultResourceID
}

func (p *DoubaoTTS) voice() string {
	if p.Voice != "" {
		return p.Voice
	}
	return doubaoTTSDefaultVoice
}

func (p *DoubaoTTS) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// Speak 合成一段语音, 以 PCM16LE@16kHz 流式返回。
func (p *DoubaoTTS) Speak(ctx context.Context, text string, voice Voice) (TTSStream, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("doubao-tts: 缺少 APIKey")
	}
	voiceID := voice.ID
	if voiceID == "" {
		voiceID = p.voice()
	}

	payload := map[string]any{
		"user": map[string]any{"uid": "ai-interview-platform"},
		"req_params": map[string]any{
			"text":    text,
			"speaker": voiceID,
			"audio_params": map[string]any{
				"format":      "pcm",
				"sample_rate": SampleRate,
				"speech_rate": 0,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", p.APIKey)
	req.Header.Set("X-Api-Resource-Id", p.resourceID())
	req.Header.Set("X-Api-Request-Id", uuid.NewString())

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("doubao-tts: HTTP %d: %s", resp.StatusCode, truncateBytes(b, 300))
	}

	streamCtx, cancel := context.WithCancel(ctx)
	s := &doubaoTTSStream{ctx: streamCtx, cancel: cancel, body: resp.Body, ch: make(chan AudioChunk, 8)}
	safeGo(streamCtx, "media.doubao_tts", s.run)
	return s, nil
}

type doubaoTTSStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	ch     chan AudioChunk
	once   sync.Once
}

func (s *doubaoTTSStream) Chunks() <-chan AudioChunk { return s.ch }

func (s *doubaoTTSStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.body.Close()
	})
	return nil
}

func (s *doubaoTTSStream) run() {
	defer close(s.ch)
	defer s.Close()

	scanner := bufio.NewScanner(s.body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	seq := 0
	var atMS int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if frame.Code != 0 {
			// 0 是成功; 20000000 之类是可选终止帧, 跳过即可。
			if frame.Code >= 20000000 {
				return
			}
			return
		}
		pcm, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil || len(pcm) == 0 {
			continue
		}
		chunk := AudioChunk{PCM: pcm, Seq: seq, AtMS: atMS}
		seq++
		atMS += PCMDurationMS(pcm)
		select {
		case s.ch <- chunk:
		case <-s.ctx.Done():
			return
		}
	}
}
