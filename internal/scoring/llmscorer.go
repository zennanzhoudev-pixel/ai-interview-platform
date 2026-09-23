package scoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/llm"
)

// RubricLevel 描述一个等级的含义, 会被注入评分 prompt。
type RubricLevel struct {
	Level Level
	Desc  string
}

// Rubric 是一个能力项的评分标准。
//
// 它必须由人工评审产出, 而不是让模型现编。评分标准本身的不确定性
// 会直接变成招聘结论的不确定性, 这是企业级系统不能接受的部分。
type Rubric struct {
	Competency string
	Levels     []RubricLevel
}

// DefaultRubric 返回与设计方案一致的五级定义。
func DefaultRubric(competency string) Rubric {
	return Rubric{
		Competency: competency,
		Levels: []RubricLevel{
			{LevelNone, "无法描述基本概念"},
			{LevelAware, "知道概念, 但讲不出落地方式"},
			{LevelProficient, "能设计并说明权衡"},
			{LevelAdvanced, "能预判瓶颈, 给出容量估算或降级方案"},
			{LevelExpert, "有跨系统权衡经验, 能提出我们没想到的方案"},
		},
	}
}

// ScoringUsage 累计模型调用与 token 消耗, 直接对接成本看板。
type ScoringUsage struct {
	Calls            int64
	Failures         int64
	PromptTokens     int64
	CompletionTokens int64
}

// LLMScorer 是用大模型实现的能力评分器。
//
// 它的价值不在于"用了大模型", 而在于把三件事做成了硬约束:
//
//  1. 结构化输出: 只接受符合约定 schema 的 JSON, 其余一律重试或作废;
//  2. 证据反查: 模型引用的原话必须真的出现在候选人回答里, 编造的
//     "证据"会被逐条丢弃 —— 这是抗幻觉最关键的一道闸门;
//  3. 失败可降级: 任何环节失败都产出"无证据结果", 交给 ChainScorer
//     回落到规则评分, 而不是让这场面试没有分数。
type LLMScorer struct {
	client  *llm.Client
	rubrics map[string]Rubric
	retries int
	timeout time.Duration

	mu    sync.Mutex
	usage ScoringUsage
}

// LLMOption 配置 LLMScorer。
type LLMOption func(*LLMScorer)

// WithRubrics 设置各能力项的评分标准。
func WithRubrics(m map[string]Rubric) LLMOption {
	return func(s *LLMScorer) { s.rubrics = m }
}

// WithSchemaRetries 设置"输出不合规时再问一次"的次数。
func WithSchemaRetries(n int) LLMOption {
	return func(s *LLMScorer) {
		if n < 0 {
			n = 0
		}
		s.retries = n
	}
}

// WithScoreTimeout 设置单次评分的总超时。
func WithScoreTimeout(d time.Duration) LLMOption {
	return func(s *LLMScorer) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// NewLLMScorer 构造大模型评分器。
func NewLLMScorer(client *llm.Client, opts ...LLMOption) *LLMScorer {
	s := &LLMScorer{
		client:  client,
		retries: 1,
		timeout: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *LLMScorer) Name() string {
	if s.client == nil {
		return "llm(nil)"
	}
	return "llm:" + s.client.Model()
}

// Usage 返回累计消耗快照。
func (s *LLMScorer) Usage() ScoringUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// scoreSpec 是评分输出的 schema。
// 判定要点写在这里而不是散在校验代码里, 是为了让"系统接受什么样的输出"
// 这件事一眼可见、可评审。
var scoreSpec = llm.ObjectSpec{Fields: []llm.FieldSpec{
	{Name: "level", Kind: llm.KindNumber, Required: true, Int: true, Min: 1, Max: 5},
	{Name: "matched", Kind: llm.KindArray, MaxItems: 12},
	{Name: "missing", Kind: llm.KindArray, MaxItems: 12},
	{Name: "evidence", Kind: llm.KindArray, Required: true, MinItems: 1, MaxItems: 5},
	{Name: "confidence", Kind: llm.KindNumber, Min: 0, Max: 1},
	{Name: "rationale", Kind: llm.KindString, Required: true, MaxLen: 300},
}}

// Score 实现 Scorer。
//
// 这里的 context 是内部构造的: 评分发生在一次回答结束之后,
// 没有需要继承的实时取消链路, 只需要一个防止卡死的上限。
func (s *LLMScorer) Score(a Answer) Result {
	if s.client == nil {
		return Result{Model: "llm(nil)", Rationale: "未配置大模型客户端"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	return s.ScoreContext(ctx, a)
}

// ScoreContext 是带 context 的评分入口, 便于离线回放与批量评测。
func (s *LLMScorer) ScoreContext(ctx context.Context, a Answer) Result {
	messages := s.buildMessages(a)
	var lastErr error

	for attempt := 0; attempt <= s.retries; attempt++ {
		resp, err := s.client.Chat(ctx, messages)
		if err != nil {
			lastErr = err
			s.recordFailure()
			// 传输层错误不再消耗"schema 重试"预算: llm.Client 内部已经做过
			// 带退避的重试, 在这里再叠一层只会把延迟翻倍, 却几乎不会提高成功率。
			break
		}
		s.record(resp)

		res, err := s.parse(a, resp.Text)
		if err == nil {
			return res.Enforce()
		}
		lastErr = err

		// 把具体的违规点回灌给模型, 让它针对性修正。
		// 比"原样再问一遍"有效得多 —— 后者模型大概率会犯同样的错。
		messages = append(messages,
			llm.Message{Role: "assistant", Content: resp.Text},
			llm.Message{Role: "user", Content: fmt.Sprintf(
				"你的输出不符合要求: %s。请重新只输出一个修正后的 JSON 对象, 不要任何解释。", err.Error())},
		)
	}

	// 返回无证据结果: Enforce 会把等级作废, ChainScorer 据此触发降级。
	return Result{
		Model:     s.Name(),
		Rationale: "大模型评分失败: " + errText(lastErr),
	}
}

func (s *LLMScorer) buildMessages(a Answer) []llm.Message {
	rubric, ok := s.rubrics[a.Competency]
	if !ok {
		rubric = DefaultRubric(a.Competency)
	}

	var b strings.Builder
	if a.Competency != "" {
		b.WriteString("能力项: " + a.Competency + "\n")
	}
	b.WriteString("题目: " + a.Question + "\n")
	if len(a.Keywords) > 0 {
		b.WriteString("判定要点: " + strings.Join(a.Keywords, " / ") + "\n")
	}
	if len(a.AntiPatterns) > 0 {
		b.WriteString("断言反例(出现即判定为最低等级): " + strings.Join(a.AntiPatterns, " / ") + "\n")
	}
	b.WriteString("\n等级定义:\n")
	for _, l := range rubric.Levels {
		fmt.Fprintf(&b, "L%d %s: %s\n", l.Level.Number(), l.Level.Label(), l.Desc)
	}
	b.WriteString("\n候选人回答:\n\"\"\"\n" + a.Text + "\n\"\"\"\n")
	b.WriteString("\n请只输出一个 JSON 对象。")

	return []llm.Message{
		{Role: "system", Content: scoringSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

const scoringSystemPrompt = `你是一名资深技术面试官, 负责按给定的评分标准给候选人的回答打分。

必须遵守:
1. 只输出一个 JSON 对象, 不要输出任何解释文字, 不要包裹 markdown 代码块。
2. evidence 里必须引用候选人回答中的原话, 且必须是回答里真实出现的连续文字。
   不得改写、不得概括、不得编造 —— 系统会逐条回原文核对, 编造的会被丢弃。
3. 拿不到任何支撑证据时, 给最低等级, 并在 missing 里说明缺什么。
4. 表达流畅不能代替技术正确: 回答命中断言反例时, 直接给最低等级。
5. 不要因为候选人态度好、表达礼貌或使用了很多术语而加分。

输出格式:
{
  "level": 1 到 5 的整数,
  "matched": ["命中的判定要点"],
  "missing": ["没有覆盖的判定要点"],
  "evidence": [{"quote": "回答中的原话", "matched": "对应的判定要点"}],
  "confidence": 0 到 1 之间的小数,
  "rationale": "一句话说明评分理由"
}`

// parse 把模型输出解析成评分结果, 并执行两道本地硬校验:
// schema 合规, 以及证据必须能在原文中核实。
func (s *LLMScorer) parse(a Answer, raw string) (Result, error) {
	obj, err := llm.ValidateObject([]byte(raw), scoreSpec)
	if err != nil {
		return Result{}, err
	}

	levelNum, _ := llm.GetInt(obj, "level")
	res := Result{
		Model:     s.Name(),
		Level:     LevelFromNumber(levelNum),
		Matched:   llm.GetStrings(obj, "matched"),
		Missing:   llm.GetStrings(obj, "missing"),
		Rationale: llm.GetString(obj, "rationale"),
	}
	if c, ok := llm.GetFloat(obj, "confidence"); ok {
		res.Confidence = c
	}

	// 反例由本地再核对一遍: 不指望模型每次都记得"命中反例要降级"。
	for _, ap := range a.AntiPatterns {
		if strings.Contains(stripSpaces(a.Text), stripSpaces(ap)) {
			res.AntiHits = append(res.AntiHits, ap)
		}
	}
	if len(res.AntiHits) > 0 && res.Level > LevelNone {
		res.Level = LevelNone
		res.Rationale = "命中断言反例, 强制降级。" + res.Rationale
	}

	// 证据反查: 这是整套抗幻觉机制里最关键的一步。
	fabricated := 0
	tooShort := 0
	for _, e := range llm.GetObjects(obj, "evidence") {
		quote := strings.TrimSpace(llm.GetString(e, "quote"))
		if runeCount(quote) < 4 {
			tooShort++
			continue
		}
		if !quoteExists(a.Text, quote) {
			fabricated++
			continue
		}
		res.Evidence = append(res.Evidence, Evidence{
			QuestionID: a.QuestionID,
			Kind:       EvidenceSupport,
			Matched:    strings.TrimSpace(llm.GetString(e, "matched")),
			Quote:      quote,
		})
	}

	switch {
	case len(res.Evidence) == 0 && fabricated > 0:
		return Result{}, fmt.Errorf(
			"evidence 中引用的原话在候选人回答里找不到(共 %d 条), 属于编造证据", fabricated)
	case len(res.Evidence) == 0:
		return Result{}, errors.New("evidence 里必须引用候选人回答中的原话, 当前没有可核实的证据")
	}

	if fabricated > 0 || tooShort > 0 {
		// 部分证据不可核实: 保留可核实的那部分, 但显著降低置信度,
		// 并把这件事写进理由 —— 复核的面试官需要知道哪些依据是打折的。
		res.Confidence *= 0.5
		res.Rationale = fmt.Sprintf("%s(丢弃不可核实证据 %d 条, 过短证据 %d 条)",
			res.Rationale, fabricated, tooShort)
	}
	return res, nil
}

func (s *LLMScorer) record(resp llm.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.Calls++
	s.usage.PromptTokens += int64(resp.PromptTokens)
	s.usage.CompletionTokens += int64(resp.CompletionTokens)
}

func (s *LLMScorer) recordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.Failures++
}

func errText(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}

// quoteExists 判断模型引用的原话是否真的出现在候选人回答中。
//
// 比较前去掉全部空白字符: 中文回答里模型常常在标点后补一个空格,
// 这种差异不该被判定为编造。但去掉空白后仍然找不到, 就只能认为
// 这段"证据"是模型自己写出来的 —— 这正是我们要拦的东西。
func quoteExists(answer, quote string) bool {
	na := stripSpaces(answer)
	nq := stripSpaces(quote)
	if nq == "" {
		return false
	}
	return strings.Contains(na, nq)
}

func stripSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func runeCount(s string) int { return len([]rune(s)) }
