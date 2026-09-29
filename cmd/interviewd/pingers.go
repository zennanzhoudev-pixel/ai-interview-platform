package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/api"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/media"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
)

// 上游能力的真实探测。
//
// 这些 probe 存在的意义只有一个: 把"配了密钥"和"真的能用"分开。
// 面试当天才发现密钥失效, 代价是一场面试直接作废 —— 而一次 1 token
// 的探测请求只需要几十毫秒。

// llmPing 探测大模型可用性。
type llmPing struct {
	cfg llm.Config
}

// Ping 用最小代价确认模型与凭证都有效。
func (p llmPing) Ping(ctx context.Context) error {
	_, err := llm.NewClient(p.cfg).Chat(ctx,
		[]llm.Message{{Role: "user", Content: "ping"}}, llm.WithMaxTokens(1))
	if err != nil {
		return fmt.Errorf("模型 %s 探测失败: %w", p.cfg.Model, err)
	}
	return nil
}

// embedPing 探测嵌入模型。
type embedPing struct {
	emb rag.Embedder
}

// Ping 计算一个短文本的向量。
func (p embedPing) Ping(ctx context.Context) error {
	vec, err := p.emb.Embed(ctx, "ping")
	if err != nil {
		return fmt.Errorf("嵌入模型 %s 探测失败: %w", p.emb.Name(), err)
	}
	if len(vec) == 0 {
		return fmt.Errorf("嵌入模型 %s 返回了空向量", p.emb.Name())
	}
	return nil
}

// ttsPing 探测语音合成。
type ttsPing struct {
	provider media.TTSProvider
}

// Ping 合成一个短句并读取首块音频。
//
// 只判断"能不能拿到第一块"而不是"是否合成完整": 面试体验的关键指标
// 就是首块延迟, 能拿到首块说明凭证、网络与模型三件事都成立。
func (p ttsPing) Ping(ctx context.Context) error {
	stream, err := p.provider.Speak(ctx, "你好", media.Voice{})
	if err != nil {
		return fmt.Errorf("语音合成失败: %w", err)
	}
	defer func() { _ = stream.Close() }()
	select {
	case <-ctx.Done():
		return fmt.Errorf("语音合成超时, 未收到音频")
	case chunk, ok := <-stream.Chunks():
		if !ok {
			return fmt.Errorf("语音合成返回了空音频流")
		}
		if len(chunk.PCM) == 0 {
			return fmt.Errorf("语音合成返回了空音频块")
		}
		return nil
	}
}

// asrPing 如实声明语音识别无法被探针验证。
type asrPing struct {
	provider media.ASRProvider
}

// Ping 返回 ErrPingUnsupported。
func (p asrPing) Ping(context.Context) error {
	// 说明为什么不探: 识别质量取决于真实音频与热词表, 静音探测只会
	// 得到"接口通了"这种没有信息量的结论, 反而会掩盖真正的问题
	// (例如热词没生效导致 Goroutine 被识别成"高肉停")。
	_ = p.provider
	return fmt.Errorf("%w: 语音识别需要在真实面试中验证(热词与采样率无法离线复现)",
		api.ErrPingUnsupported)
}

// buildPingers 组装全部探测器。
func buildPingers(ts media.TTSProvider, asr media.ASRProvider, emb rag.Embedder, cfg llm.Config) map[string]api.ProviderPing {
	out := make(map[string]api.ProviderPing)
	if cfg.Enabled() {
		out["llm"] = llmPing{cfg: cfg}
		if modelB := strings.TrimSpace(os.Getenv("LLM_MODEL_B")); modelB != "" {
			cfgB := cfg
			cfgB.Model = modelB
			out["llm_secondary"] = llmPing{cfg: cfgB}
		}
	}
	if emb != nil {
		out["embedding"] = embedPing{emb: emb}
	}
	if ts != nil {
		out["tts"] = ttsPing{provider: ts}
	}
	if asr != nil {
		out["asr"] = asrPing{provider: asr}
	}
	return out
}
