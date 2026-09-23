package resume

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

// LLMExtractor 用大模型做结构化抽取, 并对抽取结果做原文定位校验。
//
// 大模型擅长"读懂简历", 但会顺手改写措辞、甚至补写原文没有的内容。
// 所以抽取之后必须把每个实体回原文核对: 找不到的就丢弃并告警,
// 而不是照单全收 —— 这正是评分器里"证据反查"的同一条铁律。
type LLMExtractor struct {
	client  *llm.Client
	timeout time.Duration
	retries int
}

// NewLLMExtractor 构造大模型抽取器。
func NewLLMExtractor(client *llm.Client) *LLMExtractor {
	return &LLMExtractor{client: client, timeout: 30 * time.Second, retries: 1}
}

var extractSpec = llm.ObjectSpec{Fields: []llm.FieldSpec{
	{Name: "skills", Kind: llm.KindArray, MaxItems: 40},
	{Name: "projects", Kind: llm.KindArray, MaxItems: 20},
}}

const extractSystemPrompt = "你是简历结构化助手。请从候选人简历中抽取信息, 只输出一个 JSON 对象, 不要任何解释。\n\n" +
	"必须遵守:\n" +
	"1. skills 与项目 tech 里的每一项, 必须是简历原文中真实出现的原词, 不得改写、概括、补写。系统会逐项回原文核对, 找不到的会被丢弃。\n" +
	"2. projects 的 name 必须是原文里真实出现的项目名或职位名。\n" +
	"3. description 可以概括, time_range 必须是原文里的原样日期区间。\n\n" +
	"输出格式:\n" +
	"{\n" +
	"  \"skills\": [\"Go\", \"Redis\"],\n" +
	"  \"projects\": [{\"name\": \"IM 对话平台\", \"description\": \"负责 Redis 存储改造\", \"time_range\": \"2023.06 - 2024.03\", \"tech\": [\"Redis\", \"ZSet\"]}]\n" +
	"}"

// Extract 从原文抽取结构化实体, 并回原文定位偏移。
func (e *LLMExtractor) Extract(ctx context.Context, source string) (*Resume, error) {
	if e.client == nil {
		return nil, errors.New("resume: 未配置大模型客户端")
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	messages := []llm.Message{
		{Role: "system", Content: extractSystemPrompt},
		{Role: "user", Content: "简历原文:\n\"\"\"\n" + source + "\n\"\"\""},
	}

	var lastErr error
	for attempt := 0; attempt <= e.retries; attempt++ {
		resp, err := e.client.Chat(ctx, messages)
		if err != nil {
			lastErr = err
			break
		}
		obj, err := llm.ValidateObject([]byte(resp.Text), extractSpec)
		if err != nil {
			lastErr = err
			messages = append(messages,
				llm.Message{Role: "assistant", Content: resp.Text},
				llm.Message{Role: "user", Content: "输出不符合要求: " + err.Error() + "。请重新只输出 JSON。"},
			)
			continue
		}

		r := Parse(source)
		e.populate(r, obj)
		r.crossValidate()
		return r, nil
	}
	return nil, fmt.Errorf("resume: 大模型抽取失败: %v", lastErr)
}

func (e *LLMExtractor) populate(r *Resume, obj map[string]any) {
	lower := strings.ToLower(r.Source)

	for _, skill := range llm.GetStrings(obj, "skills") {
		if idx := runeIndex(lower, strings.ToLower(skill)); idx >= 0 {
			r.Entities = append(r.Entities, Entity{
				Kind:  KindSkill,
				Value: skill,
				Start: idx,
				End:   idx + utf8.RuneCountInString(skill),
			})
			continue
		}
		r.Warnings = append(r.Warnings,
			"技能「"+skill+"」无法在原文定位, 已丢弃(可能是模型改写或补写)")
	}

	for _, p := range llm.GetObjects(obj, "projects") {
		name := strings.TrimSpace(llm.GetString(p, "name"))
		desc := strings.TrimSpace(llm.GetString(p, "description"))
		tr := strings.TrimSpace(llm.GetString(p, "time_range"))

		if name == "" {
			continue
		}
		if idx := runeIndex(lower, strings.ToLower(name)); idx >= 0 {
			r.Entities = append(r.Entities, Entity{
				Kind:  KindProject,
				Value: name,
				Start: idx,
				End:   idx + utf8.RuneCountInString(name),
				Details: map[string]string{
					"description": desc,
					"time_range":  tr,
				},
			})
		} else {
			r.Warnings = append(r.Warnings,
				"项目「"+name+"」无法在原文定位, 已丢弃")
			continue
		}

		for _, tech := range llm.GetStrings(p, "tech") {
			if idx := runeIndex(lower, strings.ToLower(tech)); idx >= 0 {
				r.Entities = append(r.Entities, Entity{
					Kind:  KindSkill,
					Value: tech,
					Start: idx,
					End:   idx + utf8.RuneCountInString(tech),
				})
			}
		}
	}
}
