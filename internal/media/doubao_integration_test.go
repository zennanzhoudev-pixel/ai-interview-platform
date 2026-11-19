package media

import (
	"context"
	"os"
	"testing"
)

// TestDoubaoTTSAndASRRoundTrip 用真实火山语音技术做一次闭环:
// TTS 合成一句中文 -> ASR 再把这段音频识别回文字。
// 如果识别结果非空, 就证明 ASR/TTS 两端的凭证与协议都通。
//
// 未配置 DOUBAO_API_KEY 时跳过。
func TestDoubaoTTSAndASRRoundTrip(t *testing.T) {
	key := os.Getenv("DOUBAO_API_KEY")
	if key == "" {
		t.Skip("未设置 DOUBAO_API_KEY, 跳过豆包语音真连测试")
	}

	ctx := context.Background()

	// 1. TTS 合成
	tts := &DoubaoTTS{APIKey: key}
	stream, err := tts.Speak(ctx, "你好，欢迎参加今天的面试。", Voice{})
	if err != nil {
		t.Fatalf("TTS 合成失败: %v", err)
	}
	var pcm []byte
	for chunk := range stream.Chunks() {
		pcm = append(pcm, chunk.PCM...)
	}
	if len(pcm) == 0 {
		t.Fatal("TTS 返回了空音频")
	}
	t.Logf("TTS 合成 %d 字节 PCM, 约 %d ms", len(pcm), PCMDurationMS(pcm))

	// 2. ASR 识别(按 20ms 一帧送入)
	asr := &DoubaoASR{APIKey: key}
	if rid := os.Getenv("DOUBAO_ASR_RESOURCE_ID"); rid != "" {
		asr.ResourceID = rid
	}
	as, err := asr.Open(ctx, ASRConfig{SampleRate: SampleRate})
	if err != nil {
		t.Fatalf("ASR 打开失败: %v", err)
	}
	for i := 0; i < len(pcm); i += FrameBytes {
		end := i + FrameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := as.Push(AudioChunk{PCM: pcm[i:end]}); err != nil {
			t.Fatalf("ASR Push 失败: %v", err)
		}
	}
	_ = as.Close()

	// 豆包 ASR 的 partial 每次都返回"到目前为止的完整文本",
	// 所以这里取最后一次, 而不是把所有结果拼接起来。
	var transcript string
	for r := range as.Results() {
		if r.Text != "" {
			transcript = r.Text
		}
	}
	t.Logf("ASR 识别结果: %q", transcript)
	if transcript == "" {
		t.Fatal("ASR 未返回任何识别文本")
	}
}
