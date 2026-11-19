// Command latencybench 测量面试关键链路的真实延迟:
// LLM 首字(整次调用)延迟、TTS 首块延迟、ASR 端到端识别延迟。
//
// 这些数字是企业级面试系统必须有的: 没有它们, "体验好不好"只能靠感觉,
// 也没法在模型/厂商之间做有依据的取舍。
//
// 用法(与主服务共用同一组环境变量):
//
//	LLM_BASE_URL=... LLM_API_KEY=... LLM_MODEL=... LLM_API_MODE=chat \
//	ASR_API_KEY=... ASR_PROVIDER=doubao \
//	TTS_API_KEY=... TTS_PROVIDER=doubao \
//	go run ./cmd/latencybench
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
)

func main() {
	ctx := context.Background()
	fmt.Println("面试链路延迟测量")
	fmt.Println("----------------------------------------")

	var ttsPCM []byte

	// 1. LLM: 端到端(非流式)调用延迟。
	if cfg := llm.FromEnv(); cfg.Enabled() {
		start := time.Now()
		_, err := llm.NewClient(cfg).Chat(ctx,
			[]llm.Message{{Role: "user", Content: "ping"}},
			llm.WithMaxTokens(64), llm.WithJSONMode(false))
		report("LLM 调用", time.Since(start), err)
	} else {
		fmt.Println("LLM       : 未配置(LLM_API_KEY)")
	}

	// 2. TTS: 首块音频延迟 + 整体合成时长。
	if key := os.Getenv("TTS_API_KEY"); key != "" {
		tts := newTTS(key)
		start := time.Now()
		stream, err := tts.Speak(ctx, "你好，欢迎参加今天的面试。", media.Voice{})
		if err != nil {
			report("TTS 首块", time.Since(start), err)
		} else {
			first, ok := <-stream.Chunks()
			firstLatency := time.Since(start)
			if !ok {
				fmt.Println("TTS 首块  : 空音频流")
			} else {
				ttsPCM = append(ttsPCM, first.PCM...)
				for c := range stream.Chunks() {
					ttsPCM = append(ttsPCM, c.PCM...)
				}
				fmt.Printf("TTS 首块  : %s | 合成总耗时 %s | 音频 %d ms\n",
					firstLatency.Round(time.Millisecond),
					time.Since(start).Round(time.Millisecond),
					media.PCMDurationMS(ttsPCM))
			}
		}
	} else {
		fmt.Println("TTS       : 未配置(TTS_API_KEY)")
	}

	// 3. ASR: 用 TTS 合成出的音频做端到端识别, 测"最后一次 Push 到拿到 final"的延迟。
	if key := os.Getenv("ASR_API_KEY"); key != "" {
		if len(ttsPCM) == 0 {
			fmt.Println("ASR       : 跳过(没有可用的 TTS 音频)")
		} else {
			asr := newASR(key)
			as, err := asr.Open(ctx, media.ASRConfig{SampleRate: media.SampleRate})
			if err != nil {
				report("ASR 打开", 0, err)
			} else {
				for i := 0; i < len(ttsPCM); i += media.FrameBytes {
					end := i + media.FrameBytes
					if end > len(ttsPCM) {
						end = len(ttsPCM)
					}
					if err := as.Push(media.AudioChunk{PCM: ttsPCM[i:end]}); err != nil {
						break
					}
				}
				start := time.Now()
				_ = as.Close()
				var text string
				for r := range as.Results() {
					if r.Text != "" {
						text = r.Text
					}
				}
				fmt.Printf("ASR 收尾  : %s | 识别结果 %q\n",
					time.Since(start).Round(time.Millisecond), text)
			}
		}
	} else {
		fmt.Println("ASR       : 未配置(ASR_API_KEY)")
	}

	fmt.Println("----------------------------------------")
}

func report(name string, d time.Duration, err error) {
	if err != nil {
		fmt.Printf("%-9s : 失败(%v)\n", name, err)
		return
	}
	fmt.Printf("%-9s : %s\n", name, d.Round(time.Millisecond))
}

func newTTS(key string) media.TTSProvider {
	if os.Getenv("TTS_PROVIDER") == "doubao" {
		return &media.DoubaoTTS{APIKey: key, ResourceID: os.Getenv("TTS_RESOURCE_ID"), Voice: os.Getenv("TTS_VOICE")}
	}
	return &media.OpenAITTS{BaseURL: os.Getenv("TTS_BASE_URL"), APIKey: key, Model: os.Getenv("TTS_MODEL")}
}

func newASR(key string) media.ASRProvider {
	if os.Getenv("ASR_PROVIDER") == "doubao" {
		return &media.DoubaoASR{APIKey: key, ResourceID: os.Getenv("ASR_RESOURCE_ID")}
	}
	return &media.OpenAIASR{BaseURL: os.Getenv("ASR_BASE_URL"), APIKey: key, Model: os.Getenv("ASR_MODEL")}
}
