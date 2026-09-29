package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/sandbox"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 编程判题接口。
//
// 二面(编程与实战)在真实招聘里是"现场写代码 + 跑用例"。这条链路有两个
// 容易被做错的地方, 这里都显式处理了:
//
//  1. **隔离等级必须回传**: 响应里带 isolated 字段。降级到本机执行时,
//     调用方与界面必须能看到"这次执行不具备隔离能力", 而不是把它当成
//     一个和容器执行等价的结果;
//  2. **隐藏用例不回传期望输出**: 否则候选人提交一段"打印期望值"的代码
//     就能全绿。这在判题系统里是最常见、也最容易上线的一个洞。

type codeRunRequest struct {
	Language  string             `json:"language"`
	Code      string             `json:"code"`
	Stdin     string             `json:"stdin"`
	TimeoutMS int                `json:"timeout_ms"`
	TestCases []sandbox.TestCase `json:"test_cases"`
}

// maxCasesPerRun 限制单次判题的用例数。
//
// 每个用例都是一次真实进程启动, 用例数不设上限等于把"一次请求"
// 放大成"一次压测"。20 个用例足够覆盖一道面试题的正常规模。
const maxCasesPerRun = 20

func (s *Server) handleCodeRun(w http.ResponseWriter, r *http.Request) {
	s.runCode(w, r, platform.Tenant(r.Context()), "")
}

// handleCandidateCodeRun 让候选人在面试中运行自己的代码。
//
// 候选人没有 API Key, 因此这条接口用会话令牌认证 —— 判题是面试过程的一部分,
// 不是管理操作。
func (s *Server) handleCandidateCodeRun(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		return
	}
	s.runCode(w, r, claims.TenantID, claims.SessionID)
}

func (s *Server) runCode(w http.ResponseWriter, r *http.Request, tenant, sessionID string) {
	if s.cfg.Sandbox == nil {
		writeError(w, http.StatusServiceUnavailable, "判题沙箱未启用")
		return
	}
	var req codeRunRequest
	if !decodeBody(w, r, 1<<20, &req) {
		return
	}
	if len(req.TestCases) > maxCasesPerRun {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("单次最多提交 %d 个用例", maxCasesPerRun))
		return
	}
	if _, err := sandbox.LookupLanguage(req.Language); err != nil {
		writeError(w, http.StatusBadRequest, "不支持的语言")
		return
	}
	if len(req.Code) == 0 {
		writeError(w, http.StatusBadRequest, "代码不能为空")
		return
	}

	timeout := sandbox.DefaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	// 判题的总预算比单个用例的预算宽松一些, 但仍必须存在: 没有总预算时,
	// 20 个用例 × 6 秒 = 2 分钟的请求会一直占着连接与沙箱并发额度。
	ctx, cancel := context.WithTimeout(r.Context(), timeout*time.Duration(maxInt(1, len(req.TestCases)))+5*time.Second)
	defer cancel()

	start := time.Now()
	runReq := sandbox.Request{
		Language: req.Language, Code: req.Code, Stdin: req.Stdin, Timeout: timeout,
	}

	isolated := s.cfg.Sandbox.Isolated()
	language := req.Language
	status := "ok"

	if len(req.TestCases) == 0 {
		result, err := s.cfg.Sandbox.Run(ctx, runReq)
		if err != nil {
			status = "error"
			s.observeSandbox(language, status, isolated, time.Since(start))
			s.writeSandboxError(w, err)
			return
		}
		if !result.Passed() {
			status = "failed"
		}
		s.observeSandbox(language, status, isolated, time.Since(start))
		s.auditCodeRun(r, tenant, sessionID, language, status, isolated)
		writeJSON(w, http.StatusOK, map[string]any{
			"result":   result,
			"isolated": isolated,
			"engine":   s.cfg.Sandbox.Name(),
			"warning":  s.sandboxWarning(),
		})
		return
	}

	cases, last := sandbox.Judge(ctx, s.cfg.Sandbox, runReq, req.TestCases)
	passed := 0
	for _, c := range cases {
		if c.Passed {
			passed++
		}
	}
	if passed != len(cases) {
		status = "failed"
	}
	s.observeSandbox(language, status, isolated, time.Since(start))
	s.auditCodeRun(r, tenant, sessionID, language, status, isolated)

	writeJSON(w, http.StatusOK, map[string]any{
		"cases":    cases,
		"passed":   passed,
		"total":    len(cases),
		"result":   last,
		"isolated": isolated,
		"engine":   s.cfg.Sandbox.Name(),
		"warning":  s.sandboxWarning(),
	})
}

func (s *Server) observeSandbox(language, status string, isolated bool, elapsed time.Duration) {
	s.metrics.SandboxRuns.WithLabelValues(language, status, boolLabel(isolated)).Inc()
	s.metrics.SandboxMS.WithLabelValues(language).Observe(elapsed.Seconds())
}

// writeSandboxError 把沙箱的失败翻译成对候选人可理解的响应。
func (s *Server) writeSandboxError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandbox.ErrUnsupportedLanguage):
		writeError(w, http.StatusBadRequest, "不支持的语言")
	case errors.Is(err, sandbox.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "提交的代码过大")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "判题超时, 请检查是否存在死循环")
	default:
		// 执行环境自身的问题(镜像缺失、工具链没装)不是候选人的错,
		// 因此返回 503 而不是 400 —— 前端据此提示"判题服务不可用"。
		s.logger.Error("沙箱执行失败", "err", err)
		writeError(w, http.StatusServiceUnavailable, "判题服务暂时不可用")
	}
}

// sandboxWarning 在未隔离时返回一句明确的告警。
func (s *Server) sandboxWarning() string {
	if s.cfg.Sandbox == nil || s.cfg.Sandbox.Isolated() {
		return ""
	}
	return "当前判题未在隔离环境中执行(本机模式), 仅限本地开发; 生产必须配置容器运行时"
}

func (s *Server) auditCodeRun(r *http.Request, tenant, sessionID, language, status string, isolated bool) {
	target := sessionID
	if target == "" {
		target = "sandbox"
	}
	actor := platform.Actor(r.Context())
	if actor == "" {
		actor = "candidate"
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	err := s.cfg.Store.AppendAudit(ctx, store.AuditEntry{
		TenantID: defaultString(tenant, platform.Tenant(r.Context())),
		Actor:    actor,
		Action:   store.AuditCodeRun,
		Target:   target,
		Detail:   fmt.Sprintf("language=%s status=%s isolated=%t", language, status, isolated),
	})
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("append_audit").Inc()
	}
}

// maxInt 返回两个整数中的较大值。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
