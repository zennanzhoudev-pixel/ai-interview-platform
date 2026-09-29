package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
)

// accountContract 是账号存储的行为契约: 内存实现与 MySQL 实现必须同时满足。
//
// 为什么值得单独写一套: 账号是最容易"本地能跑、线上不能跑"的一块 ——
// 唯一键、NULL 语义、RowsAffected 的含义在不同后端上都有差异, 而它们的
// 表现都是"注册了却登不上"或"同一个人有两个账号", 排查成本极高。
//
// 契约里特意盯住几处 MySQL 特有的坑:
//  1. 只填手机号的多个用户不能互相冲突 —— 邮箱必须存 NULL 而不是空串;
//  2. 更新"值没变"的记录仍要成功 —— RowsAffected 为 0 不代表记录不存在;
//  3. 人脸向量必须逐位往返一致 —— 浮点丢精度会让本人第二天就登不上。
func accountContract(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	t.Run("账号在本租户内唯一且跨租户隔离", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		u := User{
			ID: "u_1", TenantID: "tenant-a", Name: "陈雨",
			Email: "chen@example.com", Phone: "+8613800138000",
			Role: auth.RoleCandidate, PasswordHash: "hash", Status: "active", CreatedAt: base,
		}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("创建账号失败: %v", err)
		}
		dup := u
		dup.ID = "u_2"
		if err := s.CreateUser(ctx, dup); !errors.Is(err, ErrUserExists) {
			t.Fatalf("重复邮箱应被拒绝, 实际 %v", err)
		}
		dup = u
		dup.ID = "u_3"
		dup.Email = "other@example.com"
		if err := s.CreateUser(ctx, dup); !errors.Is(err, ErrUserExists) {
			t.Fatalf("重复手机号应被拒绝, 实际 %v", err)
		}
		// 同一个手机号在两家公司都可能是候选人, 跨租户不应冲突。
		other := u
		other.ID = "u_4"
		other.TenantID = "tenant-b"
		if err := s.CreateUser(ctx, other); err != nil {
			t.Fatalf("跨租户应允许相同联系方式: %v", err)
		}
		if _, err := s.GetUser(ctx, "tenant-b", "u_1"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("跨租户读取应 ErrUserNotFound, 实际 %v", err)
		}
	})

	t.Run("只填手机号的多个账号互不冲突", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		for i, phone := range []string{"+8613800138001", "+8613800138002", "+8613800138003"} {
			u := User{
				ID: "u_p" + string(rune('a'+i)), TenantID: "tenant-a", Name: "无邮箱用户",
				Phone: phone, Role: auth.RoleCandidate, PasswordHash: "hash",
				Status: "active", CreatedAt: base,
			}
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatalf("第 %d 个只填手机号的账号创建失败: %v", i+1, err)
			}
		}
		list, err := s.ListUsers(ctx, "tenant-a", 10)
		if err != nil {
			t.Fatalf("列出账号失败: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("应有 3 个账号, 实际 %d", len(list))
		}
	})

	t.Run("按邮箱或手机号查找", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		u := User{
			ID: "u_1", TenantID: "tenant-a", Name: "陈雨", Email: "chen@example.com",
			Phone: "+8613800138000", Role: auth.RoleCandidate, PasswordHash: "hash",
			Status: "active", CreatedAt: base,
		}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("创建账号失败: %v", err)
		}
		for _, identifier := range []string{
			"chen@example.com", " CHEN@Example.com ", "13800138000", "+86 138 0013 8000",
		} {
			found, err := s.FindUser(ctx, "tenant-a", identifier)
			if err != nil {
				t.Fatalf("按 %q 查找失败: %v", identifier, err)
			}
			if found.ID != "u_1" {
				t.Fatalf("按 %q 查到了错误的账号: %s", identifier, found.ID)
			}
		}
		if _, err := s.FindUser(ctx, "tenant-a", "nobody@example.com"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("不存在的账号应 ErrUserNotFound, 实际 %v", err)
		}
	})

	t.Run("更新账号并保持唯一约束", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		a := User{ID: "u_a", TenantID: "tenant-a", Name: "甲", Email: "a@example.com",
			Role: auth.RoleCandidate, PasswordHash: "h1", Status: "active", CreatedAt: base}
		b := User{ID: "u_b", TenantID: "tenant-a", Name: "乙", Email: "b@example.com",
			Role: auth.RoleCandidate, PasswordHash: "h2", Status: "active", CreatedAt: base}
		for _, u := range []User{a, b} {
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatalf("创建账号失败: %v", err)
			}
		}
		b.Email = "a@example.com"
		if err := s.UpdateUser(ctx, b); !errors.Is(err, ErrUserExists) {
			t.Fatalf("改成已存在的邮箱应被拒绝, 实际 %v", err)
		}
		// 值完全没变也要成功: MySQL 的 RowsAffected 会是 0, 但不代表记录不存在。
		b.Email = "b@example.com"
		if err := s.UpdateUser(ctx, b); err != nil {
			t.Fatalf("更新为相同值应成功, 实际 %v", err)
		}
		b.FaceEnrolled = true
		b.Status = "disabled"
		b.LastLoginAt = base.Add(time.Hour)
		if err := s.UpdateUser(ctx, b); err != nil {
			t.Fatalf("更新账号失败: %v", err)
		}
		got, err := s.GetUser(ctx, "tenant-a", "u_b")
		if err != nil {
			t.Fatalf("读取账号失败: %v", err)
		}
		if !got.FaceEnrolled || got.Status != "disabled" || got.LastLoginAt.IsZero() {
			t.Fatalf("字段未持久化: %+v", got)
		}
		if err := s.UpdateUser(ctx, User{ID: "nope", TenantID: "tenant-a"}); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("更新不存在的账号应 ErrUserNotFound, 实际 %v", err)
		}
	})

	t.Run("人脸模板的读写与删除", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		u := User{ID: "u_1", TenantID: "tenant-a", Name: "陈雨", Email: "chen@example.com",
			Role: auth.RoleCandidate, PasswordHash: "h", Status: "active", CreatedAt: base}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("创建账号失败: %v", err)
		}
		if _, err := s.GetFace(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrFaceNotEnrolled) {
			t.Fatalf("未录入时应 ErrFaceNotEnrolled, 实际 %v", err)
		}
		vec := []float32{0.1, -0.2, 0.3, 0.4}
		profile := FaceProfile{
			TenantID: "tenant-a", UserID: "u_1", Template: vec, Matcher: "test-matcher",
			Assurance: "development-only", Quality: 0.9, Frames: 3, EnrolledAt: base,
		}
		if err := s.PutFace(ctx, profile); err != nil {
			t.Fatalf("保存人脸模板失败: %v", err)
		}
		got, err := s.GetFace(ctx, "tenant-a", "u_1")
		if err != nil {
			t.Fatalf("读取人脸模板失败: %v", err)
		}
		if len(got.Template) != len(vec) {
			t.Fatalf("维度不一致: %d vs %d", len(got.Template), len(vec))
		}
		for i := range vec {
			if got.Template[i] != vec[i] {
				t.Fatalf("第 %d 维不一致: %v vs %v", i, got.Template[i], vec[i])
			}
		}
		if got.Matcher != "test-matcher" || got.Assurance != "development-only" || got.Frames != 3 {
			t.Fatalf("模板元信息丢失: %+v", got)
		}
		if _, err := s.GetFace(ctx, "tenant-b", "u_1"); !errors.Is(err, ErrFaceNotEnrolled) {
			t.Fatalf("跨租户读取人脸模板应失败, 实际 %v", err)
		}
		profile.Template = []float32{0.5, 0.5}
		profile.Frames = 1
		if err := s.PutFace(ctx, profile); err != nil {
			t.Fatalf("覆盖人脸模板失败: %v", err)
		}
		got, _ = s.GetFace(ctx, "tenant-a", "u_1")
		if len(got.Template) != 2 || got.Frames != 1 {
			t.Fatalf("覆盖后应为新模板: %+v", got)
		}
		if err := s.DeleteFace(ctx, "tenant-a", "u_1"); err != nil {
			t.Fatalf("删除人脸模板失败: %v", err)
		}
		if _, err := s.GetFace(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrFaceNotEnrolled) {
			t.Fatalf("删除后应读不到, 实际 %v", err)
		}
		if err := s.DeleteFace(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrFaceNotEnrolled) {
			t.Fatalf("重复删除应 ErrFaceNotEnrolled, 实际 %v", err)
		}
	})

	t.Run("登录会话可读可撤且按过期时间失效", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		u := User{ID: "u_1", TenantID: "tenant-a", Name: "陈雨", Email: "chen@example.com",
			Role: auth.RoleCandidate, PasswordHash: "h", Status: "active", CreatedAt: base}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("创建账号失败: %v", err)
		}
		session := LoginSession{
			TokenHash: HashToken("token-1"), TenantID: "tenant-a", UserID: "u_1",
			Role: auth.RoleCandidate, IP: "203.0.113.x", UserAgent: "Chrome",
			// 过期判定用真实时钟(MySQL 实现用的是 time.Now), 因此这里也必须
			// 相对"现在"来构造, 否则换一个后端就会出现"刚建的会话已过期"。
			CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
		if err := s.CreateLoginSession(ctx, session); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}
		got, err := s.GetLoginSession(ctx, session.TokenHash)
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		if got.UserID != "u_1" || got.Role != auth.RoleCandidate || got.TenantID != "tenant-a" {
			t.Fatalf("会话字段丢失: %+v", got)
		}
		if got.Expired(base) {
			t.Fatal("未到期的会话不应被判为过期")
		}
		// 过期会话读出来必须失败, 否则"会话过期"只是文档里的话。
		expired := session
		expired.TokenHash = HashToken("token-expired")
		expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
		if err := s.CreateLoginSession(ctx, expired); err != nil {
			t.Fatalf("创建过期会话失败: %v", err)
		}
		if _, err := s.GetLoginSession(ctx, expired.TokenHash); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("过期会话应 ErrSessionExpired, 实际 %v", err)
		}
		second := session
		second.TokenHash = HashToken("token-2")
		if err := s.CreateLoginSession(ctx, second); err != nil {
			t.Fatalf("创建第二个会话失败: %v", err)
		}
		if err := s.DeleteUserSessions(ctx, "tenant-a", "u_1"); err != nil {
			t.Fatalf("撤销会话失败: %v", err)
		}
		for _, tk := range []string{session.TokenHash, second.TokenHash} {
			if _, err := s.GetLoginSession(ctx, tk); !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("撤销后会话应失效, 实际 %v", err)
			}
		}
		third := session
		third.TokenHash = HashToken("token-3")
		if err := s.CreateLoginSession(ctx, third); err != nil {
			t.Fatalf("创建第三个会话失败: %v", err)
		}
		if err := s.DeleteLoginSession(ctx, third.TokenHash); err != nil {
			t.Fatalf("退出登录失败: %v", err)
		}
		if _, err := s.GetLoginSession(ctx, third.TokenHash); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("退出后应失效, 实际 %v", err)
		}
	})

	t.Run("删除账号会连带清掉人脸与会话", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		u := User{ID: "u_1", TenantID: "tenant-a", Name: "陈雨", Email: "chen@example.com",
			Role: auth.RoleCandidate, PasswordHash: "h", Status: "active", CreatedAt: base}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("创建账号失败: %v", err)
		}
		if err := s.PutFace(ctx, FaceProfile{TenantID: "tenant-a", UserID: "u_1",
			Template: []float32{1, 0}, Matcher: "m", EnrolledAt: base}); err != nil {
			t.Fatalf("保存人脸失败: %v", err)
		}
		if err := s.CreateLoginSession(ctx, LoginSession{TokenHash: HashToken("t"),
			TenantID: "tenant-a", UserID: "u_1", Role: auth.RoleCandidate,
			CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}
		if err := s.DeleteUser(ctx, "tenant-a", "u_1"); err != nil {
			t.Fatalf("删除账号失败: %v", err)
		}
		if _, err := s.GetUser(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("删除后应读不到账号, 实际 %v", err)
		}
		// 数据主体权利要求"删除即真的删除": 人脸模板与会话不能留在库里。
		if _, err := s.GetFace(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrFaceNotEnrolled) {
			t.Fatalf("删除账号后人脸模板也应清掉, 实际 %v", err)
		}
		if _, err := s.GetLoginSession(ctx, HashToken("t")); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("删除账号后会话也应失效, 实际 %v", err)
		}
		if err := s.DeleteUser(ctx, "tenant-a", "u_1"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("重复删除应 ErrUserNotFound, 实际 %v", err)
		}
	})

	t.Run("服务层在任意存储上都能完成注册到登录", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		svc := New(Config{
			Store: s, TenantID: "tenant-a", AdminInviteCode: "HR-1",
			Secret: []byte("secret"), Hasher: NewPasswordHasher(1000),
		})
		u, err := svc.Register(ctx, RegisterInput{
			Name: "王琳", Email: "hr@example.com", Password: "面试用的密码123",
			Role: auth.RoleAdmin, InviteCode: "HR-1",
		})
		if err != nil {
			t.Fatalf("注册失败: %v", err)
		}
		if !u.Staff() {
			t.Fatal("管理员应被判定为企业成员")
		}
		_, session, err := svc.Login(ctx, LoginInput{
			Identifier: "hr@example.com", Password: "面试用的密码123",
		})
		if err != nil {
			t.Fatalf("登录失败: %v", err)
		}
		me, _, err := svc.Authenticate(ctx, session.Token)
		if err != nil {
			t.Fatalf("令牌解析失败: %v", err)
		}
		if me.ID != u.ID || me.Role != auth.RoleAdmin {
			t.Fatalf("身份不正确: %+v", me)
		}
	})
}

// TestMemoryStoreSatisfiesAccountContract 让内存实现跑一遍契约。
func TestMemoryStoreSatisfiesAccountContract(t *testing.T) {
	accountContract(t, func(t *testing.T) Store { return NewMemoryStore() })
}
