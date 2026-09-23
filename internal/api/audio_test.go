package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// TestVoiceLoopCarriesAudioUplinkAndTranscript 验证语音模式整条链路:
// 二进制音频上行 -> VAD -> ASR 转写 -> 引擎推进 -> TTS 二进制音频下行。
//
// 用 ScriptedASR 让"候选人说的话"可以离线确定, 用 LocalTTS 产静音音频,
// 这样在没有真实密钥的情况下也能完整回归这条链路。
func TestVoiceLoopCarriesAudioUplinkAndTranscript(t *testing.T) {
	const answer = "我们把排行榜拆成 ZSet 索引, 用 score 存权重, 查询 RT 降了 70%。"
	asr := &media.ScriptedASR{
		NextText:         func() string { return answer },
		FramesPerPartial: 1,
		RunesPerPartial:  20,
	}
	tts := &media.LocalTTS{Realtime: false}

	ts := httptest.NewServer(NewServer(Config{
		Store: store.NewMemoryStore(),
		ASR:   asr,
		TTS:   tts,
	}).Handler())
	defer ts.Close()

	_, body := createSession(t, ts.URL, map[string]any{
		"round": 1, "minutes": 45, "consent_recording": true,
	})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	_ = readMsg(t, conn) // state
	_ = readMsg(t, conn) // 首题(开场)

	if err := conn.WriteJSON(map[string]any{"type": "start_voice"}); err != nil {
		t.Fatalf("启动语音模式失败: %v", err)
	}

	send := func(frames int, pcm []byte) {
		for i := 0; i < frames; i++ {
			if err := conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
				t.Fatalf("发送音频失败: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// 3 帧触发 VAD 开始 + 8 帧语音 + 30 帧静音触发结束
	speech := media.SynthSine(media.FrameMS, 220, 0.3)
	silence := media.SynthSilence(media.FrameMS)
	send(3, speech)
	send(8, speech)
	send(30, silence)

	binaryCount := 0
	sawTranscript := false
	sawTurnResult := false
	nextQuestionID := ""

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			binaryCount++
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg["type"] {
		case "final_transcript":
			if msg["text"] == answer {
				sawTranscript = true
			}
		case "turn_result":
			sawTurnResult = true
		case "question":
			nextQuestionID, _ = msg["id"].(string)
		}
		if sawTurnResult && nextQuestionID != "" {
			break
		}
	}

	if binaryCount == 0 {
		t.Fatal("应收到助手的二进制音频(TTS 下行)")
	}
	if !sawTranscript {
		t.Fatal("应收到识别出的 final_transcript")
	}
	if !sawTurnResult {
		t.Fatal("回答应被引擎评分并返回 turn_result")
	}
	if nextQuestionID != "q_resume_zset" {
		t.Fatalf("开场回答后应推进到简历深挖, 实际下一题 %q", nextQuestionID)
	}
}

// 未配置 ASR/TTS 时, 发送二进制音频应得到明确错误而不是静默吞掉。
func TestVoiceModeRejectsBinaryWithoutProviders(t *testing.T) {
	ts := httptest.NewServer(NewServer(Config{Store: store.NewMemoryStore()}).Handler())
	defer ts.Close()

	_, body := createSession(t, ts.URL, map[string]any{"round": 1, "consent_recording": true})
	id := body["session_id"].(string)

	conn := dialWS(t, ts, id)
	_ = readMsg(t, conn)
	_ = readMsg(t, conn)

	if err := conn.WriteMessage(websocket.BinaryMessage, media.SynthSilence(media.FrameMS)); err != nil {
		t.Fatalf("发送音频失败: %v", err)
	}

	msg := readMsg(t, conn)
	if msg["type"] != "error" {
		t.Fatalf("未配置语音提供方时应返回错误, 实际 %v", msg["type"])
	}
}
