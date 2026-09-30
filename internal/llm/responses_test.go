package llm

import (
	"strings"
	"testing"
)

// 这些用例只测"协议转换"这一层, 不发起任何 HTTP 请求。
// 协议转换(请求体怎么拼、响应怎么读)是能离线验证的纯函数;
// 而"真连火山方舟"要密钥与网络, 由 -selftest 在真实环境里做。

func TestResponsesInputConvertsMessages(t *testing.T) {
	input := responsesInput([]Message{
		{Role: "system", Content: "你是一名面试官"},
		{Role: "user", Content: "讲讲 GMP"},
	})
	if len(input) != 2 {
		t.Fatalf("应转换两条消息, 实际 %d", len(input))
	}
	if input[0]["role"] != "system" || input[1]["role"] != "user" {
		t.Fatalf("角色丢失: %+v", input)
	}
	content := input[0]["content"].([]map[string]string)
	if content[0]["type"] != "input_text" || content[0]["text"] != "你是一名面试官" {
		t.Fatalf("input_text 结构不对: %+v", content)
	}
}

func TestParseResponsesExtractsAssistantText(t *testing.T) {
	raw := []byte(`{
		"output": [
			{"type": "message", "role": "assistant", "content": [
				{"type": "output_text", "text": "GMP 的核心是 "},
				{"type": "output_text", "text": "P 的本地队列。"}
			]},
			{"type": "web_search_call", "role": "assistant", "content": []}
		],
		"usage": {"input_tokens": 12, "output_tokens": 34, "total_tokens": 46}
	}`)
	resp, err := parseResponses(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Text != "GMP 的核心是 P 的本地队列。" {
		t.Fatalf("助手文本拼接错误: %q", resp.Text)
	}
	if resp.PromptTokens != 12 || resp.CompletionTokens != 34 || resp.TotalTokens != 46 {
		t.Fatalf("token 统计错误: %+v", resp)
	}
}

func TestParseResponsesSkipsNonAssistantAndNonText(t *testing.T) {
	raw := []byte(`{
		"output": [
			{"type": "reasoning", "role": "assistant", "content": [{"type":"reasoning_text","text":"思考过程"}]},
			{"type": "message", "role": "assistant", "content": [
				{"type": "refusal", "text": "拒绝"},
				{"type": "output_text", "text": "正式回答"}
			]}
		]
	}`)
	resp, err := parseResponses(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Text != "正式回答" {
		t.Fatalf("应只取 output_text, 实际 %q", resp.Text)
	}
}

func TestParseResponsesRejectsEmptyText(t *testing.T) {
	raw := []byte(`{"output": [{"type":"message","role":"assistant","content":[]}]}`)
	if _, err := parseResponses(raw); err == nil {
		t.Fatal("没有助手文本时应报错, 而不是返回空串")
	}
}

func TestParseResponsesRejectsMalformedJSON(t *testing.T) {
	if _, err := parseResponses([]byte("not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestRequestForSwitchesProtocol(t *testing.T) {
	chat := NewClient(Config{BaseURL: "https://x", APIKey: "k", Model: "m", APIMode: APIChat})
	_, endpoint, _, err := chat.requestFor([]Message{{Role: "user", Content: "hi"}}, options{maxTokens: 100})
	if err != nil {
		t.Fatalf("组装 chat 请求失败: %v", err)
	}
	if endpoint != "/chat/completions" {
		t.Fatalf("chat 模式应走 /chat/completions, 实际 %s", endpoint)
	}

	resp := NewClient(Config{BaseURL: "https://x", APIKey: "k", Model: "m", APIMode: APIResponses})
	body, endpoint, _, err := resp.requestFor([]Message{{Role: "user", Content: "hi"}}, options{maxTokens: 100})
	if err != nil {
		t.Fatalf("组装 responses 请求失败: %v", err)
	}
	if endpoint != "/responses" {
		t.Fatalf("responses 模式应走 /responses, 实际 %s", endpoint)
	}
	if !strings.Contains(string(body), `"input"`) || !strings.Contains(string(body), `"max_output_tokens"`) {
		t.Fatalf("responses 请求体应含 input 与 max_output_tokens: %s", body)
	}
}

func TestFromEnvReadsAPIMode(t *testing.T) {
	t.Setenv("LLM_API_MODE", "responses")
	if got := FromEnv().APIMode; got != APIResponses {
		t.Fatalf("应读取 LLM_API_MODE, 实际 %q", got)
	}
}

func TestNewClientDefaultsToChatMode(t *testing.T) {
	c := NewClient(Config{})
	if c.cfg.APIMode != APIChat {
		t.Fatalf("未指定协议时应默认 chat, 实际 %q", c.cfg.APIMode)
	}
}
