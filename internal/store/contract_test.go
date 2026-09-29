package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// storeContract 是所有 SessionStore 实现都必须满足的行为契约。
//
// "本地用内存跑通, 线上换 MySQL" 这句话如果只靠人自觉, 迟早会因为
// 某个实现少了一条约束而出现语义差异 —— 而且这种差异通常要等到
// 数据错乱或安全事件才被发现。把它写成可执行用例, 新增后端时直接复用。
func storeContract(t *testing.T, newStore func(t *testing.T) SessionStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	t.Run("创建并读取会话", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.CreateSession(ctx, Session{
			ID: "s1", TenantID: tenantA, Round: 1, Minutes: 45,
			Stage: "GREETING", CreatedAt: base,
			Position: "高级后端工程师", Company: "云杉科技",
			CandidateName: "陈雨", CandidateRef: "cand_aaa", InterviewerName: "林澈",
			ResumeJSON: []byte(`{"entities":[]}`),
		}); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}

		got, err := s.GetSession(ctx, tenantA, "s1")
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		if got.Round != 1 || got.Minutes != 45 || got.Stage != "GREETING" {
			t.Fatalf("会话字段丢失: %+v", got)
		}
		if got.Status != StatusRunning {
			t.Fatalf("未指定状态时应默认为 running, 实际 %q", got.Status)
		}
		if got.CandidateRef != "cand_aaa" {
			t.Fatalf("候选人引用值丢失: %q", got.CandidateRef)
		}
		if got.Position != "高级后端工程师" || got.Company != "云杉科技" ||
			got.CandidateName != "陈雨" || got.InterviewerName != "林澈" {
			t.Fatalf("展示元信息丢失: %+v", got)
		}
		if string(got.ResumeJSON) != `{"entities":[]}` {
			t.Fatalf("简历实体 JSON 丢失: %s", got.ResumeJSON)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("时间字段应自动填充: %+v", got)
		}
	})

	// 这是整个存储层最重要的一条安全断言: 租户 A 读不到租户 B 的任何数据。
	// 少了它, 知道 session_id 的人就能读到别家公司的面试报告。
	t.Run("跨租户读取一律不可见", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		mustCreate(t, s, Session{ID: "iso1", TenantID: tenantA, CandidateRef: "cand_x", CreatedAt: base})
		if err := s.AppendTurn(ctx, Turn{TenantID: tenantA, SessionID: "iso1", Index: 1, Question: "q", Answer: "a"}); err != nil {
			t.Fatalf("写入问答失败: %v", err)
		}
		if err := s.SaveReport(ctx, Report{TenantID: tenantA, SessionID: "iso1", Recommendation: "HIRE", Payload: []byte(`{"a":1}`)}); err != nil {
			t.Fatalf("写入报告失败: %v", err)
		}
		if err := s.SaveConsent(ctx, Consent{TenantID: tenantA, SessionID: "iso1", Scope: "recording"}); err != nil {
			t.Fatalf("写入授权失败: %v", err)
		}

		if _, err := s.GetSession(ctx, tenantB, "iso1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读会话应返回 ErrNotFound, 实际 %v", err)
		}
		if _, err := s.ListTurns(ctx, tenantB, "iso1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读问答应返回 ErrNotFound, 实际 %v", err)
		}
		if _, err := s.GetReport(ctx, tenantB, "iso1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读报告应返回 ErrNotFound, 实际 %v", err)
		}
		if _, err := s.ListConsents(ctx, tenantB, "iso1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读授权应返回 ErrNotFound, 实际 %v", err)
		}

		// 列表也必须按租户过滤。
		listB, _ := s.ListSessions(ctx, tenantB, 50)
		if len(listB) != 0 {
			t.Fatalf("租户 B 的列表不应包含租户 A 的会话, 实际 %d 条", len(listB))
		}

		// 跨租户写入同样必须被拒绝: 否则可以往别人的会话里塞数据。
		if err := s.AppendTurn(ctx, Turn{TenantID: tenantB, SessionID: "iso1", Index: 2}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户写问答应被拒绝, 实际 %v", err)
		}
		if err := s.SaveReport(ctx, Report{TenantID: tenantB, SessionID: "iso1"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户写报告应被拒绝, 实际 %v", err)
		}
	})

	t.Run("缺少租户或会话ID应被拒绝", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		if err := s.CreateSession(ctx, Session{ID: "no-tenant", Round: 1}); err == nil {
			t.Fatal("缺少租户的会话应被拒绝")
		}
		if err := s.CreateSession(ctx, Session{TenantID: tenantA}); err == nil {
			t.Fatal("缺少会话 ID 应被拒绝")
		}
		if err := s.CreateSession(ctx, Session{
			ID: "s1", TenantID: tenantA, Round: 1, CreatedAt: base,
		}); err != nil {
			t.Fatalf("首次创建失败: %v", err)
		}
		if err := s.CreateSession(ctx, Session{ID: "s1", TenantID: tenantA, Round: 1}); err == nil {
			t.Fatal("重复创建应报错, 而不是静默覆盖已有会话")
		}
	})

	t.Run("更新会话与不存在时的错误", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "s2", TenantID: tenantA, CreatedAt: base})

		updated := Session{
			ID: "s2", TenantID: tenantA, Stage: "SCORING",
			Status: StatusFinished, Recommendation: "HIRE",
		}
		if err := s.UpdateSession(ctx, updated); err != nil {
			t.Fatalf("更新失败: %v", err)
		}
		got, _ := s.GetSession(ctx, tenantA, "s2")
		if got.Stage != "SCORING" || got.Status != StatusFinished || got.Recommendation != "HIRE" {
			t.Fatalf("更新未生效: %+v", got)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("更新不应丢失创建时间")
		}

		if err := s.UpdateSession(ctx, Session{ID: "ghost", TenantID: tenantA}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在的会话应返回 ErrNotFound, 实际 %v", err)
		}
		// 跨租户更新必须失败。
		if err := s.UpdateSession(ctx, Session{ID: "s2", TenantID: tenantB, Stage: "X"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户更新应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("列表倒序并支持条数上限", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		mustCreate(t, s, Session{ID: "a1", TenantID: tenantA, CreatedAt: base})
		mustCreate(t, s, Session{ID: "a2", TenantID: tenantA, CreatedAt: base.Add(time.Minute)})
		mustCreate(t, s, Session{ID: "b1", TenantID: tenantB, CreatedAt: base.Add(2 * time.Minute)})

		all, err := s.ListSessions(ctx, tenantA, 10)
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(all) != 2 || all[0].ID != "a2" {
			t.Fatalf("应按创建时间倒序返回本租户会话, 实际 %v", ids(all))
		}
		limited, _ := s.ListSessions(ctx, tenantA, 1)
		if len(limited) != 1 {
			t.Fatalf("条数上限未生效, 实际 %d 条", len(limited))
		}
	})

	t.Run("追加与读取问答", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "s3", TenantID: tenantA, CreatedAt: base})

		for i := 1; i <= 3; i++ {
			if err := s.AppendTurn(ctx, Turn{
				TenantID: tenantA, SessionID: "s3", Index: i, Stage: "TECH_FUNDAMENTAL",
				QuestionID: "q1", Question: "问题", Answer: "回答",
				DurationMS: 90000, Scored: true, Level: "L3 熟练", LevelNum: 3, Confidence: 0.8,
				Verdict: []byte(`{"gap":0,"final":{"level":"L3 熟练"}}`),
			}); err != nil {
				t.Fatalf("追加第 %d 轮失败: %v", i, err)
			}
		}

		turns, err := s.ListTurns(ctx, tenantA, "s3")
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
		if string(turns[0].Verdict) != `{"gap":0,"final":{"level":"L3 熟练"}}` {
			t.Fatalf("评分结论 JSON 丢失: %s", turns[0].Verdict)
		}
	})

	// 消息队列至少一次投递是常态, 重复写入必须无副作用。
	t.Run("重复投递同一轮问答必须幂等", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "s4", TenantID: tenantA, CreatedAt: base})

		turn := Turn{TenantID: tenantA, SessionID: "s4", Index: 1, Question: "q", Answer: "a", CreatedAt: base}
		if err := s.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("首次追加失败: %v", err)
		}
		if err := s.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("重复追加不应报错: %v", err)
		}
		turns, _ := s.ListTurns(ctx, tenantA, "s4")
		if len(turns) != 1 {
			t.Fatalf("重复投递不应产生第二条记录, 实际 %d 条", len(turns))
		}
	})

	t.Run("报告的保存与读取", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "s5", TenantID: tenantA, CreatedAt: base})

		payload := []byte(`{"recommendation":"HIRE","score":82}`)
		if err := s.SaveReport(ctx, Report{
			TenantID: tenantA, SessionID: "s5", Recommendation: "HIRE",
			Confidence: 0.82, Score: 82, Payload: payload,
		}); err != nil {
			t.Fatalf("保存报告失败: %v", err)
		}
		got, err := s.GetReport(ctx, tenantA, "s5")
		if err != nil {
			t.Fatalf("读取报告失败: %v", err)
		}
		if got.Recommendation != "HIRE" || got.Score != 82 {
			t.Fatalf("报告字段丢失: %+v", got)
		}
		if string(got.Payload) != string(payload) {
			t.Fatalf("报告原文必须可完整复现, 实际 %s", got.Payload)
		}
		if _, err := s.GetReport(ctx, tenantA, "ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的报告应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("授权记录幂等且可追溯", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "s6", TenantID: tenantA, CreatedAt: base})

		c := Consent{
			TenantID: tenantA, SessionID: "s6", CandidateID: "cand_1", Scope: "recording",
			AgreedAt: base, IP: "203.0.113.x", UserAgent: "Mozilla/5.0",
		}
		if err := s.SaveConsent(ctx, c); err != nil {
			t.Fatalf("保存授权失败: %v", err)
		}
		if err := s.SaveConsent(ctx, Consent{
			TenantID: tenantA, SessionID: "s6", CandidateID: "cand_1", Scope: "recording",
			AgreedAt: base.Add(time.Hour),
		}); err != nil {
			t.Fatalf("重复授权不应报错: %v", err)
		}

		list, err := s.ListConsents(ctx, tenantA, "s6")
		if err != nil {
			t.Fatalf("读取授权失败: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("同一范围的授权应只保留一条, 实际 %d 条", len(list))
		}
		if !list[0].AgreedAt.Equal(base) {
			t.Fatalf("应保留最早一次授权时间, 实际 %v", list[0].AgreedAt)
		}
		if list[0].IP != "203.0.113.x" {
			t.Fatalf("授权应只保留脱敏后的网段, 实际 %q", list[0].IP)
		}
	})

	// 事务边界: "会话建好了但授权没记上"意味着持有录音却没有同意凭据,
	// 这是合规事故。授权写入失败时必须把会话一起回滚。
	t.Run("建会话与写授权必须同生共死", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		consents := []Consent{
			{TenantID: tenantA, SessionID: "tx1", Scope: "recording", AgreedAt: base},
			{TenantID: tenantA, SessionID: "tx1", Scope: "scoring", AgreedAt: base},
		}
		if err := s.CreateSessionWithConsents(ctx, Session{
			ID: "tx1", TenantID: tenantA, CreatedAt: base,
		}, consents); err != nil {
			t.Fatalf("组合写入失败: %v", err)
		}

		got, err := s.ListConsents(ctx, tenantA, "tx1")
		if err != nil || len(got) != 2 {
			t.Fatalf("授权应写入两条, 实际 %d 条 (err=%v)", len(got), err)
		}

		// 第二次用同一个 session_id 必然失败, 且不能留下半截数据。
		if err := s.CreateSessionWithConsents(ctx, Session{
			ID: "tx1", TenantID: tenantA, CreatedAt: base,
		}, consents); err == nil {
			t.Fatal("重复创建应失败")
		}
		if got, _ := s.ListConsents(ctx, tenantA, "tx1"); len(got) != 2 {
			t.Fatalf("失败的写入不应改变已有数据, 实际 %d 条", len(got))
		}
	})

	t.Run("结束会话时报告与状态一致落库", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		mustCreate(t, s, Session{ID: "fin1", TenantID: tenantA, Stage: "WRAP_UP", CreatedAt: base})

		rep := Report{
			TenantID: tenantA, SessionID: "fin1",
			Recommendation: "HIRE", Confidence: 0.8, Score: 82, Payload: []byte(`{"score":82}`),
		}
		sess := Session{ID: "fin1", TenantID: tenantA, Stage: "DONE", Recommendation: "HIRE", Minutes: 45}
		if err := s.FinishSession(ctx, sess, rep); err != nil {
			t.Fatalf("结束会话失败: %v", err)
		}

		got, err := s.GetSession(ctx, tenantA, "fin1")
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		if got.Status != StatusFinished || got.Stage != "DONE" {
			t.Fatalf("会话终态未落库: %+v", got)
		}
		if _, err := s.GetReport(ctx, tenantA, "fin1"); err != nil {
			t.Fatalf("报告应同时落库: %v", err)
		}

		// 对不存在的会话结束必须失败, 不能凭空造出报告。
		if err := s.FinishSession(ctx, Session{ID: "ghost", TenantID: tenantA}, rep); !errors.Is(err, ErrNotFound) {
			t.Fatalf("对不存在会话结束应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("审计日志只追加并按租户隔离", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		entries := []AuditEntry{
			{TenantID: tenantA, Actor: "admin/招聘系统", Action: AuditReportView, Target: "s1", CreatedAt: base},
			{TenantID: tenantA, Actor: "interviewer/张三", Action: AuditScoreOverride, Target: "s1", Detail: "L2->L3", CreatedAt: base.Add(time.Minute)},
			{TenantID: tenantB, Actor: "admin/别家", Action: AuditReportView, Target: "s9", CreatedAt: base},
		}
		for _, e := range entries {
			if err := s.AppendAudit(ctx, e); err != nil {
				t.Fatalf("写审计失败: %v", err)
			}
		}

		gotA, err := s.ListAudit(ctx, tenantA, 10)
		if err != nil {
			t.Fatalf("读审计失败: %v", err)
		}
		if len(gotA) != 2 {
			t.Fatalf("租户 A 应有 2 条审计, 实际 %d", len(gotA))
		}
		if gotA[0].Action != AuditScoreOverride {
			t.Fatalf("审计应按时间倒序, 最新一条应是改分, 实际 %q", gotA[0].Action)
		}
		gotB, _ := s.ListAudit(ctx, tenantB, 10)
		if len(gotB) != 1 || gotB[0].Actor != "admin/别家" {
			t.Fatalf("审计必须按租户隔离, 实际 %+v", gotB)
		}
	})

	t.Run("候选人数据可导出与删除", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		ref := "cand_export_1"
		for _, id := range []string{"e1", "e2"} {
			mustCreate(t, s, Session{ID: id, TenantID: tenantA, CandidateRef: ref, CreatedAt: base})
			if err := s.AppendTurn(ctx, Turn{TenantID: tenantA, SessionID: id, Index: 1, Question: "q", Answer: "a"}); err != nil {
				t.Fatalf("写问答失败: %v", err)
			}
			if err := s.SaveReport(ctx, Report{TenantID: tenantA, SessionID: id, Payload: []byte(`{}`)}); err != nil {
				t.Fatalf("写报告失败: %v", err)
			}
			if err := s.SaveConsent(ctx, Consent{TenantID: tenantA, SessionID: id, Scope: "recording"}); err != nil {
				t.Fatalf("写授权失败: %v", err)
			}
		}
		// 另一个候选人的数据不能被带出来。
		mustCreate(t, s, Session{ID: "other", TenantID: tenantA, CandidateRef: "cand_other", CreatedAt: base})

		bundle, err := s.ExportCandidate(ctx, tenantA, ref)
		if err != nil {
			t.Fatalf("导出失败: %v", err)
		}
		if len(bundle.Sessions) != 2 || len(bundle.Turns) != 2 || len(bundle.Reports) != 2 || len(bundle.Consents) != 2 {
			t.Fatalf("导出内容不完整: %+v", bundle)
		}
		if _, err := s.ExportCandidate(ctx, tenantB, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户导出应失败, 实际 %v", err)
		}

		erased, err := s.EraseCandidate(ctx, tenantA, ref)
		if err != nil {
			t.Fatalf("删除失败: %v", err)
		}
		if erased != 2 {
			t.Fatalf("应删除 2 个会话, 实际 %d", erased)
		}
		if _, err := s.GetSession(ctx, tenantA, "e1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后不应还能读到会话, 实际 %v", err)
		}
		// 同租户的其他候选人不受影响。
		if _, err := s.GetSession(ctx, tenantA, "other"); err != nil {
			t.Fatalf("删除必须精确到候选人, 其他数据不应受影响: %v", err)
		}
	})
}

func mustCreate(t *testing.T, s SessionStore, sess Session) {
	t.Helper()
	if err := s.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("创建会话 %s 失败: %v", sess.ID, err)
	}
}

func TestAnalyticsContract(t *testing.T) {
	storeContract(t, func(t *testing.T) SessionStore { return NewMemoryStore() })
	// 单独的聚合断言: 数据准备与上面共用同一套语义。
}

func TestAnalyticsAggregatesPerTenant(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	s := NewMemoryStore()
	defer s.Close()

	// 租户 A: 一场完成(分数 82, 含一次降级)、一场进行中
	mustCreate(t, s, Session{ID: "an1", TenantID: tenantA, Status: StatusFinished, Recommendation: "HIRE", CreatedAt: base})
	mustCreate(t, s, Session{ID: "an2", TenantID: tenantA, Status: StatusRunning, CreatedAt: base})
	if err := s.SaveReport(ctx, Report{TenantID: tenantA, SessionID: "an1", Score: 82, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("写报告失败: %v", err)
	}
	for i, t2 := range []Turn{
		{TenantID: tenantA, SessionID: "an1", Index: 1, Competency: "architecture", Level: "L4 精通", Scored: true},
		{TenantID: tenantA, SessionID: "an1", Index: 2, Competency: "architecture", Level: "L3 熟练", Scored: true, IsProbe: true, DegradedFrom: "规则"},
		{TenantID: tenantA, SessionID: "an1", Index: 3, Competency: "language_core", Level: "L2 了解", Scored: true},
	} {
		t2.Index = i + 1
		if err := s.AppendTurn(ctx, t2); err != nil {
			t.Fatalf("写问答失败: %v", err)
		}
	}
	// 租户 B: 不应被统计进去
	mustCreate(t, s, Session{ID: "an3", TenantID: tenantB, Status: StatusFinished, Recommendation: "NO_HIRE", CreatedAt: base})

	got, err := s.Analytics(ctx, tenantA)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if got.Sessions != 2 || got.Finished != 1 || got.Running != 1 {
		t.Fatalf("场次统计错误: %+v", got)
	}
	if got.ByRecommendation["HIRE"] != 1 {
		t.Fatalf("结论分布错误: %+v", got.ByRecommendation)
	}
	if got.AvgScore != 82 {
		t.Fatalf("平均分错误: %v", got.AvgScore)
	}
	if got.TotalTurns != 3 || got.ScoredTurns != 3 || got.ProbeTurns != 1 || got.DegradedTurns != 1 {
		t.Fatalf("轮次统计错误: %+v", got)
	}
	if got.ByCompetency["architecture"]["L4 精通"] != 1 || got.ByCompetency["architecture"]["L3 熟练"] != 1 {
		t.Fatalf("能力项分布错误: %+v", got.ByCompetency)
	}

	gotB, _ := s.Analytics(ctx, tenantB)
	if gotB.Sessions != 1 || gotB.ByRecommendation["NO_HIRE"] != 1 {
		t.Fatalf("租户 B 统计错误: %+v", gotB)
	}
}

func ids(list []Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}
