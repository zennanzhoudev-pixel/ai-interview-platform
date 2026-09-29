package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/knowledge"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/privacy"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/resume"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 本文件是"面试之外"的业务接口: 职位、候选人、投递管道、题库、面试安排。
//
// 它们共享一条规则: **读走 recruit:read, 写走各自权限点, 写操作一律审计**。
// 招聘系统里的每一次改动都会影响某个人的职业选择, 因此"谁改的"必须查得到。

var idCounter int64

// newID 生成带前缀的业务 ID。
//
// 用"时间戳 + 进程内自增"而不是随机串: 招聘看板要按创建顺序稳定排序,
// 可读且单调的 ID 让"这两条数据谁先建的"一眼可见, 排查时序问题时省掉一轮。
func newID(prefix string) string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("%s_%d_%03d", prefix, time.Now().Unix(), n%1000)
}

// decodeBody 解析请求体并统一处理错误响应。
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return false
	}
	return true
}

// storeErr 把存储错误翻译成 HTTP 响应。
func (s *Server) storeErr(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "记录不存在")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "记录已存在")
	default:
		s.metrics.StoreErrors.WithLabelValues(op).Inc()
		writeError(w, http.StatusInternalServerError, "存储操作失败")
	}
}

/* ---------------- 职位 ---------------- */

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	jobs, err := s.cfg.Store.ListJobs(ctx, platform.Tenant(r.Context()))
	if err != nil {
		s.storeErr(w, err, "list_jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req store.Job
	if !decodeBody(w, r, 256<<10, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "职位名称不能为空")
		return
	}
	req.ID = defaultString(req.ID, newID("job"))
	req.TenantID = platform.Tenant(r.Context())
	if req.Status == "" {
		req.Status = store.JobDraft
	}
	if req.Headcount <= 0 {
		req.Headcount = 1
	}
	// 轮次编排必须补齐: 只填了"我们有 3 轮"的职位也应该能开面,
	// 否则界面上的"第 3 / 3 轮"会因为缺配置而显示成空。
	req.Rounds = store.NormalizeRounds(req.Rounds)

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Store.CreateJob(ctx, req); err != nil {
		s.storeErr(w, err, "create_job")
		return
	}
	s.audit(r.Context(), store.AuditJobUpsert, req.ID,
		fmt.Sprintf("新建职位 %s (%d 轮)", req.Title, len(req.Rounds)))
	s.notifyKnowledgeChanged(r.Context())
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	job, err := s.cfg.Store.GetJob(ctx, platform.Tenant(r.Context()), r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "get_job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	id := r.PathValue("id")

	current, err := s.cfg.Store.GetJob(ctx, tenant, id)
	if err != nil {
		s.storeErr(w, err, "get_job")
		return
	}
	var req store.Job
	if !decodeBody(w, r, 256<<10, &req) {
		return
	}
	// 局部更新: 未提交的字段保持原值。管理界面里"只改状态"是最常见的操作,
	// 要求客户端回传整份对象会把"忘了带某个字段"变成静默的数据丢失。
	merged := mergeJob(current, req)
	merged.TenantID = tenant
	merged.ID = id
	merged.Rounds = store.NormalizeRounds(merged.Rounds)
	if err := s.cfg.Store.UpdateJob(ctx, merged); err != nil {
		s.storeErr(w, err, "update_job")
		return
	}
	s.audit(r.Context(), store.AuditJobUpsert, id, "更新职位 "+merged.Title)
	writeJSON(w, http.StatusOK, merged)
}

func mergeJob(cur, patch store.Job) store.Job {
	out := cur
	if patch.Title != "" {
		out.Title = patch.Title
	}
	if patch.Department != "" {
		out.Department = patch.Department
	}
	if patch.Level != "" {
		out.Level = patch.Level
	}
	if patch.Location != "" {
		out.Location = patch.Location
	}
	if patch.Headcount > 0 {
		out.Headcount = patch.Headcount
	}
	if patch.Status != "" {
		out.Status = patch.Status
	}
	if len(patch.CompetencyModel) > 0 {
		out.CompetencyModel = patch.CompetencyModel
	}
	if len(patch.Rounds) > 0 {
		out.Rounds = patch.Rounds
	}
	if patch.Owner != "" {
		out.Owner = patch.Owner
	}
	return out
}

/* ---------------- 候选人 ---------------- */

func (s *Server) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	list, err := s.cfg.Store.ListCandidates(ctx, platform.Tenant(r.Context()), limit)
	if err != nil {
		s.storeErr(w, err, "list_candidates")
		return
	}
	// 列表接口不回简历原文: 一条简历几十 KB, 一屏 50 条就是几 MB,
	// 而列表上根本展示不了正文。
	writeJSON(w, http.StatusOK, map[string]any{"candidates": list})
}

func (s *Server) handleUpsertCandidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CandidateRef string   `json:"candidate_ref"`
		CandidateID  string   `json:"candidate_id"`
		Name         string   `json:"name"`
		Email        string   `json:"email"`
		Phone        string   `json:"phone"`
		Source       string   `json:"source"`
		Tags         []string `json:"tags"`
		ResumeText   string   `json:"resume_text"`
	}
	if !decodeBody(w, r, 2<<20, &req) {
		return
	}
	tenant := platform.Tenant(r.Context())

	// 候选人原始 ID 不落库, 只落 HMAC 假名引用值。
	ref := strings.TrimSpace(req.CandidateRef)
	if ref == "" {
		key := strings.TrimSpace(req.CandidateID)
		if key == "" {
			key = strings.TrimSpace(req.Email)
		}
		if key == "" {
			key = strings.TrimSpace(req.Phone)
		}
		if key == "" {
			writeError(w, http.StatusBadRequest, "需要提供 candidate_id、email 或 phone 之一用于生成候选人引用值")
			return
		}
		ref = privacy.Ref(s.cfg.Secret, tenant, key)
	}

	candidate := store.Candidate{
		Ref: ref, TenantID: tenant,
		Name: req.Name, Source: req.Source, Tags: req.Tags,
		// 联系方式落库前脱敏: 招聘系统被拖库时, 泄漏"陈雨 + 邮箱前缀"
		// 和泄漏"完整手机号"是完全不同的量级。
		Email: privacy.MaskEmail(req.Email),
		Phone: privacy.MaskPhone(req.Phone),
	}
	if strings.TrimSpace(req.ResumeText) != "" {
		parsed := resume.NewRuleExtractor().Extract(req.ResumeText)
		candidate.ResumeSource = req.ResumeText
		if raw, err := json.Marshal(parsed); err == nil {
			candidate.ResumeJSON = raw
		}
	}

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Store.CreateCandidate(ctx, candidate); err != nil {
		s.storeErr(w, err, "upsert_candidate")
		return
	}
	s.audit(r.Context(), store.AuditCandidateUpsert, ref, "导入/更新候选人档案")
	writeJSON(w, http.StatusOK, candidate)
}

func (s *Server) handleGetCandidate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	ref := r.PathValue("ref")

	candidate, err := s.cfg.Store.GetCandidate(ctx, tenant, ref)
	if err != nil {
		s.storeErr(w, err, "get_candidate")
		return
	}
	apps, err := s.cfg.Store.ListApplicationsByCandidate(ctx, tenant, ref)
	if err != nil {
		s.storeErr(w, err, "list_applications")
		return
	}
	sessions, _ := s.cfg.Store.ListSessions(ctx, tenant, 200)
	var mine []store.Session
	for _, sess := range sessions {
		if sess.CandidateRef == ref {
			mine = append(mine, sess)
		}
	}
	// 简历原文与结构化实体只在这个"单人详情"接口返回, 并且需要 recruit:read。
	writeJSON(w, http.StatusOK, map[string]any{
		"candidate":    candidate,
		"resume_text":  candidate.ResumeSource,
		"resume":       json.RawMessage(nonEmptyJSON(candidate.ResumeJSON)),
		"applications": apps,
		"sessions":     mine,
	})
}

func nonEmptyJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

/* ---------------- 投递(招聘管道) ---------------- */

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	list, err := s.cfg.Store.ListApplications(ctx, platform.Tenant(r.Context()), r.URL.Query().Get("job_id"))
	if err != nil {
		s.storeErr(w, err, "list_applications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": list})
}

func (s *Server) handleCreateApplication(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobID        string `json:"job_id"`
		CandidateRef string `json:"candidate_ref"`
		CandidateID  string `json:"candidate_id"`
		Owner        string `json:"owner"`
		Source       string `json:"source"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	tenant := platform.Tenant(r.Context())
	if strings.TrimSpace(req.JobID) == "" {
		writeError(w, http.StatusBadRequest, "必须指定职位")
		return
	}

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()

	job, err := s.cfg.Store.GetJob(ctx, tenant, req.JobID)
	if err != nil {
		s.storeErr(w, err, "get_job")
		return
	}
	ref := strings.TrimSpace(req.CandidateRef)
	if ref == "" && strings.TrimSpace(req.CandidateID) != "" {
		ref = privacy.Ref(s.cfg.Secret, tenant, req.CandidateID)
	}
	if ref == "" {
		writeError(w, http.StatusBadRequest, "必须指定候选人")
		return
	}
	candidate, err := s.cfg.Store.GetCandidate(ctx, tenant, ref)
	if err != nil {
		s.storeErr(w, err, "get_candidate")
		return
	}

	app := store.Application{
		ID: newID("app"), TenantID: tenant, JobID: job.ID, JobTitle: job.Title,
		CandidateRef: ref, CandidateName: candidate.Name,
		Stage: store.StageScreening, Status: "active", Owner: req.Owner, Source: req.Source,
	}
	app.NormalizeRounds(job.Rounds)
	if err := s.cfg.Store.CreateApplication(ctx, app); err != nil {
		s.storeErr(w, err, "create_application")
		return
	}
	s.audit(r.Context(), store.AuditApplicationUpsert, app.ID,
		fmt.Sprintf("候选人 %s 投递 %s", ref, job.Title))
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	app, err := s.cfg.Store.GetApplication(ctx, platform.Tenant(r.Context()), r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "get_application")
		return
	}

	// 把每一轮的会话与报告挂上, 让"看板上点进去就能看结果"成立。
	var sessions []store.Session
	for _, rt := range app.Rounds {
		if rt.SessionID == "" {
			continue
		}
		sess, err := s.cfg.Store.GetSession(ctx, app.TenantID, rt.SessionID)
		if err == nil {
			sessions = append(sessions, sess)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"application": app, "sessions": sessions})
}

func (s *Server) handlePatchApplication(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stage  string `json:"stage"`
		Status string `json:"status"`
		Owner  string `json:"owner"`
		Note   string `json:"note"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())

	app, err := s.cfg.Store.GetApplication(ctx, tenant, r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "get_application")
		return
	}
	previous := app.Stage
	if req.Stage != "" {
		stage := store.AppStage(req.Stage)
		if !store.ValidStage(stage) {
			writeError(w, http.StatusBadRequest, "管道阶段取值不合法")
			return
		}
		app.Stage = stage
		switch stage {
		case store.StageHired:
			app.Status = "hired"
		case store.StageRejected:
			app.Status = "rejected"
		case store.StageWithdrawn:
			app.Status = "withdrawn"
		default:
			app.Status = "active"
		}
	}
	if req.Status != "" {
		app.Status = req.Status
	}
	if req.Owner != "" {
		app.Owner = req.Owner
	}
	if err := s.cfg.Store.UpdateApplication(ctx, app); err != nil {
		s.storeErr(w, err, "update_application")
		return
	}
	s.audit(r.Context(), store.AuditApplicationUpsert, app.ID,
		fmt.Sprintf("管道阶段 %s -> %s; 备注: %s", previous, app.Stage, privacy.MaskUserAgent(req.Note)))
	writeJSON(w, http.StatusOK, app)
}

/* ---------------- 题库 ---------------- */

func (s *Server) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	items, err := s.cfg.Store.ListQuestions(ctx, tenant)
	if err != nil {
		s.storeErr(w, err, "list_questions")
		return
	}
	// 内置题库一并返回, 但标记来源, 界面上不可编辑。
	out := make([]store.QuestionItem, 0, len(items))
	out = append(out, items...)
	for _, seed := range knowledge.SeedItems() {
		seed.TenantID = tenant
		out = append(out, seed)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"questions": out,
		"stats":     s.knowledgeStats(r.Context()),
	})
}

func (s *Server) handleCreateQuestion(w http.ResponseWriter, r *http.Request) {
	item, ok := s.decodeQuestion(w, r)
	if !ok {
		return
	}
	// 新建时必须交代"这是什么阶段的什么题", 否则它既进不了面试抽题,
	// 也无法被检索到正确的上下文。
	if strings.TrimSpace(item.Text) == "" {
		writeError(w, http.StatusBadRequest, "题干不能为空")
		return
	}
	if strings.TrimSpace(item.Stage) == "" {
		writeError(w, http.StatusBadRequest, "必须指定题目所属阶段")
		return
	}
	item.ID = defaultString(item.ID, newID("q"))
	item.TenantID = platform.Tenant(r.Context())
	if item.Status == "" {
		item.Status = "draft"
	}
	if item.Version == 0 {
		item.Version = 1
	}

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Store.CreateQuestion(ctx, item); err != nil {
		s.storeErr(w, err, "create_question")
		return
	}
	s.audit(r.Context(), store.AuditQuestionUpsert, item.ID,
		fmt.Sprintf("新建题目(status=%s, 参考要点 %d 条)", item.Status, len(item.ReferencePoints)))
	s.notifyKnowledgeChanged(r.Context())
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleUpdateQuestion(w http.ResponseWriter, r *http.Request) {
	item, ok := s.decodeQuestion(w, r)
	if !ok {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	id := r.PathValue("id")

	current, err := s.cfg.Store.GetQuestion(ctx, tenant, id)
	if err != nil {
		s.storeErr(w, err, "get_question")
		return
	}
	merged := mergeQuestion(current, item)
	merged.ID = id
	merged.TenantID = tenant
	if err := s.cfg.Store.UpdateQuestion(ctx, merged); err != nil {
		s.storeErr(w, err, "update_question")
		return
	}
	s.audit(r.Context(), store.AuditQuestionUpsert, id,
		fmt.Sprintf("更新题目(status=%s, 版本 %d)", merged.Status, merged.Version))
	s.notifyKnowledgeChanged(r.Context())

	// 返回更新后的实体: 版本号由存储层自增, 客户端必须拿到真实值,
	// 否则界面上的"版本 2"会和数据库里的"版本 3"长期不一致。
	updated, err := s.cfg.Store.GetQuestion(ctx, tenant, id)
	if err != nil {
		writeJSON(w, http.StatusOK, merged)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	id := r.PathValue("id")
	if err := s.cfg.Store.DeleteQuestion(ctx, platform.Tenant(r.Context()), id); err != nil {
		s.storeErr(w, err, "delete_question")
		return
	}
	s.audit(r.Context(), store.AuditQuestionDelete, id, "删除题目")
	s.notifyKnowledgeChanged(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) decodeQuestion(w http.ResponseWriter, r *http.Request) (store.QuestionItem, bool) {
	var item store.QuestionItem
	if !decodeBody(w, r, 256<<10, &item) {
		return item, false
	}
	// 这里只校验"给了的字段是否合法", 不要求字段齐全 ——
	// 局部更新只需要改一个字段, 要求回传整份对象会把"漏传字段"
	// 变成一次静默失败。必填项在创建接口里单独校验。
	if item.Text != "" && strings.TrimSpace(item.Text) == "" {
		writeError(w, http.StatusBadRequest, "题干不能为空白")
		return item, false
	}
	// 参考要点必须同时有名字与正文: 只有名字的点无法生成追问话术,
	// 只有正文的点无法做覆盖度去重, 两者缺一都会让 RAG 链路退化。
	for _, p := range item.ReferencePoints {
		if strings.TrimSpace(p.Key) == "" || strings.TrimSpace(p.Text) == "" {
			writeError(w, http.StatusBadRequest, "参考要点必须同时填写要点名与表述")
			return item, false
		}
	}
	return item, true
}

func mergeQuestion(cur, patch store.QuestionItem) store.QuestionItem {
	out := cur
	if patch.Stage != "" {
		out.Stage = patch.Stage
	}
	if patch.Competency != "" {
		out.Competency = patch.Competency
	}
	if patch.Text != "" {
		out.Text = patch.Text
	}
	if patch.Keywords != nil {
		out.Keywords = patch.Keywords
	}
	if patch.AntiPatterns != nil {
		out.AntiPatterns = patch.AntiPatterns
	}
	if patch.ReferencePoints != nil {
		out.ReferencePoints = patch.ReferencePoints
	}
	if patch.Difficulty != "" {
		out.Difficulty = patch.Difficulty
	}
	if patch.Importance != "" {
		out.Importance = patch.Importance
	}
	if patch.MaxProbe > 0 {
		out.MaxProbe = patch.MaxProbe
	}
	if patch.Status != "" {
		out.Status = patch.Status
	}
	if patch.Tags != nil {
		out.Tags = patch.Tags
	}
	if patch.Rounds != nil {
		out.Rounds = patch.Rounds
	}
	return out
}

/* ---------------- 检索(把 RAG 暴露成可复核的能力) ---------------- */

func (s *Server) handleRetrievalSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	topK := 5
	if v := r.URL.Query().Get("top_k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topK = n
		}
	}
	corpus := s.knowledge.get(r.Context(), platform.Tenant(r.Context()))
	if corpus == nil {
		writeError(w, http.StatusServiceUnavailable, "知识库未初始化")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	start := time.Now()
	hits, err := corpus.Search(ctx, query, topK)
	elapsed := time.Since(start)
	if err != nil {
		s.metrics.Retrievals.WithLabelValues("error").Inc()
		writeError(w, http.StatusBadRequest, "检索失败: "+err.Error())
		return
	}
	result := "miss"
	if len(hits) > 0 {
		result = "hit"
	}
	s.metrics.Retrievals.WithLabelValues(result).Inc()
	s.metrics.RetrieveMS.WithLabelValues(result).Observe(elapsed.Seconds())

	writeJSON(w, http.StatusOK, map[string]any{
		"query":      query,
		"hits":       hits,
		"elapsed_ms": elapsed.Milliseconds(),
		"pipeline":   "BM25 + 向量 + RRF + 精排",
	})
}

func (s *Server) handleKnowledgeStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.knowledgeStats(r.Context()))
}

func (s *Server) knowledgeStats(ctx context.Context) map[string]any {
	corpus := s.knowledge.get(ctx, platform.Tenant(ctx))
	if corpus == nil {
		return map[string]any{"ready": false}
	}
	stats := corpus.Stats()
	return map[string]any{
		"ready":                     true,
		"questions":                 stats.Questions,
		"reference_points":          stats.Points,
		"documents":                 stats.Documents,
		"embedder":                  stats.Embedder,
		"reranker":                  stats.Reranker,
		"source":                    stats.Source,
		"pipeline":                  stats.Pipeline,
		"built_at":                  stats.BuiltAt,
		"build_ms":                  stats.BuildMS,
		"stages":                    corpus.Stages(),
		"missing_structural_stages": corpus.MissingStructuralStages(),
	}
}

/* ---------------- 面试安排 ---------------- */

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	from, to := time.Time{}, time.Time{}
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	list, err := s.cfg.Store.ListSchedules(ctx, platform.Tenant(r.Context()), from, to)
	if err != nil {
		s.storeErr(w, err, "list_schedules")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": list})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApplicationID string    `json:"application_id"`
		Round         int       `json:"round"`
		ScheduledAt   time.Time `json:"scheduled_at"`
		DurationMin   int       `json:"duration_min"`
		Interviewer   string    `json:"interviewer"`
		Mode          string    `json:"mode"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())

	app, err := s.cfg.Store.GetApplication(ctx, tenant, req.ApplicationID)
	if err != nil {
		s.storeErr(w, err, "get_application")
		return
	}
	if req.Round < 1 || req.Round > 5 {
		writeError(w, http.StatusBadRequest, "轮次必须在 1 到 5 之间")
		return
	}
	job, err := s.cfg.Store.GetJob(ctx, tenant, app.JobID)
	if err != nil {
		s.storeErr(w, err, "get_job")
		return
	}
	spec, ok := job.RoundSpecOf(req.Round)
	if !ok {
		writeError(w, http.StatusBadRequest, "该职位没有这一轮的编排")
		return
	}
	if req.Mode == "" {
		req.Mode = spec.Mode
	}
	if req.DurationMin <= 0 {
		req.DurationMin = spec.Minutes
	}
	if req.ScheduledAt.IsZero() {
		writeError(w, http.StatusBadRequest, "必须指定面试时间")
		return
	}

	schedule := store.Schedule{
		ID: newID("sch"), TenantID: tenant, ApplicationID: app.ID, JobID: job.ID,
		CandidateRef: app.CandidateRef, Round: req.Round, Mode: req.Mode,
		ScheduledAt: req.ScheduledAt.UTC(), DurationMin: req.DurationMin,
		Interviewer: defaultString(req.Interviewer, spec.Name), Status: "pending",
	}
	if err := s.cfg.Store.CreateSchedule(ctx, schedule); err != nil {
		s.storeErr(w, err, "create_schedule")
		return
	}

	// 同步到管道: 看板上立刻能看到"第 N 轮已排期"。
	if rt, ok := app.RoundOf(req.Round); ok {
		at := schedule.ScheduledAt
		rt.Status = "scheduled"
		rt.ScheduledAt = &at
		rt.Mode = req.Mode
		rt.Interviewer = schedule.Interviewer
		for i := range app.Rounds {
			if app.Rounds[i].Round == req.Round {
				app.Rounds[i] = rt
			}
		}
		if app.CurrentRound == 0 {
			app.CurrentRound = req.Round
		}
		if app.Stage == store.StageScreening {
			app.Stage = store.StageAIInterview
		}
		if err := s.cfg.Store.UpdateApplication(ctx, app); err != nil {
			s.logger.ErrorContext(r.Context(), "同步管道失败",
				append(platform.AuditAttrs(r.Context()), slog.Any("err", err))...)
		}
	}

	s.audit(r.Context(), store.AuditScheduleUpsert, schedule.ID,
		fmt.Sprintf("安排第 %d 轮面试 %s %s", req.Round, req.Mode, schedule.ScheduledAt.Format(time.RFC3339)))
	writeJSON(w, http.StatusOK, schedule)
}

// handleStartSchedule 把一条面试安排变成真实的面试会话, 并返回候选人链接。
//
// 这是招聘流程里最关键的一次状态跃迁: "计划"变成"正在发生"。
// 所有需要候选人同意的合规门槛都在创建会话时才真正生效, 因此这里
// 不能绕开 handleCreateSession 的逻辑, 而是复用同一段实现。
func (s *Server) handleStartSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant := platform.Tenant(ctx)
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()

	schedule, err := s.cfg.Store.GetSchedule(sctx, tenant, r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "get_schedule")
		return
	}
	app, err := s.cfg.Store.GetApplication(sctx, tenant, schedule.ApplicationID)
	if err != nil {
		s.storeErr(w, err, "get_application")
		return
	}
	job, err := s.cfg.Store.GetJob(sctx, tenant, app.JobID)
	if err != nil {
		s.storeErr(w, err, "get_job")
		return
	}
	spec, _ := job.RoundSpecOf(schedule.Round)
	candidate, err := s.cfg.Store.GetCandidate(sctx, tenant, app.CandidateRef)
	if err != nil {
		s.storeErr(w, err, "get_candidate")
		return
	}

	sess, token, expiresAt, err := s.createSession(ctx, createSessionInput{
		Round:         schedule.Round,
		Minutes:       schedule.DurationMin,
		Position:      job.Title,
		Company:       defaultString(candidate.Name, "候选人"),
		CandidateName: candidate.Name,
		CandidateRef:  app.CandidateRef,
		Interviewer:   schedule.Interviewer,
		ApplicationID: app.ID,
		ResumeText:    candidate.ResumeSource,
		ResumeJSON:    candidate.ResumeJSON,
	})
	if err != nil {
		s.storeErr(w, err, "create_session")
		return
	}

	schedule.Status = "running"
	schedule.SessionID = sess.ID
	if err := s.cfg.Store.UpdateSchedule(sctx, schedule); err != nil {
		s.logger.ErrorContext(ctx, "更新安排状态失败",
			append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
	}
	if rt, ok := app.RoundOf(schedule.Round); ok {
		rt.Status = "running"
		rt.SessionID = sess.ID
		rt.Mode = schedule.Mode
		rt.Interviewer = schedule.Interviewer
		for i := range app.Rounds {
			if app.Rounds[i].Round == schedule.Round {
				app.Rounds[i] = rt
			}
		}
		app.CurrentRound = schedule.Round
		if err := s.cfg.Store.UpdateApplication(sctx, app); err != nil {
			s.logger.ErrorContext(ctx, "同步管道失败",
				append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
		}
	}

	s.audit(ctx, store.AuditScheduleUpsert, schedule.ID, "开始面试, 会话 "+sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    sess.ID,
		"session_token": token,
		"expires_at":    expiresAt,
		"ws_url":        "/ws/interview/" + sess.ID,
		"candidate_url": "/?s=" + sess.ID + "&t=" + token,
		"mode":          defaultString(schedule.Mode, spec.Mode),
		"round":         sess.Round,
	})
}

/* ---------------- 管道分析 ---------------- */

func (s *Server) handlePipelineAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())

	apps, err := s.cfg.Store.ListApplications(ctx, tenant, "")
	if err != nil {
		s.storeErr(w, err, "list_applications")
		return
	}
	jobs, err := s.cfg.Store.ListJobs(ctx, tenant)
	if err != nil {
		s.storeErr(w, err, "list_jobs")
		return
	}
	base, err := s.cfg.Store.Analytics(ctx, tenant)
	if err != nil {
		s.storeErr(w, err, "analytics")
		return
	}

	byStage := make(map[string]int)
	roundFunnel := make(map[int]map[string]int)
	var totalScore, scored int
	for _, a := range apps {
		byStage[string(a.Stage)]++
		for _, rt := range a.Rounds {
			if roundFunnel[rt.Round] == nil {
				roundFunnel[rt.Round] = map[string]int{}
			}
			roundFunnel[rt.Round][rt.Status]++
			if rt.Status == "finished" && rt.Score > 0 {
				totalScore += rt.Score
				scored++
			}
		}
	}
	avg := 0.0
	if scored > 0 {
		avg = float64(totalScore) / float64(scored)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":               len(jobs),
		"open_jobs":          countOpenJobs(jobs),
		"applications":       len(apps),
		"by_stage":           byStage,
		"round_funnel":       roundFunnel,
		"avg_round_score":    avg,
		"scored_rounds":      scored,
		"interview_sessions": base,
	})
}

func countOpenJobs(jobs []store.Job) int {
	n := 0
	for _, j := range jobs {
		if j.Status == store.JobOpen {
			n++
		}
	}
	return n
}

/* ---------------- 面试结论回流到管道 ---------------- */

// syncPipeline 把一场面试的结论写回招聘管道。
//
// 这里有一条明确的产品边界: **回流的是"事实", 不是"决定"**。
// 系统会记录"第 2 轮已完成、AI 建议 HIRE、综合 82 分", 但不会因此
// 自动把候选人推进到下一轮, 更不会自动淘汰 —— 自动淘汰会带来误伤与
// 歧视风险, 而招聘决策必须署在具体的人名下。
//
// 唯一允许自动变动的阶段是 screening -> ai_interview: 候选人确实已经
// 参加过 AI 面试了, 这是对已发生事实的记录, 而不是对人的评价。
// 即使写回失败也不影响面试结论: 面试已经结束、报告已经落库,
// 回流只是让看板更及时。因此失败只记日志, 不回滚、不报错。
func (s *Server) syncPipeline(ctx context.Context, sess store.Session, score int, recommendation string) {
	if sess.ApplicationID == "" {
		return
	}
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()

	app, err := s.cfg.Store.GetApplication(sctx, sess.TenantID, sess.ApplicationID)
	if err != nil {
		s.logger.WarnContext(ctx, "回流面试结论失败: 读不到投递",
			append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
		return
	}

	changed := false
	for i := range app.Rounds {
		if app.Rounds[i].Round != sess.Round {
			continue
		}
		app.Rounds[i].Status = "finished"
		app.Rounds[i].SessionID = sess.ID
		app.Rounds[i].Score = score
		app.Rounds[i].Recommendation = recommendation
		changed = true
	}
	if app.Stage == store.StageScreening {
		app.Stage = store.StageAIInterview
		changed = true
	}
	if !changed {
		return
	}
	if err := s.cfg.Store.UpdateApplication(sctx, app); err != nil {
		s.logger.WarnContext(ctx, "回流面试结论失败: 更新投递出错",
			append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
		return
	}

	// 把同一轮的面试安排标记为完成, 否则日历上会一直挂着"进行中"。
	schedules, err := s.cfg.Store.ListSchedules(sctx, sess.TenantID, time.Time{}, time.Time{})
	if err != nil {
		return
	}
	for _, sc := range schedules {
		if sc.SessionID != sess.ID {
			continue
		}
		sc.Status = "done"
		if err := s.cfg.Store.UpdateSchedule(sctx, sc); err != nil {
			s.logger.WarnContext(ctx, "更新面试安排状态失败",
				append(platform.AuditAttrs(ctx), slog.Any("err", err))...)
		}
	}
}
