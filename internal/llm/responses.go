package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件实现 OpenAI / 火山方舟的 Responses 协议适配。
//
// 与 /chat/completions 的区别主要在两点:
//   - 请求体把 `messages` 换成 `input` 数组, 文本用
//     {type:"input_text", text:...} 结构;
//   - 响应把 `choices[0].message.content` 换成 `output` 数组里的
//     {type:"message", content:[{type:"output_text", text:...}]}。
//
// 刻意只支持**非流式**(stream=false): 我们的评分与追问都是"一次性取回
// 完整文本"的调用, 不需要 SSE 流式解析。需要流式(比如实时追问打字机效果)
// 时再单独加, 不在这一层混进流式与非流式两套解析。

// responsesInput 把内部消息转成 Responses 协议的 input 数组。
func responsesInput(messages []Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, map[string]any{
			"role": m.Role,
			"content": []map[string]string{
				{"type": "input_text", "text": m.Content},
			},
		})
	}
	return out
}

// responsesEnvelope 是 /responses 的非流式响应结构(只取用到的字段)。
type responsesEnvelope struct {
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// parseResponses 解析 /responses 的响应, 拼出助手文本。
func parseResponses(raw []byte) (Response, error) {
	var out responsesEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("llm: 解析 responses 响应失败: %w", err)
	}

	var text strings.Builder
	for _, item := range out.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	if text.Len() == 0 {
		return Response{}, errors.New("llm: responses 响应中没有助手文本")
	}

	return Response{
		Text:             text.String(),
		PromptTokens:     out.Usage.InputTokens,
		CompletionTokens: out.Usage.OutputTokens,
		TotalTokens:      out.Usage.TotalTokens,
	}, nil
}
