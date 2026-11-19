package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/account"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// seedDemoData 生成一套开箱即用的演示数据: 管理员 + 职位 + 候选人 +
// 投递 + 面试排期。设计成幂等: 已存在的数据会被跳过, 可以重复执行。
func seedDemoData(ctx context.Context, st store.SessionStore, accounts *account.Service, tenant, invite string, logger *log.Logger) error {
	now := time.Now()

	// 1. 演示管理员(需要邀请码; 未设置时跳过, 不阻断其它数据)
	if invite != "" {
		_, err := accounts.Register(ctx, account.RegisterInput{
			TenantID:   tenant,
			Name:       "演示管理员",
			Email:      "admin@demo.local",
			Password:   "Demo123456!",
			Role:       auth.RoleAdmin,
			InviteCode: invite,
		})
		switch {
		case err == nil:
			logger.Print("演示管理员已创建: admin@demo.local / Demo123456!")
		case errors.Is(err, account.ErrUserExists):
			logger.Print("演示管理员已存在, 跳过")
		default:
			logger.Printf("演示管理员创建失败(跳过): %v", err)
		}
	} else {
		logger.Print("未设置 ADMIN_INVITE_CODE, 跳过演示管理员创建")
	}

	// 2. 职位
	jobs := []store.Job{
		{
			ID: "job_demo_backend", TenantID: tenant, Title: "高级后端工程师",
			Department: "基础架构", Level: "P6", Location: "西安", Headcount: 3,
			Status: store.JobOpen, Owner: "演示管理员",
			CompetencyModel: map[string]int{"language_core": 30, "distributed_system": 30, "project_depth": 25, "architecture": 15},
			Rounds:          store.NormalizeRounds(nil), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "job_demo_ai", TenantID: tenant, Title: "AI 应用工程师",
			Department: "智能平台", Level: "P6", Location: "北京", Headcount: 2,
			Status: store.JobOpen, Owner: "演示管理员",
			CompetencyModel: map[string]int{"architecture": 30, "tech_choice": 25, "project_depth": 25, "language_core": 20},
			Rounds:          store.NormalizeRounds(nil), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "job_demo_frontend", TenantID: tenant, Title: "前端工程师",
			Department: "用户体验", Level: "P5", Location: "远程", Headcount: 2,
			Status: store.JobOpen, Owner: "演示管理员",
			CompetencyModel: map[string]int{"language_core": 40, "project_depth": 30, "tech_choice": 30},
			Rounds:          store.NormalizeRounds(nil), CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, j := range jobs {
		if err := st.CreateJob(ctx, j); err != nil {
			logger.Printf("职位 %q 已存在或创建失败(跳过): %v", j.Title, err)
		}
	}

	// 3. 候选人
	candidates := []store.Candidate{
		{Ref: "cand_demo_zhangwei", TenantID: tenant, Name: "张伟", Email: "zhangwei@demo.local",
			Phone: "13800000001", Source: "内推", Tags: []string{"Go", "分布式"}, CreatedAt: now, UpdatedAt: now},
		{Ref: "cand_demo_lina", TenantID: tenant, Name: "李娜", Email: "lina@demo.local",
			Phone: "13800000002", Source: "校招", Tags: []string{"Java", "算法"}, CreatedAt: now, UpdatedAt: now},
		{Ref: "cand_demo_wangqiang", TenantID: tenant, Name: "王强", Email: "wangqiang@demo.local",
			Phone: "13800000003", Source: "猎头", Tags: []string{"Python", "AI"}, CreatedAt: now, UpdatedAt: now},
		{Ref: "cand_demo_zhaomin", TenantID: tenant, Name: "赵敏", Email: "zhaomin@demo.local",
			Phone: "13800000004", Source: "官网", Tags: []string{"前端", "React"}, CreatedAt: now, UpdatedAt: now},
	}
	for _, c := range candidates {
		if err := st.CreateCandidate(ctx, c); err != nil {
			logger.Printf("候选人 %q 已存在或创建失败(跳过): %v", c.Name, err)
		}
	}

	// 4. 投递(招聘管道)
	jobRounds := make(map[string][]store.RoundSpec, len(jobs))
	for _, j := range jobs {
		jobRounds[j.ID] = j.Rounds
	}
	apps := []store.Application{
		{ID: "app_demo_1", TenantID: tenant, JobID: "job_demo_backend", JobTitle: "高级后端工程师",
			CandidateRef: "cand_demo_zhangwei", CandidateName: "张伟", Stage: store.StageAIInterview,
			Status: "active", CurrentRound: 1, Owner: "演示管理员", Source: "内推", CreatedAt: now, UpdatedAt: now},
		{ID: "app_demo_2", TenantID: tenant, JobID: "job_demo_backend", JobTitle: "高级后端工程师",
			CandidateRef: "cand_demo_lina", CandidateName: "李娜", Stage: store.StageScreening,
			Status: "active", CurrentRound: 0, Owner: "演示管理员", Source: "校招", CreatedAt: now, UpdatedAt: now},
		{ID: "app_demo_3", TenantID: tenant, JobID: "job_demo_ai", JobTitle: "AI 应用工程师",
			CandidateRef: "cand_demo_wangqiang", CandidateName: "王强", Stage: store.StageHuman,
			Status: "active", CurrentRound: 2, Owner: "演示管理员", Source: "猎头", CreatedAt: now, UpdatedAt: now},
		{ID: "app_demo_4", TenantID: tenant, JobID: "job_demo_frontend", JobTitle: "前端工程师",
			CandidateRef: "cand_demo_zhaomin", CandidateName: "赵敏", Stage: store.StageOffer,
			Status: "active", CurrentRound: 5, Owner: "演示管理员", Source: "官网", CreatedAt: now, UpdatedAt: now},
	}
	for i := range apps {
		if r, ok := jobRounds[apps[i].JobID]; ok {
			apps[i].NormalizeRounds(r)
		}
		if err := st.CreateApplication(ctx, apps[i]); err != nil {
			logger.Printf("投递 %s 已存在或创建失败(跳过): %v", apps[i].ID, err)
		}
	}

	// 5. 面试排期
	schedules := []store.Schedule{
		{ID: "sch_demo_1", TenantID: tenant, ApplicationID: "app_demo_2", JobID: "job_demo_backend",
			CandidateRef: "cand_demo_lina", Round: 1, Mode: "video", ScheduledAt: now.Add(24 * time.Hour),
			DurationMin: 45, Interviewer: "演示管理员", Status: "pending", CreatedAt: now, UpdatedAt: now},
		{ID: "sch_demo_2", TenantID: tenant, ApplicationID: "app_demo_3", JobID: "job_demo_ai",
			CandidateRef: "cand_demo_wangqiang", Round: 3, Mode: "video", ScheduledAt: now.Add(48 * time.Hour),
			DurationMin: 60, Interviewer: "演示管理员", Status: "confirmed", CreatedAt: now, UpdatedAt: now},
	}
	for _, s := range schedules {
		if err := st.CreateSchedule(ctx, s); err != nil {
			logger.Printf("排期 %s 已存在或创建失败(跳过): %v", s.ID, err)
		}
	}

	logger.Printf("演示数据生成完成: %d 个职位 / %d 位候选人 / %d 条投递 / %d 条排期",
		len(jobs), len(candidates), len(apps), len(schedules))
	return nil
}
