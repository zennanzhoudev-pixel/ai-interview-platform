// Package resume 做简历解析与结构化抽取。
//
// 整个包围绕一条铁律: 抽出来的每一个实体都必须能定位回原文。
// "候选人说他用过 ZSet" 和"简历第 3 段第 12 个字起写着 ZSet"是两种
// 完全不同的证据强度 —— 面试官追问时, 只有后者能让问题精确到原文,
// 也只有后者才能防止模型编造。
package resume

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// EntityKind 是实体类型。
type EntityKind string

const (
	KindSkill    EntityKind = "skill"
	KindProject  EntityKind = "project"
	KindTimeline EntityKind = "timeline"
)

// Entity 是简历里的一个结构化事实, 带原文定位(Start/End 为 rune 偏移)。
type Entity struct {
	Kind    EntityKind        `json:"kind"`
	Value   string            `json:"value"`
	Start   int               `json:"start"`
	End     int               `json:"end"`
	Details map[string]string `json:"details,omitempty"`
}

// Block 是简历文本的一个段落/行, 带原文偏移。
type Block struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Resume 是一次解析的结果。
type Resume struct {
	Source   string   `json:"source"`
	Blocks   []Block  `json:"blocks"`
	Entities []Entity `json:"entities"`
	Warnings []string `json:"warnings"`
}

// Quote 返回某个实体在原文中的原话片段。
func (r *Resume) Quote(e Entity) string {
	if e.Start < 0 || e.End > utf8.RuneCountInString(r.Source) || e.Start >= e.End {
		return ""
	}
	rs := []rune(r.Source)
	return string(rs[e.Start:e.End])
}

// Parse 把简历文本切成块(按行), 并记录每个块在原文中的 rune 偏移。
func Parse(source string) *Resume {
	r := &Resume{Source: source}
	var offset int
	lines := strings.Split(source, "\n")
	for _, line := range lines {
		// 跳过行首行尾空白, 但偏移要基于原始文本, 所以先定位再裁剪
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			offset += len([]rune(line)) + 1
			continue
		}
		lead := len([]rune(line)) - len([]rune(strings.TrimLeft(line, " \t")))
		start := offset + lead
		r.Blocks = append(r.Blocks, Block{
			Index: len(r.Blocks),
			Text:  trimmed,
			Start: start,
			End:   start + utf8.RuneCountInString(trimmed),
		})
		offset += len([]rune(line)) + 1
	}
	return r
}

// 技术技能词典。这是规则抽取的靶子, 因此必须显式维护并接受评审,
// 而不是藏在正则里。
var skillDict = []string{
	"Go", "Golang", "gRPC", "Gin", "GORM", "Redis", "MySQL", "Kafka", "RabbitMQ",
	"Elasticsearch", "ES", "Docker", "Kubernetes", "K8s", "Prometheus", "Grafana",
	"Jaeger", "Airflow", "ZSet", "WebSocket", "Protobuf", "MCP", "Agent", "RAG",
	"LLM", "Python", "Java", "C++", "Rust", "Nginx", "微服务", "分布式", "消息队列",
	"限流", "幂等", "状态机", "分库分表", "缓存", "WASM", "Extism",
}

// 项目段落标记: 出现这些词的行, 大概率是在描述项目。
var projectMarkers = []string{"项目", "负责", "参与", "主导", "实现", "重构", "优化", "开发"}

// dateRangeRE 匹配时间区间, 例如 "2023.06 - 2024.03" 或 "2023年6月-2024年3月"。
var dateRangeRE = regexp.MustCompile(`(\d{4})[.\-/年](\d{1,2})[月]?\s*[-~至]\s*(\d{4})[.\-/年](\d{1,2})[月]?`)

// RuleExtractor 是确定性的规则抽取器。
//
// 它是简历解析的基线: 不需要模型、零成本、结果可解释、可回归。
// 生产环境应该用大模型抽取 + 本规则器做交叉校验与兜底, 两者各司其职。
type RuleExtractor struct{}

// NewRuleExtractor 构造规则抽取器。
func NewRuleExtractor() *RuleExtractor { return &RuleExtractor{} }

// Extract 从原文抽取技能、项目与时间线, 并做交叉校验。
func (e *RuleExtractor) Extract(source string) *Resume {
	r := Parse(source)
	lower := strings.ToLower(source)

	for _, skill := range skillDict {
		needle := strings.ToLower(skill)
		from := 0
		for {
			idx := runeIndexFrom(lower, needle, from)
			if idx < 0 {
				break
			}
			r.Entities = append(r.Entities, Entity{
				Kind:  KindSkill,
				Value: skill,
				Start: idx,
				End:   idx + utf8.RuneCountInString(skill),
			})
			from = idx + 1
		}
	}

	for _, b := range r.Blocks {
		text := strings.TrimSpace(b.Text)
		if text == "" {
			continue
		}
		isProject := false
		for _, m := range projectMarkers {
			if strings.Contains(text, m) {
				isProject = true
				break
			}
		}
		if strings.HasPrefix(text, "-") || strings.HasPrefix(text, "·") || strings.HasPrefix(text, "•") {
			isProject = true
		}
		if !isProject {
			continue
		}

		details := map[string]string{}
		if m := dateRangeRE.FindStringSubmatch(text); len(m) >= 5 {
			details["time_range"] = m[0]
		}
		r.Entities = append(r.Entities, Entity{
			Kind:    KindProject,
			Value:   text,
			Start:   b.Start,
			End:     b.End,
			Details: details,
		})
	}

	for _, m := range dateRangeRE.FindAllStringSubmatchIndex(source, -1) {
		// regexp 返回的是字节偏移, 而整个模型用 rune 偏移定位原文。
		// 中文简历里这两个数字不同, 不转换会把原文定位错到别的字上。
		start := byteToRuneOffset(source, m[0])
		end := byteToRuneOffset(source, m[1])
		r.Entities = append(r.Entities, Entity{
			Kind:  KindTimeline,
			Value: source[m[0]:m[1]],
			Start: start,
			End:   end,
		})
	}

	r.crossValidate()
	return r
}

// crossValidate 做两条交叉校验:
//  1. 技能一致性: 出现在"技能"段落里、却在任何项目描述里都找不到的技能, 提示"只是罗列";
//  2. 时间线连续性: 起止时间倒挂的直接告警。
//
// 这些校验不是"锦上添花" —— 它们是结构化抽取能被信任的前提。
func (r *Resume) crossValidate() {
	projectText := ""
	for _, e := range r.Entities {
		if e.Kind == KindProject {
			projectText += e.Value + "\n"
		}
	}
	lowerProjects := strings.ToLower(projectText)

	seenSkill := make(map[string]bool)
	for _, e := range r.Entities {
		if e.Kind != KindSkill {
			continue
		}
		key := strings.ToLower(e.Value)
		if seenSkill[key] {
			continue
		}
		seenSkill[key] = true
		if !strings.Contains(lowerProjects, key) {
			r.Warnings = append(r.Warnings,
				"技能「"+e.Value+"」没有在任何项目描述中出现, 可能只是罗列")
		}
	}

	for _, e := range r.Entities {
		if e.Kind != KindTimeline {
			continue
		}
		sub := dateRangeRE.FindStringSubmatch(e.Value)
		if len(sub) >= 5 {
			startY, endY := sub[1], sub[3]
			if startY > endY {
				r.Warnings = append(r.Warnings,
					"时间区间倒挂:「"+e.Value+"」")
			}
		}
	}
}

// runeIndexFrom 从 from 起在 s 中查找 substr, 返回 rune 偏移。
func runeIndexFrom(s, substr string, from int) int {
	if from < 0 {
		from = 0
	}
	rs := []rune(s)
	sub := []rune(substr)
	if len(sub) == 0 || from+len(sub) > len(rs) {
		return -1
	}
	for i := from; i+len(sub) <= len(rs); i++ {
		match := true
		for j := range sub {
			if rs[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// runeIndex 在整个字符串中查找 substr 的 rune 偏移。
func runeIndex(s, substr string) int {
	return runeIndexFrom(s, substr, 0)
}

// byteToRuneOffset 把字节偏移换算成 rune 偏移。
func byteToRuneOffset(s string, byteIdx int) int {
	if byteIdx <= 0 {
		return 0
	}
	if byteIdx >= len(s) {
		return utf8.RuneCountInString(s)
	}
	return utf8.RuneCountInString(s[:byteIdx])
}
