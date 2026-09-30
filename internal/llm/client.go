// Package llm 提供 OpenAI 兼容的对话补全客户端与结构化输出校验。
//
// 只依赖标准库, 目的是不被任何一家厂商的 SDK 绑死 —— 只要对方兼容
// /chat/completions 协议, 换 base_url 就能切过去(OpenAI、DeepSeek、
// Moonshot、通义、vLLM、Ollama 都兼容这一套)。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config 是客户端配置。
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	// Timeout 是单次请求超时。注意它必须明显小于面试轮次的容忍时间:
	// 评分慢一点没关系, 但卡住对话链路就是事故。
	Timeout    time.Duration
	MaxRetries int
	// DisableJSONMode 关闭"强制 JSON 输出"。
	//
	// 默认开启 —— 评分场景下模型自由发挥地写解释文字没有任何价值,
	// 只会让解析失败率上升。少数兼容服务不支持 response_format,
	// 这时显式关掉即可。注意这里用的是"反向开关"而不是 JSONMode bool:
	// 零值 Config 应该得到最安全的默认行为, 而不是悄悄失去约束。
	DisableJSONMode bool
	// APIMode 选择对话协议:
	//   - "chat"(默认): POST /chat/completions, OpenAI 经典协议;
	//   - "responses": POST /responses, 火山方舟/OpenAI 新版 Responses 协议。
	// 两家协议的请求体与响应结构不通用, 因此必须显式选择, 不能靠"自动探测"。
	APIMode string
}

// 对话协议常量。
const (
	APIChat      = "chat"
	APIResponses = "responses"
)

// DefaultConfig 返回一份可直接使用的默认配置。
func DefaultConfig() Config {
	return Config{
		BaseURL:     "https://api.openai.com/v1",
		Model:       "gpt-4o-mini",
		Temperature: 0,
		MaxTokens:   800,
		Timeout:     20 * time.Second,
		MaxRetries:  2,
	}
}

// FromEnv 从环境变量读取配置。
//
// 用环境变量而不是配置文件, 是为了在容器里能直接对接任意兼容服务,
// 也避免密钥被误提交进仓库。
func FromEnv() Config {
	cfg := DefaultConfig()
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		cfg.APIKey = v
	} else if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("LLM_API_MODE"); v != "" {
		cfg.APIMode = v
	}
	if os.Getenv("LLM_DISABLE_JSON_MODE") != "" {
		cfg.DisableJSONMode = true
	}
	return cfg
}

// Enabled 表示配置是否足以发起调用。
func (c Config) Enabled() bool { return c.APIKey != "" && c.Model != "" }

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Response 是一次调用的结果。
type Response struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Latency          time.Duration
	Attempts         int
}

// APIError 是上游返回的非 2xx 响应。
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: HTTP %d: %s", e.StatusCode, e.Body)
}

// Retryable 判断该响应是否值得重试。
// 429(限流)和 5xx(上游故障)值得; 其余 4xx 说明请求本身有问题, 重试没有意义。
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// IsRetryable 判断错误是否属于"可以再试一次"。
//
// 注意: 调用方在重试前还必须确认父 context 没结束 ——
// 用户主动取消(比如打断)导致的中断绝不该重试。
func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return true
}

// Client 是 OpenAI 兼容的对话补全客户端。
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient 构造客户端。
func NewClient(cfg Config) *Client {
	def := DefaultConfig()
	if cfg.BaseURL == "" {
		cfg.BaseURL = def.BaseURL
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = def.MaxTokens
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.APIMode == "" {
		cfg.APIMode = APIChat
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{},
	}
}

// Model 返回当前模型名。
func (c *Client) Model() string { return c.cfg.Model }

// Chat 发起一次对话补全。
func (c *Client) Chat(ctx context.Context, messages []Message, opts ...Option) (Response, error) {
	if !c.cfg.Enabled() {
		return Response{}, errors.New("llm: 未配置 API Key 或模型")
	}

	o := options{
		temperature: c.cfg.Temperature,
		maxTokens:   c.cfg.MaxTokens,
		jsonMode:    !c.cfg.DisableJSONMode,
	}
	for _, opt := range opts {
		opt(&o)
	}

	body, endpoint, parse, err := c.requestFor(messages, o)
	if err != nil {
		return Response{}, err
	}

	attempts := c.cfg.MaxRetries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}

		resp, err := c.do(ctx, endpoint, body, parse)
		resp.Attempts = attempt
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// 父 context 已结束(用户取消 / 面试被打断)时立即放弃。
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !IsRetryable(err) || attempt == attempts {
			break
		}

		delay := backoff(attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	return Response{}, lastErr
}

// requestFor 按协议模式组装请求体、端点与响应解析函数。
func (c *Client) requestFor(messages []Message, o options) ([]byte, string, responseParser, error) {
	if c.cfg.APIMode == APIResponses {
		body, err := json.Marshal(map[string]any{
			"model":             c.cfg.Model,
			"input":             responsesInput(messages),
			"temperature":       o.temperature,
			"max_output_tokens": o.maxTokens,
		})
		return body, "/responses", parseResponses, err
	}

	payload := map[string]any{
		"model":       c.cfg.Model,
		"messages":    messages,
		"temperature": o.temperature,
		"max_tokens":  o.maxTokens,
	}
	if o.jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	return body, "/chat/completions", parseCompletions, err
}

type responseParser func([]byte) (Response, error)

func (c *Client) do(ctx context.Context, endpoint string, body []byte, parse responseParser) (Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	latency := time.Since(start)

	if resp.StatusCode/100 != 2 {
		return Response{Latency: latency}, &APIError{
			StatusCode: resp.StatusCode,
			Body:       truncate(string(raw), 300),
		}
	}

	parsed, err := parse(raw)
	if err != nil {
		return Response{Latency: latency}, err
	}
	parsed.Latency = latency
	return parsed, nil
}

// parseCompletions 解析 /chat/completions 的响应。
func parseCompletions(raw []byte) (Response, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("llm: 解析响应失败: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("llm: 响应中没有 choices")
	}

	return Response{
		Text:             out.Choices[0].Message.Content,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		TotalTokens:      out.Usage.TotalTokens,
	}, nil
}

type options struct {
	temperature float64
	maxTokens   int
	jsonMode    bool
}

// Option 调整单次调用的参数。
type Option func(*options)

// WithTemperature 覆盖温度。
func WithTemperature(v float64) Option {
	return func(o *options) { o.temperature = v }
}

// WithMaxTokens 覆盖最大输出长度。
func WithMaxTokens(v int) Option {
	return func(o *options) { o.maxTokens = v }
}

// WithJSONMode 覆盖 JSON 模式开关。
func WithJSONMode(on bool) Option {
	return func(o *options) { o.jsonMode = on }
}

// backoff 返回带抖动的指数退避时长。
// 抖动不是可有可无的细节: 上游限流时, 成百上千个并发请求如果按
// 同一个节奏重试, 会形成新的尖峰, 把刚恢复的服务再打垮一次。
func backoff(attempt int) time.Duration {
	base := time.Duration(300*(1<<(attempt-1))) * time.Millisecond
	if base > 4*time.Second {
		base = 4 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base/2) + 1))
	return base + jitter
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "..."
}
