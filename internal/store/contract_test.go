package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// storeContract 是所有 SessionStore 实现都必须满足的行为契约。
//
// "本地用内存跑通, 线上换 MySQL" 这句话如果只靠人自觉, 迟早会因为
// 某个实现少了一条约束而出现语义差异, 而且这种差异通常要等到
// 数据错乱才被发现。把它写成可执行用例, 新增后端时直接复用。
func storeContract(t *testing.T, newStore func(t *testing.T) SessionStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	t.Run("创建并读取会话", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.CreateSession(ctx, Session{
			ID: "s1", TenantID: "t1", Round: 1, Minutes: 45,
			Stage: "GREETING", CreatedAt: base,
		}); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}

		got, err := s.GetSession(ctx, "s1")
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		if got.Round != 1 || got.Minutes != 45 || got.Stage != "GREETING" {
			t.Fatalf("会话字段丢失: %+v", got)
		}
		if got.Status != StatusRunning {
			t.Fatalf("未指定状态时应默认为 running, 实际 %q", got.Status)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("时间字段应自动填充: %+v", got)
		}
	})

	t.Run("重复创建同一会话应报错", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		sess := Session{ID: "dup", TenantID: "t1", Round: 1, CreatedAt: base}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("首次创建失败: %v", err)
		}
		if err := s.CreateSession(ctx, sess); err == nil {
			t.Fatal("重复创建应报错, 而不是静默覆盖已有会话")
		}
	})

	t.Run("空会话ID应被拒绝", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		if err := s.CreateSession(ctx, Session{Round: 1}); err == nil {
			t.Fatal("空会话 ID 应被拒绝")
		}
	})

	t.Run("读取不存在的会话返回 ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("更新会话与不存在时的错误", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		if err := s.CreateSession(ctx, Session{ID: "s2", TenantID: "t1", CreatedAt: base}); err != nil {
			t.Fatalf("创建失败: %v", err)
		}

		updated := Session{
			ID: "s2", TenantID: "t1", Stage: "SCORING",
			Status: StatusFinished, Recommendation: "HIRE",
		}
		if err := s.UpdateSession(ctx, updated); err != nil {
			t.Fatalf("更新失败: %v", err)
		}
		got, _ := s.GetSession(ctx, "s2")
		if got.Stage != "SCORING" || got.Status != StatusFinished || got.Recommendation != "HIRE" {
			t.Fatalf("更新未生效: %+v", got)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("更新不应丢失创建时间")
		}

		if err := s.UpdateSession(ctx, Session{ID: "ghost"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在的会话应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("列表倒序并支持租户过滤和条数上限", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		_ = s.CreateSession(ctx, Session{ID: "a1", TenantID: "t1", CreatedAt: base})
		_ = s.CreateSession(ctx, Session{ID: "a2", TenantID: "t1", CreatedAt: base.Add(time.Minute)})
		_ = s.CreateSession(ctx, Session{ID: "b1", TenantID: "t2", CreatedAt: base.Add(2 * time.Minute)})

		all, err := s.ListSessions(ctx, "", 10)
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(all) != 3 || all[0].ID != "b1" {
			t.Fatalf("应按创建时间倒序返回全部会话, 实际 %+v", ids(all))
		}

		t1, _ := s.ListSessions(ctx, "t1", 10)
		if len(t1) != 2 || t1[0].ID != "a2" {
			t.Fatalf("租户过滤错误: %+v", ids(t1))
		}

		limited, _ := s.ListSessions(ctx, "", 2)
		if len(limited) != 2 {
			t.Fatalf("条数上限未生效, 实际 %d 条", len(limited))
		}
	})

	t.Run("追加与读取问答", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		_ = s.CreateSession(ctx, Session{ID: "s3", TenantID: "t1", CreatedAt: base})

		for i := 1; i <= 3; i++ {
			if err := s.AppendTurn(ctx, Turn{
				SessionID: "s3", Index: i, Stage: "TECH_FUNDAMENTAL",
				QuestionID: "q1", Question: "问题", Answer: "回答",
				DurationMS: 90000, Scored: true, Level: "L3 熟练", LevelNum: 3, Confidence: 0.8,
				Evidence: []byte(`[{"quote":"原话"}]`),
			}); err != nil {
				t.Fatalf("追加第 %d 轮失败: %v", i, err)
			}
		}

		turns, err := s.ListTurns(ctx, "s3")
		if err != nil {
			t.Fatalf("读取问答失败: %v", err)
		}
		if len(turns) != 3 {
			t.Fatalf("应有 3 轮问答, 实际 %d", len(turns))
		}
		for i, tn := range turns {
			if tn.Index != i+1 {
				t.Fatalf("问答应按序号排序: 第 %d 位是 %d", i, tn.Index)
			}
		}
		if turns[0].LevelNum != 3 || turns[0].Confidence != 0.8 {
			t.Fatalf("评分字段丢失: %+v", turns[0])
		}
		if string(turns[0].Evidence) != `[{"quote":"原话"}]` {
			t.Fatalf("证据 JSON 丢失: %s", turns[0].Evidence)
		}
	})

	// 消息队列至少一次投递是常态, 重复写入必须无副作用。
	t.Run("重复投递同一轮问答必须幂等", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		_ = s.CreateSession(ctx, Session{ID: "s4", TenantID: "t1", CreatedAt: base})

		turn := Turn{SessionID: "s4", Index: 1, Question: "q", Answer: "a", CreatedAt: base}
		if err := s.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("首次追加失败: %v", err)
		}
		if err := s.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("重复追加不应报错: %v", err)
		}

		turns, _ := s.ListTurns(ctx, "s4")
		if len(turns) != 1 {
			t.Fatalf("重复投递不应产生第二条记录, 实际 %d 条", len(turns))
		}
	})

	t.Run("报告的保存与读取", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		_ = s.CreateSession(ctx, Session{ID: "s5", TenantID: "t1", CreatedAt: base})

		payload := []byte(`{"recommendation":"HIRE","dimensions":[]}`)
		if err := s.SaveReport(ctx, Report{
			SessionID: "s5", Recommendation: "HIRE", Confidence: 0.82, Payload: payload,
		}); err != nil {
			t.Fatalf("保存报告失败: %v", err)
		}

		got, err := s.GetReport(ctx, "s5")
		if err != nil {
			t.Fatalf("读取报告失败: %v", err)
		}
		if got.Recommendation != "HIRE" || got.Confidence != 0.82 {
			t.Fatalf("报告字段丢失: %+v", got)
		}
		if string(got.Payload) != string(payload) {
			t.Fatalf("报告原文必须可完整复现, 实际 %s", got.Payload)
		}

		if _, err := s.GetReport(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的报告应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("授权记录幂等且可追溯", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		_ = s.CreateSession(ctx, Session{ID: "s6", TenantID: "t1", CreatedAt: base})

		c := Consent{
			SessionID: "s6", CandidateID: "c1", Scope: "recording",
			AgreedAt: base, IP: "10.0.0.1", UserAgent: "Mozilla/5.0",
		}
		if err := s.SaveConsent(ctx, c); err != nil {
			t.Fatalf("保存授权失败: %v", err)
		}
		// 重复授权: 保留最早一条, 时间先后本身有法律意义。
		if err := s.SaveConsent(ctx, Consent{
			SessionID: "s6", CandidateID: "c1", Scope: "recording",
			AgreedAt: base.Add(time.Hour),
		}); err != nil {
			t.Fatalf("重复授权不应报错: %v", err)
		}

		list, err := s.ListConsents(ctx, "s6")
		if err != nil {
			t.Fatalf("读取授权失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("同一范围的授权应只保留一条, 实际 %d 条", len(list))
		}
		if !list[0].AgreedAt.Equal(base) {
			t.Fatalf("应保留最早一次授权时间, 实际 %v", list[0].AgreedAt)
		}
		if list[0].IP != "10.0.0.1" || list[0].UserAgent == "" {
			t.Fatalf("授权留痕不完整: %+v", list[0])
		}
	})
}

func ids(list []Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}
