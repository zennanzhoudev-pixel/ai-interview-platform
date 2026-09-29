package account

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 这些向量的期望值是由 Python 的 hashlib.pbkdf2_hmac(底层 OpenSSL)算出来的,
// 不是我自己实现算的 —— 用自己的实现验证自己的实现, 等于什么都没验证。
//
// 其中第一组(passwd/salt/1 轮)同时是 RFC 7914 §11 公开的测试向量,
// 因此它还能证明"我实现的确实是标准 PBKDF2", 而不是一个自洽的私货。
func TestPBKDF2SHA256MatchesReferenceVectors(t *testing.T) {
	cases := []struct {
		password string
		salt     []byte
		iter     int
		dkLen    int
		wantHex  string
	}{
		{"passwd", []byte("salt"), 1, 32,
			"55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc"},
		{"password", []byte("salt"), 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"密码123", []byte("abc"), 1000, 32,
			"561828e0c578af8316b2b77ed69952eea12d183185ca1cd59734155e0d9a8663"},
		{"correct horse battery staple", []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, 2048, 32,
			"ffdce16f0f85cd706a554964ebb48e58165c7414dab9ee966299a1bbe1be63a1"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(PBKDF2SHA256([]byte(c.password), c.salt, c.iter, c.dkLen))
		if got != c.wantHex {
			t.Errorf("PBKDF2(%q, iter=%d):\n  got  %s\n  want %s", c.password, c.iter, got, c.wantHex)
		}
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h := NewPasswordHasher(1000)
	encoded, err := h.Hash("s3cret-密码")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	// 哈希串必须自描述: 算法 + 参数 + 盐 + 派生密钥。
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$1000$") {
		t.Fatalf("哈希串应记录算法与参数, 实际 %q", encoded)
	}
	if strings.Contains(encoded, "s3cret") {
		t.Fatal("哈希串里绝不能出现明文密码")
	}

	ok, rehash := h.Verify(encoded, "s3cret-密码")
	if !ok {
		t.Fatal("正确密码应校验通过")
	}
	if rehash {
		t.Fatal("参数一致时不应要求重新哈希")
	}
	if ok, _ := h.Verify(encoded, "s3cret-密码!"); ok {
		t.Fatal("错误密码必须校验失败")
	}
	if ok, _ := h.Verify(encoded, ""); ok {
		t.Fatal("空密码必须校验失败")
	}
}

func TestPasswordHashUsesFreshSaltEveryTime(t *testing.T) {
	h := NewPasswordHasher(1000)
	a, _ := h.Hash("same-password")
	b, _ := h.Hash("same-password")
	if a == b {
		t.Fatal("同一密码两次哈希必须不同(每次都要新盐), 否则彩虹表/相同密码可被一眼看出")
	}
}

func TestVerifyReportsRehashNeededWhenParametersChanged(t *testing.T) {
	old := NewPasswordHasher(1000)
	encoded, _ := old.Hash("pw")
	upgraded := NewPasswordHasher(2000)
	ok, rehash := upgraded.Verify(encoded, "pw")
	if !ok {
		t.Fatal("参数变化后老哈希仍应能通过校验(否则用户会被迫改密码)")
	}
	if !rehash {
		t.Fatal("参数变化后应提示重新哈希, 以便登录时透明升级")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	h := NewPasswordHasher(1000)
	bad := []string{
		"",
		"plaintext",
		"pbkdf2-sha256$notanumber$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$$a2V5",
		"pbkdf2-sha256$1000$c2FsdA$",
		"bcrypt$1000$c2FsdA$a2V5",
	}
	for _, encoded := range bad {
		if ok, _ := h.Verify(encoded, "pw"); ok {
			t.Errorf("非法哈希串 %q 不应校验通过", encoded)
		}
	}
}

func TestRandomTokenAndHashAreStable(t *testing.T) {
	a, err := RandomToken(32)
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	b, _ := RandomToken(32)
	if a == b {
		t.Fatal("两次生成的令牌不应相同")
	}
	if len(a) < 40 {
		t.Fatalf("32 字节令牌的 URL 安全编码应足够长, 实际 %d", len(a))
	}
	if HashToken(a) != HashToken(a) {
		t.Fatal("令牌哈希必须稳定(否则每次校验都失败)")
	}
	if HashToken(a) == a {
		t.Fatal("存储形态不应等于明文令牌")
	}
	if HashToken(a) == HashToken(b) {
		t.Fatal("不同令牌不应得到相同哈希")
	}
}
