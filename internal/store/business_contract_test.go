package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// businessContract 是职位/候选人/投递/题库/安排/录制件的行为契约。
//
// 与 storeContract 分开: 会话是热路径(性能与幂等优先), 业务域是管理路径
// (一致性与可追溯优先), 两者的失败模式完全不同, 混在一个用例里
// 只会让失败原因变得难读。
func businessContract(t *testing.T, newStore func(t *testing.T) SessionStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)

	t.Run("职位只对本租户可见", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.CreateJob(ctx, Job{
			ID: "job_1", TenantID: tenantA, Title: "高级后端工程师",
			Department: "基础架构", Status: JobOpen, Headcount: 3, CreatedAt: base,
			CompetencyModel: map[string]int{"language_core": 40, "architecture": 60},
		}); err != nil {
			t.Fatalf("创建职位失败: %v", err)
		}
		job, err := s.GetJob(ctx, tenantA, "job_1")
		if err != nil {
			t.Fatalf("读取职位失败: %v", err)
		}
		if job.Title != "高级后端工程师" || job.CompetencyModel["architecture"] != 60 {
			t.Fatalf("职位字段丢失: %+v", job)
		}
		if len(job.Rounds) != 5 {
			t.Fatalf("轮次编排应补齐为 5 轮, 实际 %d", len(job.Rounds))
		}
		if spec, ok := job.RoundSpecOf(2); !ok || spec.Mode != "coding" {
			t.Fatalf("二面应为编程模式: %+v", spec)
		}
		if spec, ok := job.RoundSpecOf(3); !ok || !spec.HumanPanel {
			t.Fatalf("三面应需要人类面试官在场: %+v", spec)
		}
		if _, err := s.GetJob(ctx, tenantB, "job_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读取职位应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("候选人重复推送是幂等的", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		c := Candidate{
			Ref: "cand_1", TenantID: tenantA, Name: "陈雨", Email: "chen@example.com",
			ResumeSource: "负责 ZSet 索引优化", CreatedAt: base,
		}
		if err := s.CreateCandidate(ctx, c); err != nil {
			t.Fatalf("创建候选人失败: %v", err)
		}
		c.Name = "陈雨改名"
		if err := s.CreateCandidate(ctx, c); err != nil {
			t.Fatalf("重复推送候选人应幂等, 实际报错: %v", err)
		}
		got, err := s.GetCandidate(ctx, tenantA, "cand_1")
		if err != nil {
			t.Fatalf("读取候选人失败: %v", err)
		}
		if got.Name != "陈雨改名" {
			t.Fatalf("重复推送应覆盖为新数据, 实际 %q", got.Name)
		}
		if got.ResumeSource == "" {
			t.Fatal("简历原文应被保留")
		}
		list, err := s.ListCandidates(ctx, tenantA, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("候选人列表异常: %v %d", err, len(list))
		}
		if _, err := s.GetCandidate(ctx, tenantB, "cand_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读取候选人应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("投递管道按职位过滤且保留轮次状态", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.CreateJob(ctx, Job{ID: "job_1", TenantID: tenantA, Title: "后端", CreatedAt: base}); err != nil {
			t.Fatalf("创建职位失败: %v", err)
		}
		job, _ := s.GetJob(ctx, tenantA, "job_1")

		app := Application{
			ID: "app_1", TenantID: tenantA, JobID: "job_1", JobTitle: job.Title,
			CandidateRef: "cand_1", CandidateName: "陈雨", Stage: StageAIInterview,
			Status: "active", CreatedAt: base,
		}
		app.NormalizeRounds(job.Rounds)
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("创建投递失败: %v", err)
		}
		got, err := s.GetApplication(ctx, tenantA, "app_1")
		if err != nil {
			t.Fatalf("读取投递失败: %v", err)
		}
		if len(got.Rounds) != 5 {
			t.Fatalf("投递应带 5 轮状态, 实际 %d", len(got.Rounds))
		}

		r2, _ := got.RoundOf(2)
		r2.Status = "finished"
		r2.Score = 82
		r2.Recommendation = "HIRE"
		for i := range got.Rounds {
			if got.Rounds[i].Round == 2 {
				got.Rounds[i] = r2
			}
		}
		got.CurrentRound = 2
		got.Stage = StageHuman
		if err := s.UpdateApplication(ctx, got); err != nil {
			t.Fatalf("更新投递失败: %v", err)
		}
		updated, _ := s.GetApplication(ctx, tenantA, "app_1")
		if updated.CurrentRound != 2 || updated.Stage != StageHuman {
			t.Fatalf("投递状态未更新: %+v", updated)
		}
		if r, ok := updated.RoundOf(2); !ok || r.Score != 82 {
			t.Fatalf("轮次结论丢失: %+v", r)
		}

		byJob, err := s.ListApplications(ctx, tenantA, "job_1")
		if err != nil || len(byJob) != 1 {
			t.Fatalf("按职位过滤失败: %v %d", err, len(byJob))
		}
		if other, _ := s.ListApplications(ctx, tenantA, "job_missing"); len(other) != 0 {
			t.Fatalf("不存在的职位不应返回投递, 实际 %d", len(other))
		}
		mine, err := s.ListApplicationsByCandidate(ctx, tenantA, "cand_1")
		if err != nil || len(mine) != 1 {
			t.Fatalf("按候选人查询投递失败: %v %d", err, len(mine))
		}
	})

	t.Run("题库改一次版本加一且租户隔离", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		q := QuestionItem{
			ID: "q_1", TenantID: tenantA, Stage: "TECH_FUNDAMENTAL", Competency: "language_core",
			Text: "讲讲 GMP。", Keywords: []string{"GMP", "抢占"}, MaxProbe: 2,
			Status: "published", ReferencePoints: []ReferencePoint{{Key: "本地队列", Text: "P 的本地队列"}},
			CreatedAt: base,
		}
		if err := s.CreateQuestion(ctx, q); err != nil {
			t.Fatalf("创建题目失败: %v", err)
		}
		got, err := s.GetQuestion(ctx, tenantA, "q_1")
		if err != nil {
			t.Fatalf("读取题目失败: %v", err)
		}
		if got.Version != 1 {
			t.Fatalf("新建题目版本应为 1, 实际 %d", got.Version)
		}
		if len(got.ReferencePoints) != 1 || got.ReferencePoints[0].Key != "本地队列" {
			t.Fatalf("参考答案要点丢失: %+v", got.ReferencePoints)
		}
		got.Text = "详细讲讲 Go 的 GMP 调度。"
		if err := s.UpdateQuestion(ctx, got); err != nil {
			t.Fatalf("更新题目失败: %v", err)
		}
		again, _ := s.GetQuestion(ctx, tenantA, "q_1")
		if again.Version != 2 {
			t.Fatalf("修改后版本应为 2, 实际 %d", again.Version)
		}
		if again.Text != got.Text {
			t.Fatalf("题目正文未更新: %q", again.Text)
		}
		if _, err := s.GetQuestion(ctx, tenantB, "q_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读取题目应返回 ErrNotFound, 实际 %v", err)
		}
		if err := s.DeleteQuestion(ctx, tenantB, "q_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户删除题目应返回 ErrNotFound, 实际 %v", err)
		}
		if err := s.DeleteQuestion(ctx, tenantA, "q_1"); err != nil {
			t.Fatalf("删除题目失败: %v", err)
		}
		if _, err := s.GetQuestion(ctx, tenantA, "q_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后应读不到, 实际 %v", err)
		}
	})

	t.Run("面试安排按时间窗口过滤", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		mk := func(id string, at time.Time) Schedule {
			return Schedule{
				ID: id, TenantID: tenantA, ApplicationID: "app_1", Round: 1,
				Mode: "video", ScheduledAt: at, DurationMin: 45,
				Interviewer: "林澈", Status: "confirmed", CreatedAt: base,
			}
		}
		for _, sc := range []Schedule{
			mk("sch_1", base),
			mk("sch_2", base.Add(24*time.Hour)),
			mk("sch_3", base.Add(72*time.Hour)),
		} {
			if err := s.CreateSchedule(ctx, sc); err != nil {
				t.Fatalf("创建安排失败: %v", err)
			}
		}
		window, err := s.ListSchedules(ctx, tenantA, base.Add(-time.Hour), base.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("查询安排失败: %v", err)
		}
		if len(window) != 2 {
			t.Fatalf("时间窗口内应有 2 场, 实际 %d", len(window))
		}
		if window[0].ID != "sch_1" || window[1].ID != "sch_2" {
			t.Fatalf("安排应按时间升序: %+v", window)
		}
		if _, err := s.GetSchedule(ctx, tenantB, "sch_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读取安排应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("录制件保留期到期可被清理", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.CreateRecording(ctx, Recording{
			ID: "rec_1", TenantID: tenantA, SessionID: "s1", CandidateRef: "cand_1",
			Kind: "video", MimeType: "video/webm", StorageKey: "tenant-a/s1/video.webm",
			Chunks: 12, SizeBytes: 1024, Status: "complete", CreatedAt: base,
			DeleteAfter: base.Add(-time.Hour),
		}); err != nil {
			t.Fatalf("创建录制件失败: %v", err)
		}
		if err := s.CreateRecording(ctx, Recording{
			ID: "rec_2", TenantID: tenantA, SessionID: "s2", Kind: "audio",
			StorageKey: "tenant-a/s2/audio.webm", Status: "complete", CreatedAt: base,
			DeleteAfter: base.Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("创建录制件失败: %v", err)
		}

		bySession, err := s.GetRecordingBySession(ctx, tenantA, "s1", "video")
		if err != nil {
			t.Fatalf("按会话读取录制件失败: %v", err)
		}
		if bySession.StorageKey != "tenant-a/s1/video.webm" {
			t.Fatalf("存储键丢失: %q", bySession.StorageKey)
		}
		if _, err := s.GetRecordingBySession(ctx, tenantB, "s1", "video"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨租户读取录制件应返回 ErrNotFound, 实际 %v", err)
		}
		expired, err := s.PurgeExpiredRecordings(ctx, base)
		if err != nil {
			t.Fatalf("查询到期录制件失败: %v", err)
		}
		if len(expired) != 1 || expired[0].ID != "rec_1" {
			t.Fatalf("应只有 rec_1 到期, 实际 %+v", expired)
		}
		if err := s.DeleteRecording(ctx, tenantA, "rec_1"); err != nil {
			t.Fatalf("删除录制件失败: %v", err)
		}
		list, err := s.ListRecordings(ctx, tenantA, "s2")
		if err != nil || len(list) != 1 {
			t.Fatalf("s2 应仍有 1 个录制件: %v %d", err, len(list))
		}
	})

	t.Run("更新不存在的记录返回 ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()

		if err := s.UpdateJob(ctx, Job{ID: "nope", TenantID: tenantA}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在的职位应返回 ErrNotFound, 实际 %v", err)
		}
		if err := s.UpdateApplication(ctx, Application{ID: "nope", TenantID: tenantA}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在的投递应返回 ErrNotFound, 实际 %v", err)
		}
		if err := s.UpdateQuestion(ctx, QuestionItem{ID: "nope", TenantID: tenantA}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在的题目应返回 ErrNotFound, 实际 %v", err)
		}
	})

	t.Run("归一化轮次编排稳定且幂等", func(t *testing.T) {
		once := NormalizeRounds(nil)
		twice := NormalizeRounds(once)
		if len(once) != 5 || len(twice) != 5 {
			t.Fatalf("轮次应为 5 轮: %d / %d", len(once), len(twice))
		}
		for i := range once {
			if once[i].Round != twice[i].Round || once[i].Name != twice[i].Name ||
				once[i].Minutes != twice[i].Minutes || once[i].Mode != twice[i].Mode ||
				once[i].AILead != twice[i].AILead || once[i].HumanPanel != twice[i].HumanPanel {
				t.Fatalf("归一化不幂等: %+v vs %+v", once[i], twice[i])
			}
			if once[i].Round != i+1 {
				t.Fatalf("轮次顺序错误: %+v", once)
			}
		}
		custom := NormalizeRounds([]RoundSpec{{Round: 2, Name: "自定义二面", Minutes: 90}})
		if custom[1].Name != "自定义二面" || custom[1].Minutes != 90 {
			t.Fatalf("自定义轮次被覆盖: %+v", custom[1])
		}
		if custom[0].Name == "" {
			t.Fatalf("未指定的轮次应补默认值: %+v", custom[0])
		}
	})
}

// TestBusinessMemoryContract 让内存实现跑一遍业务域契约。
func TestBusinessMemoryContract(t *testing.T) {
	businessContract(t, func(t *testing.T) SessionStore { return NewMemoryStore() })
}

// TestNormalizeTenantStripsUnsafeChars 校验租户 ID 不会带出路径穿越字符。
func TestNormalizeTenantStripsUnsafeChars(t *testing.T) {
	if got := NormalizeTenant("../Tenant A"); got != "tenanta" {
		t.Fatalf("租户归一化结果不安全: %q", got)
	}
	if got := NormalizeTenant("acme-cn_1"); got != "acme-cn_1" {
		t.Fatalf("合法租户 ID 不应被改动: %q", got)
	}
	if got := NormalizeTenant("   "); got != "" {
		t.Fatalf("空白租户应归一化为空: %q", got)
	}
}
