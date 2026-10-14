package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
)

// FollowUpContext 是生成追问措辞所需的上下文。
type FollowUpContext struct {
	ParentQuestion  string
	CandidateAnswer string
	Focus           string
	// Reference 是参考答案里关于该考点的表述。追问必须锚定它,
	// 这是"不让追问跑偏"的约束来源。
	Reference   string
	ResumeQuote string
	Depth       int
	Repeated    bool
}

// FollowUpPhraser 生成追问措辞。
//
// 返回空字符串或 error 都表示"调用方回落到默认模板" —— 措辞失败永远
// 不能让面试卡住, 也不能让追问凭空消失。
type FollowUpPhraser interface {
	Phrase(c FollowUpContext) (string, error)
}

// LLMFollowUpPhraser 用大模型把追问说得更自然, 但必须锚定参考答案。
type LLMFollowUpPhraser struct {
	client  *llm.Client
	timeout time.Duration
}

// NewLLMFollowUpPhraser 构造 LLM 追问措辞器。
func NewLLMFollowUpPhraser(client *llm.Client) *LLMFollowUpPhraser {
	return &LLMFollowUpPhraser{client: client, timeout: 15 * time.Second}
}

// Phrase 生成一句自然追问。
func (p *LLMFollowUpPhraser) Phrase(c FollowUpContext) (string, error) {
	if p.client == nil {
		return "", errors.New("orchestrator: 追问措辞器缺少 LLM 客户端")
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	resp, err := p.client.Chat(ctx,
		[]llm.Message{{Role: "user", Content: followUpPrompt(c)}},
		llm.WithMaxTokens(400),
	)
	if err != nil {
		return "", err
	}

	var out struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(resp.Text), &out); err != nil {
		return "", fmt.Errorf("orchestrator: 解析追问失败: %w", err)
	}

	q := strings.TrimSpace(out.Question)
	if q == "" {
		return "", errors.New("orchestrator: 模型返回了空追问")
	}
	if !followUpAnchored(c.Reference, q) {
		return "", errors.New("orchestrator: 追问偏离参考答案, 拒绝采用")
	}
	return q, nil
}

// followUpPrompt 把上下文拼成约束明确的追问 prompt。
//
// 约束写得很"重"是有意为之: 追问是 AI 现编的内容, 唯一能证明它没跑偏的
// 依据就是"仍落在参考答案里", 所以参考答案被单独强调为不可逾越的边界。
func followUpPrompt(c FollowUpContext) string {
	var b strings.Builder
	b.WriteString("你是技术面试官，要根据候选人的上一轮回答，针对一个具体考点生成一句自然的追问。\n\n")
	fmt.Fprintf(&b, "原题：%s\n", c.ParentQuestion)
	fmt.Fprintf(&b, "候选人回答：%s\n", c.CandidateAnswer)
	fmt.Fprintf(&b, "要追问的考点：%s\n", c.Focus)
	fmt.Fprintf(&b, "参考答案里关于该考点的表述（追问必须围绕它，不得引入它之外的任何技术点、术语或事实）：%s\n", c.Reference)
	if c.ResumeQuote != "" {
		fmt.Fprintf(&b, "候选人简历里相关原话（若非空，优先引用它）：%s\n", c.ResumeQuote)
	}
	if c.Repeated {
		b.WriteString("这是对同一考点的第二次追问，请换一个角度问，不要重复上一句。\n")
	}
	b.WriteString("\n要求：只输出一句口语化、自然的追问；不重复原题；不引入参考答案之外的内容。\n")
	b.WriteString(`输出 JSON：{"question":"..."}`)
	return b.String()
}

// followUpAnchored 校验追问是否仍锚定在参考答案的要点上。
//
// 这是"防止追问跑偏"的最后一道闸门: 生成的追问和参考答案连一个词都不重合,
// 大概率就是模型在自由发挥, 直接判定失败并回落到确定性模板。
func followUpAnchored(reference, question string) bool {
	tokens := rag.Tokenize(reference)
	if len(tokens) == 0 {
		return true
	}
	lowerQ := strings.ToLower(question)
	for _, t := range tokens {
		if strings.Contains(lowerQ, strings.ToLower(t)) {
			return true
		}
	}
	return false
}
