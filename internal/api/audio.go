package api

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// connWriter 串行化对 WebSocket 的写。
//
// gorilla/websocket 同一时刻只允许一个写者。语音模式下, 事件泵与
// 回合协程都可能写连接, 所以用一个互斥锁包住所有写操作 —— 这类并发写
// 导致的协议错乱, 通常要压测才暴露, 但根因在写代码的第一天就埋下了。
type connWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *connWriter) writeJSON(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.WriteJSON(v)
}

func (w *connWriter) writeBinary(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.WriteMessage(websocket.BinaryMessage, b)
}

// voiceLoop 用 media.Session 驱动一场语音面试。
//
// 协议(与文本模式共用同一个 /ws/interview/{id} 连接):
//
//	客户端 -> 服务端: 二进制帧 = PCM16 音频; JSON {"type":"interrupt","played_ms":N}
//	服务端 -> 客户端: 二进制帧 = 助手 PCM16 音频; JSON 事件
//
// 打断的两条路径都成立:
//   - 服务端 VAD 检测到插话, 主动取消在途的 TTS 流;
//   - 前端检测到用户说话, 上报已播进度, 服务端据此记录"AI 被听到了什么"。
//
// 并发模型: 一个 goroutine 读连接(音频/打断), 主循环处理识别结果并播报。
// Speak 阻塞直到说完或被打断 —— 打断通过 context 级联取消实现, 不轮询标志位。
func (s *Server) voiceLoop(ctx context.Context, conn *websocket.Conn, eng *orchestrator.Engine,
	sess store.Session, firstFrame []byte, asr media.ASRProvider, tts media.TTSProvider) {

	writer := &connWriter{conn: conn}
	finalCh := make(chan media.Event, 16)

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ms := media.NewSession(loopCtx, asr, tts, s.voiceConfig(), media.SinkFunc(func(ev media.Event) {
		if ev.Type == media.EvAssistantAudio {
			writer.writeBinary(ev.PCM)
			return
		}
		writer.writeJSON(voiceEventJSON(ev))
		if ev.Type == media.EvFinalTranscript {
			select {
			case finalCh <- ev:
			default:
			}
		}
	}))
	defer ms.Close()

	// 播报当前待答的问题(文本已在握手时下发, 这里补上语音)。
	_, pendingText, ok := eng.Pending()
	if !ok {
		d := eng.Start()
		pendingText = d.Question
	}
	questionSentAt := time.Now()
	go func() { _ = ms.Speak(loopCtx, pendingText) }()

	// 读连接: 二进制帧喂给 VAD/ASR, interrupt 触发打断。
	go func() {
		defer cancel()
		if len(firstFrame) > 0 {
			ms.OnAudioFrame(firstFrame)
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				ms.OnAudioFrame(data)
				continue
			}
			var msg struct {
				Type     string `json:"type"`
				PlayedMS int64  `json:"played_ms"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "interrupt" {
				ms.Interrupt(msg.PlayedMS)
			}
		}
	}()

	turnCount := 0
	for {
		select {
		case <-loopCtx.Done():
			return
		case ev := <-finalCh:
			// 识别出一段完整回答, 推进引擎。
			decision, err := eng.Submit(ev.Text, time.Since(questionSentAt))
			if err != nil {
				writer.writeJSON(map[string]any{"type": "error", "message": err.Error()})
				continue
			}
			if err := s.persistTurn(loopCtx, sess.ID, decision.Turn); err != nil {
				s.logger.Printf("语音回答落库失败: %v", err)
			}
			turnCount++
			writer.writeJSON(turnResultPayload(decision))

			if decision.Action == orchestrator.ActionFinish {
				rep, err := s.finishSession(loopCtx, sess, eng)
				if err != nil {
					writer.writeJSON(map[string]any{"type": "error", "message": err.Error()})
					return
				}
				payload, _ := json.Marshal(rep)
				writer.writeJSON(map[string]any{
					"type": "report", "payload": json.RawMessage(payload),
				})
				return
			}

			writer.writeJSON(questionPayload(decision, turnCount+1))
			questionSentAt = time.Now()
			_ = ms.Speak(loopCtx, decision.Question)
		}
	}
}

func (s *Server) voiceConfig() media.SessionConfig {
	cfg := media.DefaultSessionConfig()
	// 语音场景下 ASR 需要知道技术热词, 否则 Goroutine、ZSet 这类词基本必错。
	// 这里把题库里的判定要点都塞进热词表 —— 这是零成本的准确率提升。
	hot := make(map[string]bool)
	for _, q := range orchestrator.DefaultBank().All() {
		for _, kw := range q.Keywords {
			hot[kw] = true
		}
	}
	for kw := range hot {
		cfg.ASR.HotWords = append(cfg.ASR.HotWords, kw)
	}
	return cfg
}

// voiceEventJSON 把媒体事件转成给前端的 JSON。
func voiceEventJSON(ev media.Event) map[string]any {
	switch ev.Type {
	case media.EvPartialTranscript:
		return map[string]any{"type": "partial_transcript", "text": ev.Text, "at_ms": ev.AtMS}
	case media.EvFinalTranscript:
		return map[string]any{"type": "final_transcript", "text": ev.Text, "at_ms": ev.AtMS}
	case media.EvUserSpeechStart:
		return map[string]any{"type": "user_speech_start", "at_ms": ev.AtMS}
	case media.EvUserSpeechEnd:
		return map[string]any{"type": "user_speech_end", "at_ms": ev.AtMS}
	case media.EvInterrupted:
		return map[string]any{"type": "interrupted", "heard_text": ev.HeardText, "at_ms": ev.AtMS}
	case media.EvAssistantText:
		return map[string]any{"type": "assistant_text", "text": ev.Text}
	case media.EvAssistantSentence:
		return map[string]any{"type": "assistant_sentence", "text": ev.Text}
	case media.EvError:
		return map[string]any{"type": "error", "message": ev.Err}
	default:
		return map[string]any{"type": "media_event", "subtype": string(ev.Type)}
	}
}
