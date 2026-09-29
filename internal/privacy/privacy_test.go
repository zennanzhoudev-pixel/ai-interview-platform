package privacy

import (
	"strings"
	"testing"
)

func TestRefIsTenantScopedAndIrreversible(t *testing.T) {
	secret := []byte("server-secret")
	a := Ref(secret, "tenant-a", "13800138000")
	b := Ref(secret, "tenant-b", "13800138000")
	if a == b {
		t.Fatal("同一个候选人 ID 在不同租户下应得到不同引用值")
	}
	if a != Ref(secret, "tenant-a", "13800138000") {
		t.Fatal("引用值必须确定性")
	}
	if strings.Contains(a, "13800138000") {
		t.Fatal("引用值不能包含原文")
	}
	if Ref([]byte("other-secret"), "tenant-a", "13800138000") == a {
		t.Fatal("换密钥后引用值应改变")
	}
}

func TestMaskIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.57":     "203.0.113.x",
		"203.0.113.57:443": "203.0.113.x",
		"127.0.0.1":        "127.0.0.x",
		"":                 "",
		"不是IP":             "invalid",
	}
	for in, want := range cases {
		if got := MaskIP(in); got != want {
			t.Errorf("MaskIP(%q) = %q, 期望 %q", in, got, want)
		}
	}
	if got := MaskIP("2001:db8:85a3::8a2e:370:7334"); !strings.HasSuffix(got, "::/48") {
		t.Errorf("IPv6 应脱敏到 /48, 实际 %q", got)
	}
}

func TestMaskUserAgent(t *testing.T) {
	short := "Mozilla/5.0"
	if MaskUserAgent(short) != short {
		t.Fatal("短 UA 不应被改动")
	}
	long := strings.Repeat("x", 200)
	got := MaskUserAgent(long)
	if len([]rune(got)) != 81 {
		t.Fatalf("长 UA 应截断到 80 字符加省略号, 实际 %d", len([]rune(got)))
	}
}

func TestRedactName(t *testing.T) {
	cases := map[string]string{
		"陈雨":  "陈*",
		"欧阳修": "欧**",
		"陈":   "陈",
		"":    "候选人",
	}
	for in, want := range cases {
		if got := RedactName(in); got != want {
			t.Errorf("RedactName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
