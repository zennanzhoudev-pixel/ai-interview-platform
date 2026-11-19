package media

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// DoubaoASR 通过火山引擎语音技术的流式 WebSocket 做识别。
//
// 协议是二进制分帧: 4 字节头 + 4 字节大端 payload 长度 + gzip(payload)。
// 先发一个 full client request(JSON 配置), 再持续发 PCM 音频帧,
// 结束时发一个带 LAST 标记的音频帧, 服务端回传 final 结果。
type DoubaoASR struct {
	APIKey     string
	ResourceID string // 默认 "volc.seedasr.sauc.duration"(Seed ASR 流式)
	BaseURL    string // 默认 wss://openspeech.bytedance.com/api/v3/sauc/bigmodel
	Dialer     *websocket.Dialer
}

const (
	doubaoASRDefaultURL        = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel"
	doubaoASRDefaultResourceID = "volc.seedasr.sauc.duration"
)

// 帧头字段(见火山语音 V3 协议)。
const (
	asrProtoVer = 0b0001
	asrHdrSize  = 0b0001
	asrMFullReq = 0b0001
	asrMAudio   = 0b0010
	asrMError   = 0b1111
	asrFLast    = 0b0010
	asrSJSON    = 0b0001
	asrCGzip    = 0b0001
)

// Name 返回提供方标识。
func (p *DoubaoASR) Name() string { return "doubao-asr" }

func (p *DoubaoASR) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return doubaoASRDefaultURL
}

func (p *DoubaoASR) resourceID() string {
	if p.ResourceID != "" {
		return p.ResourceID
	}
	return doubaoASRDefaultResourceID
}

func (p *DoubaoASR) dialer() *websocket.Dialer {
	if p.Dialer != nil {
		return p.Dialer
	}
	return websocket.DefaultDialer
}

// Open 建立识别会话。
func (p *DoubaoASR) Open(ctx context.Context, cfg ASRConfig) (ASRStream, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("doubao-asr: 缺少 APIKey")
	}
	rate := cfg.SampleRate
	if rate <= 0 {
		rate = SampleRate
	}

	cid := uuid.NewString()
	header := http.Header{}
	header.Set("X-Api-Key", p.APIKey)
	header.Set("X-Api-Resource-Id", p.resourceID())
	header.Set("X-Api-Connect-Id", cid)
	header.Set("X-Api-Request-Id", cid)
	header.Set("X-Api-Sequence", "-1")

	conn, resp, err := p.dialer().DialContext(ctx, p.baseURL(), header)
	if err != nil {
		detail := ""
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			detail = fmt.Sprintf(" (HTTP %d: %s)", resp.StatusCode, truncateBytes(b, 300))
		}
		return nil, fmt.Errorf("doubao-asr: 连接失败: %w%s", err, detail)
	}

	fullReq := map[string]any{
		"user":  map[string]any{"uid": "ai-interview-platform"},
		"audio": map[string]any{"format": "pcm", "codec": "raw", "rate": rate, "bits": 16, "channel": 1},
		"request": map[string]any{
			"model_name":      "bigmodel",
			"enable_itn":      true,
			"enable_punc":     true,
			"result_type":     "full",
			"show_utterances": true,
		},
	}
	reqJSON, err := json.Marshal(fullReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	reqGz := gzipBytes(reqJSON)
	frame := asrFrame(asrHeader(asrMFullReq, 0, asrSJSON, asrCGzip), reqGz)
	if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("doubao-asr: 发送配置失败: %w", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	s := &doubaoASRStream{
		ctx: streamCtx, cancel: cancel, conn: conn,
		results: make(chan ASRResult, 32),
	}
	safeGo(streamCtx, "media.doubao_asr", s.readLoop)
	return s, nil
}

type doubaoASRStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *websocket.Conn

	results chan ASRResult
	writeMu sync.Mutex
	once    sync.Once
	atMS    int64
}

func (s *doubaoASRStream) Results() <-chan ASRResult { return s.results }

// Push 发送一帧音频。返回非 nil 表示流已经结束。
func (s *doubaoASRStream) Push(chunk AudioChunk) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	s.atMS += PCMDurationMS(chunk.PCM)
	frame := asrFrame(asrHeader(asrMAudio, 0, 0, asrCGzip), gzipBytes(chunk.PCM))
	s.writeMu.Lock()
	err := s.conn.WriteMessage(websocket.BinaryMessage, frame)
	s.writeMu.Unlock()
	return err
}

// Close 结束本次识别: 发一个带 LAST 标记的空音频帧, 等 final 结果后关闭。
func (s *doubaoASRStream) Close() error {
	s.once.Do(func() {
		frame := asrFrame(asrHeader(asrMAudio, asrFLast, 0, asrCGzip), gzipBytes(nil))
		s.writeMu.Lock()
		_ = s.conn.WriteMessage(websocket.BinaryMessage, frame)
		s.writeMu.Unlock()
		// 服务端会回传 final; readLoop 拿到 is_last 后自行关闭 channel。
		// 不在这里 close(results): 由 readLoop 统一负责。
	})
	return nil
}

func (s *doubaoASRStream) readLoop() {
	defer close(s.results)
	defer s.cancel()
	defer s.conn.Close()

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		text, isLast, ferr := parseASRFrame(data)
		if ferr != nil {
			return
		}
		if text != "" {
			select {
			case s.results <- ASRResult{Text: text, Final: isLast, AtMS: s.atMS}:
			case <-s.ctx.Done():
				return
			}
		}
		if isLast {
			return
		}
	}
}

func asrHeader(mtype, flags, serial, compress byte) [4]byte {
	return [4]byte{
		(asrProtoVer << 4) | asrHdrSize,
		(mtype << 4) | flags,
		(serial << 4) | compress,
		0x00,
	}
}

func asrFrame(header [4]byte, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	copy(out[:4], header[:])
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func gunzipBytes(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// parseASRFrame 解析服务端一帧, 返回 (文本, 是否 final, 错误)。
func parseASRFrame(data []byte) (string, bool, error) {
	if len(data) < 8 {
		return "", false, fmt.Errorf("doubao-asr: 帧过短")
	}
	mtype := (data[1] >> 4) & 0x0F
	flags := data[1] & 0x0F
	serial := (data[2] >> 4) & 0x0F
	compress := data[2] & 0x0F
	off := int(data[0]&0x0F) * 4

	if mtype == asrMError {
		if off+8 > len(data) {
			return "", false, fmt.Errorf("doubao-asr: 错误帧过短")
		}
		_ = binary.BigEndian.Uint32(data[off : off+4]) // code
		size := int(binary.BigEndian.Uint32(data[off+4 : off+8]))
		if off+8+size > len(data) {
			return "", false, fmt.Errorf("doubao-asr: 错误帧长度异常")
		}
		return "", false, fmt.Errorf("doubao-asr: 上游错误: %s", string(data[off+8:off+8+size]))
	}

	if flags&0b0001 != 0 { // 有 sequence 号
		off += 4
	}
	if off+4 > len(data) {
		return "", false, fmt.Errorf("doubao-asr: 帧头异常")
	}
	size := int(binary.BigEndian.Uint32(data[off : off+4]))
	off += 4
	if off+size > len(data) {
		return "", false, fmt.Errorf("doubao-asr: 帧长度异常")
	}
	payload := data[off : off+size]
	if compress == asrCGzip && len(payload) > 0 {
		var err error
		payload, err = gunzipBytes(payload)
		if err != nil {
			return "", false, err
		}
	}
	if serial != asrSJSON || len(payload) == 0 {
		return "", flags&asrFLast != 0, nil
	}
	var obj struct {
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", false, err
	}
	return obj.Result.Text, flags&asrFLast != 0, nil
}
