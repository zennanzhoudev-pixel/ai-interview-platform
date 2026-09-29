package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRolePermissions(t *testing.T) {
	admin := Principal{Role: RoleAdmin}
	for _, p := range []Permission{PermKeyAdmin, PermDataErase, PermScoreOverride, PermAuditRead} {
		if !admin.Can(p) {
			t.Errorf("admin 应拥有权限 %s", p)
		}
	}

	interviewer := Principal{Role: RoleInterviewer}
	if !interviewer.Can(PermScoreOverride) {
		t.Error("面试官应能人工改分")
	}
	if interviewer.Can(PermKeyAdmin) {
		t.Error("面试官不应能管理密钥")
	}
	if interviewer.Can(PermDataErase) {
		t.Error("面试官不应能删除候选人数据")
	}

	scheduler := Principal{Role: RoleScheduler}
	if !scheduler.Can(PermSessionCreate) {
		t.Error("ATS 系统账号应能创建面试")
	}
	if scheduler.Can(PermScoreOverride) {
		t.Error("ATS 系统账号不应能改分")
	}

	unknown := Principal{Role: Role("随便写的角色")}
	if unknown.Can(PermReportRead) {
		t.Error("未知角色不应拥有任何权限")
	}
}

func TestMemoryKeyStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	ks := NewMemoryKeyStore()

	raw, principal, err := ks.Add("tenant-a", "招聘系统", RoleScheduler)
	if err != nil {
		t.Fatalf("签发密钥失败: %v", err)
	}
	if !strings.HasPrefix(raw, "ik_") {
		t.Fatalf("密钥应有可识别前缀, 实际 %q", raw)
	}
	if principal.TenantID != "tenant-a" || principal.Role != RoleScheduler {
		t.Fatalf("主体信息错误: %+v", principal)
	}

	got, err := ks.Resolve(ctx, raw)
	if err != nil {
		t.Fatalf("解析密钥失败: %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("租户应来自密钥而不是调用方输入, 实际 %q", got.TenantID)
	}

	if _, err := ks.Resolve(ctx, "ik_不存在的密钥"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("无效密钥应返回 ErrInvalidKey, 实际 %v", err)
	}
	if _, err := ks.Resolve(ctx, "   "); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("空密钥应返回 ErrInvalidKey, 实际 %v", err)
	}

	if !ks.Revoke(principal.KeyID) {
		t.Fatal("吊销应成功")
	}
	if _, err := ks.Resolve(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("已吊销的密钥不应再可用, 实际 %v", err)
	}
}

func TestHashKeyNeverStoresPlaintext(t *testing.T) {
	raw := "ik_abcdef123456"
	h := HashKey(raw)
	if strings.Contains(h, "abcdef") {
		t.Fatal("哈希结果不应包含明文片段")
	}
	if HashKey(raw) != h {
		t.Fatal("哈希必须确定性")
	}
	if HashKey("ik_other") == h {
		t.Fatal("不同密钥不应产生相同哈希")
	}

	ks := NewMemoryKeyStore()
	rawKey, principal, _ := ks.Add("t1", "测试", RoleAdmin)
	for _, rec := range ks.List() {
		if rec.KeyHash == rawKey || strings.Contains(rec.KeyHash, rawKey) {
			t.Fatal("密钥库里不能出现明文")
		}
	}
	if len(ks.List()) != 1 || ks.List()[0].KeyID != principal.KeyID {
		t.Fatalf("密钥列表异常: %+v", ks.List())
	}
}

func TestSessionTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret-key")
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	want := SessionToken{
		SessionID: "s_123",
		TenantID:  "tenant-a",
		ExpiresAt: now.Add(2 * time.Hour),
	}

	raw, err := IssueSessionToken(secret, want)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	got, err := VerifySessionToken(secret, raw, now)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if got != want {
		t.Fatalf("令牌内容不一致: %+v", got)
	}
}

func TestSessionTokenRejectsTampering(t *testing.T) {
	secret := []byte("test-secret-key")
	now := time.Now()
	raw, _ := IssueSessionToken(secret, SessionToken{
		SessionID: "s_123", TenantID: "tenant-a", ExpiresAt: now.Add(time.Hour),
	})

	cases := map[string]string{
		"篡改载荷":  raw[:len(raw)-3] + "AAA",
		"篡改签名":  strings.SplitN(raw, ".", 2)[0] + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"缺少分隔符": strings.Replace(raw, ".", "", 1),
		"空串":    "",
	}
	for name, bad := range cases {
		if _, err := VerifySessionToken(secret, bad, now); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: 期望 ErrTokenInvalid, 实际 %v", name, err)
		}
	}

	// 换个密钥必须验不过: 否则任何人拿到令牌就能伪造。
	if _, err := VerifySessionToken([]byte("另一个密钥"), raw, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("不同密钥不应验签通过, 实际 %v", err)
	}
}

func TestSessionTokenExpiry(t *testing.T) {
	secret := []byte("s")
	issued := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	raw, _ := IssueSessionToken(secret, SessionToken{
		SessionID: "s_1", TenantID: "t1", ExpiresAt: issued.Add(time.Hour),
	})

	if _, err := VerifySessionToken(secret, raw, issued.Add(59*time.Minute)); err != nil {
		t.Fatalf("未过期不应报错: %v", err)
	}
	if _, err := VerifySessionToken(secret, raw, issued.Add(2*time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("过期应返回 ErrTokenExpired, 实际 %v", err)
	}
}

func TestIssueRequiresSecret(t *testing.T) {
	if _, err := IssueSessionToken(nil, SessionToken{SessionID: "s"}); err == nil {
		t.Fatal("缺签名密钥时必须拒绝签发")
	}
}
