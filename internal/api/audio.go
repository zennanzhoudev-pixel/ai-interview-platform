package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// clientMessage 是客户端通过文本帧发来的消息。
type clientMessage struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	DurationMS int64  `json:"duration_ms"`
}

// connWriter 串行化对 WebSocket 的写。
//
// gorilla/websocket 同一时刻只允许一个写者。语音模式下事件泵、心跳协程与
// 回合协程都会写连接, 所以用一个互斥锁包住所有写操作 —— 这类并发写导致的
// 协议错乱通常要压测才暴露, 但根因在写代码的第一天就埋下了。
type connWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *connWriter) writeJSON(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_ = w.conn.WriteJSON(v)
}

func (w *connWriter) writeBinary(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_ = w.conn.WriteMessage(websocket.BinaryMessage, b)
}

// writeControl 发送控制帧(心跳)。控制帧不能与数据帧并发写, 因此共用同一把锁。
func (w *connWriter) writeControl(messageType int, deadline time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteControl(messageType, nil, time.Now().Add(deadline))
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
func (s *Server) voiceLoop(ctx context.Context, conn *websocket.Conn, eng *orchestrator.Engine,
	sess store.Session, firstFrame []byte) {

	writer := &connWriter{conn: conn}
	finalCh := make(chan media.Event, 16)

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ms := media.NewSession(loopCtx, s.cfg.ASR, s.cfg.TTS, s.voiceConfig(), media.SinkFunc(func(ev media.Event) {
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
	// 播报放到独立 goroutine: Speak 会阻塞直到说完或被打断,
	// 不能卡住下面的读循环。用 platform.Go 保证它 panic 时不会带走整个进程。
	platform.Go(loopCtx, s.logger, "voice.speak", func(name string) {
		s.metrics.Panics.WithLabelValues(name).Inc()
	}, func() { _ = ms.Speak(loopCtx, pendingText) })

	// 读连接: 二进制帧喂给 VAD/ASR, interrupt 触发打断。
	platform.Go(loopCtx, s.logger, "voice.reader", func(name string) {
		s.metrics.Panics.WithLabelValues(name).Inc()
	}, func() {
		defer cancel()
		if len(firstFrame) > 0 {
			ms.OnAudioFrame(firstFrame)
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			s.metrics.WSMessages.WithLabelValues(messageKind(mt)).Inc()
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
	})

	turnCount := 0
	for {
		select {
		case <-loopCtx.Done():
			return
		case ev := <-finalCh:
			replyStarted := time.Now()
			decision, err := eng.Submit(ev.Text, time.Since(questionSentAt))
			if err != nil {
				writer.writeJSON(map[string]any{"type": "error", "message": err.Error()})
				continue
			}
			if err := s.persistTurn(loopCtx, sess, decision.Turn); err != nil {
				s.logger.ErrorContext(loopCtx, "语音回答落库失败",
					append(platform.AuditAttrs(loopCtx), slog.Any("err", err))...)
			}
			turnCount++
			s.metrics.FirstResponse.WithLabelValues(sess.TenantID).
				Observe(time.Since(replyStarted).Seconds())
			writer.writeJSON(turnResultPayload(decision))

			if decision.Action == orchestrator.ActionFinish {
				rep, ferr := s.finishSession(loopCtx, sess, eng)
				if ferr != nil {
					writer.writeJSON(map[string]any{"type": "error", "message": ferr.Error()})
					return
				}
				payload, _ := json.Marshal(rep)
				writer.writeJSON(map[string]any{"type": "report", "payload": json.RawMessage(payload)})
				return
			}

			writer.writeJSON(questionPayload(decision, turnCount+1))
			questionSentAt = time.Now()
			_ = ms.Speak(loopCtx, decision.Question)
		}
	}
}

// voiceConfig 组装语音链路配置, 并把题库里的技术热词塞进 ASR。
// 面试场景下 "Goroutine / ZSet / 幂等" 这类词通用模型基本必错,
// 而它们恰恰是评分要点 —— 热词表是零成本的准确率提升。
func (s *Server) voiceConfig() media.SessionConfig {
	cfg := media.DefaultSessionConfig()
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
