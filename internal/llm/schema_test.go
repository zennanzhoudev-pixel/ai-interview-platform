package llm

import (
	"strings"
	"testing"
)

var testSpec = ObjectSpec{Fields: []FieldSpec{
	{Name: "level", Kind: KindNumber, Required: true, Int: true, Min: 1, Max: 5},
	{Name: "matched", Kind: KindArray, MinItems: 1, MaxItems: 3},
	{Name: "confidence", Kind: KindNumber, Min: 0, Max: 1},
	{Name: "rationale", Kind: KindString, Required: true, MaxLen: 20},
	{Name: "verdict", Kind: KindString, Enum: []string{"pass", "fail"}},
}}

func TestValidateObjectAcceptsValidJSON(t *testing.T) {
	raw := []byte(`{"level":4,"matched":["ZSet"],"confidence":0.8,"rationale":"命中要点","verdict":"pass"}`)
	obj, err := ValidateObject(raw, testSpec)
	if err != nil {
		t.Fatalf("合法输出不应报错: %v", err)
	}
	if lv, ok := GetInt(obj, "level"); !ok || lv != 4 {
		t.Fatalf("level 解析错误: %v", obj["level"])
	}
	if got := GetStrings(obj, "matched"); len(got) != 1 || got[0] != "ZSet" {
		t.Fatalf("matched 解析错误: %v", got)
	}
	if f, ok := GetFloat(obj, "confidence"); !ok || f != 0.8 {
		t.Fatalf("confidence 解析错误: %v", obj["confidence"])
	}
}

func TestValidateObjectReportsActionableProblems(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"缺少必填字段", `{"matched":["a"],"rationale":"r"}`, `缺少必填字段 "level"`},
		{"等级超范围", `{"level":9,"rationale":"r"}`, "应在 [1, 5] 范围内"},
		{"等级非整数", `{"level":3.5,"rationale":"r"}`, "应为整数"},
		{"类型错误", `{"level":"高","rationale":"r"}`, "应为数值"},
		{"数组元素过少", `{"level":3,"matched":[],"rationale":"r"}`, "至少需要 1 个元素"},
		{"数组元素过多", `{"level":3,"matched":["a","b","c","d"],"rationale":"r"}`, "最多 3 个元素"},
		{"枚举越界", `{"level":3,"rationale":"r","verdict":"maybe"}`, "取值必须是"},
		{"字符串过长", `{"level":3,"rationale":"` + strings.Repeat("长", 30) + `"}`, "长度不能超过 20"},
		{"空字符串", `{"level":3,"rationale":"   "}`, "不能为空"},
		{"非法JSON", `level=3`, "不是合法 JSON 对象"},
		{"空输出", ``, "输出为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateObject([]byte(c.raw), testSpec)
			if err == nil {
				t.Fatalf("应判定为不合规: %s", c.raw)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应便于回灌给模型, 期望包含 %q, 实际 %q", c.want, err.Error())
			}
		})
	}
}

// 即使开了 JSON 模式, 模型偶尔仍会把结果包进 markdown 代码块。
// 直接兼容比为此重试一次更划算。
func TestValidateObjectStripsCodeFence(t *testing.T) {
	raw := []byte("```json\n{\"level\":5,\"matched\":[\"a\"],\"rationale\":\"ok\"}\n```")
	obj, err := ValidateObject(raw, testSpec)
	if err != nil {
		t.Fatalf("应兼容代码块包裹: %v", err)
	}
	if lv, _ := GetInt(obj, "level"); lv != 5 {
		t.Fatalf("level 应为 5, 实际 %v", obj["level"])
	}
}

func TestValidateObjectRejectsNonObject(t *testing.T) {
	if _, err := ValidateObject([]byte(`[1,2,3]`), testSpec); err == nil {
		t.Fatal("顶层不是对象时应报错")
	}
}

func TestGetHelpersAreDefensive(t *testing.T) {
	obj := map[string]any{"s": "x", "n": "not-a-number", "arr": []any{"a", "", 3}}
	if got := GetString(obj, "missing"); got != "" {
		t.Fatalf("缺失字段应返回空串, 实际 %q", got)
	}
	if _, ok := GetInt(obj, "n"); ok {
		t.Fatal("非数值字段不应被判为整数")
	}
	if _, ok := GetFloat(obj, "missing"); ok {
		t.Fatal("缺失字段不应返回数值")
	}
	// 数组里的空串与非字符串元素应被丢弃, 而不是污染后续逻辑
	if got := GetStrings(obj, "arr"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("字符串数组应过滤脏数据, 实际 %v", got)
	}
	if got := GetObjects(obj, "arr"); len(got) != 0 {
		t.Fatalf("对象数组对非对象元素应返回空, 实际 %v", got)
	}
}
