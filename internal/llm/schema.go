package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Kind 是字段的类型。
type Kind string

const (
	KindString Kind = "string"
	KindNumber Kind = "number"
	KindBool   Kind = "bool"
	KindArray  Kind = "array"
	KindObject Kind = "object"
)

// FieldSpec 描述一个字段的约束。
type FieldSpec struct {
	Name       string
	Kind       Kind
	Required   bool
	Enum       []string
	Int        bool
	Min, Max   float64
	MinItems   int
	MaxItems   int
	MaxLen     int
	AllowEmpty bool
}

// ObjectSpec 描述一个 JSON 对象的约束。
type ObjectSpec struct {
	Fields []FieldSpec
}

// ValidateObject 校验 raw 是否为满足 spec 的 JSON 对象。
//
// 返回的错误信息是刻意写成"可以直接回灌给模型"的形式 ——
// 大模型输出不合规时, 与其重试同一段 prompt 指望它这次听话,
// 不如把具体的违规点告诉它再让它改一次, 成功率完全不同。
func ValidateObject(raw []byte, spec ObjectSpec) (map[string]any, error) {
	text := stripCodeFence(strings.TrimSpace(string(raw)))
	if text == "" {
		return nil, errors.New("输出为空")
	}

	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("输出不是合法 JSON 对象: %v", err)
	}

	var problems []string
	for _, f := range spec.Fields {
		v, ok := obj[f.Name]
		if !ok {
			if f.Required {
				problems = append(problems, fmt.Sprintf("缺少必填字段 %q", f.Name))
			}
			continue
		}
		if err := checkField(f, v); err != nil {
			problems = append(problems, fmt.Sprintf("字段 %q %v", f.Name, err))
		}
	}
	if len(problems) > 0 {
		return obj, errors.New(strings.Join(problems, "; "))
	}
	return obj, nil
}

func checkField(f FieldSpec, v any) error {
	switch f.Kind {
	case KindString:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("应为字符串, 实际是 %T", v)
		}
		if !f.AllowEmpty && strings.TrimSpace(s) == "" {
			return errors.New("不能为空")
		}
		if f.MaxLen > 0 && len([]rune(s)) > f.MaxLen {
			return fmt.Errorf("长度不能超过 %d 个字符", f.MaxLen)
		}
		if len(f.Enum) > 0 && !containsString(f.Enum, s) {
			return fmt.Errorf("取值必须是 %v 之一, 实际是 %q", f.Enum, s)
		}
	case KindNumber:
		n, err := toFloat(v)
		if err != nil {
			return err
		}
		if f.Int && n != math.Trunc(n) {
			return fmt.Errorf("应为整数, 实际是 %v", n)
		}
		if (f.Min != 0 || f.Max != 0) && (n < f.Min || n > f.Max) {
			return fmt.Errorf("应在 [%v, %v] 范围内, 实际是 %v", f.Min, f.Max, n)
		}
	case KindArray:
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("应为数组, 实际是 %T", v)
		}
		if f.MinItems > 0 && len(arr) < f.MinItems {
			return fmt.Errorf("至少需要 %d 个元素, 实际 %d 个", f.MinItems, len(arr))
		}
		if f.MaxItems > 0 && len(arr) > f.MaxItems {
			return fmt.Errorf("最多 %d 个元素, 实际 %d 个", f.MaxItems, len(arr))
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("应为布尔值, 实际是 %T", v)
		}
	case KindObject:
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("应为对象, 实际是 %T", v)
		}
	}
	return nil
}

// GetString 读取字符串字段。
func GetString(obj map[string]any, key string) string {
	if s, ok := obj[key].(string); ok {
		return s
	}
	return ""
}

// GetInt 读取整数字段。
func GetInt(obj map[string]any, key string) (int, bool) {
	f, err := toFloat(obj[key])
	if err != nil {
		return 0, false
	}
	return int(f), true
}

// GetFloat 读取浮点字段。
func GetFloat(obj map[string]any, key string) (float64, bool) {
	f, err := toFloat(obj[key])
	if err != nil {
		return 0, false
	}
	return f, true
}

// GetStrings 读取字符串数组字段, 顺带丢弃空串。
func GetStrings(obj map[string]any, key string) []string {
	arr, ok := obj[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// GetObjects 读取对象数组字段。
func GetObjects(obj map[string]any, key string) []map[string]any {
	arr, ok := obj[key].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Float64()
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("应为数值, 实际是 %T", v)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// stripCodeFence 剥离 markdown 代码块包裹。
//
// 即使开了 JSON 模式, 模型偶尔仍会把结果包在 ```json ... ``` 里。
// 与其为此重试一次(浪费一次调用和几秒延迟), 不如直接兼容这种写法。
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
