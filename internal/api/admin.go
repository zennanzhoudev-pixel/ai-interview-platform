package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/auth"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

/* ---------------- API Key 管理 ---------------- */

// keyManager 是密钥库的管理能力。用类型断言而不是把方法塞进 KeyStore 接口:
// 认证路径只需要 Resolve, 管理能力是可选扩展, 塞进同一个接口会让
// "只读密钥库"这种实现被迫实现一堆空方法。
type keyManager interface {
	Add(tenantID, name string, role auth.Role) (string, auth.Principal, error)
	List() []auth.KeyRecord
	Revoke(keyID string) bool
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	km, ok := s.cfg.Keys.(keyManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前密钥库不支持列举")
		return
	}
	tenant := platform.Tenant(r.Context())
	out := make([]auth.KeyRecord, 0)
	for _, rec := range km.List() {
		if rec.TenantID == tenant {
			out = append(out, rec)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	km, ok := s.cfg.Keys.(keyManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前密钥库不支持创建")
		return
	}
	var req struct {
		Name string    `json:"name"`
		Role auth.Role `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	switch req.Role {
	case auth.RoleAdmin, auth.RoleInterviewer, auth.RoleScheduler:
	default:
		writeError(w, http.StatusBadRequest, "角色必须是 admin / interviewer / scheduler 之一")
		return
	}

	tenant := platform.Tenant(r.Context())
	raw, principal, err := km.Add(tenant, defaultString(req.Name, "未命名"), req.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建密钥失败")
		return
	}
	s.audit(r.Context(), store.AuditKeyCreate, principal.KeyID,
		fmt.Sprintf("role=%s name=%s", req.Role, req.Name))

	// 明文只在这一个响应里出现, 之后数据库里只有哈希。
	writeJSON(w, http.StatusOK, map[string]any{
		"key_id":  principal.KeyID,
		"api_key": raw,
		"role":    principal.Role,
		"notice":  "请立即保存, 明文不会再次展示",
	})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	km, ok := s.cfg.Keys.(keyManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "当前密钥库不支持吊销")
		return
	}
	keyID := r.PathValue("id")
	if !km.Revoke(keyID) {
		writeError(w, http.StatusNotFound, "密钥不存在")
		return
	}
	s.audit(r.Context(), store.AuditKeyRevoke, keyID, "密钥已吊销")
	writeJSON(w, http.StatusOK, map[string]any{"key_id": keyID, "status": "revoked"})
}

/* ---------------- 审计 ---------------- */

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	entries, err := s.cfg.Store.ListAudit(sctx, platform.Tenant(ctx), limit)
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("list_audit").Inc()
		writeError(w, http.StatusInternalServerError, "读取审计日志失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

/* ---------------- 数据看板 ---------------- */

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	stats, err := s.cfg.Store.Analytics(sctx, platform.Tenant(ctx))
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("analytics").Inc()
		writeError(w, http.StatusInternalServerError, "统计失败")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

/* ---------------- 数据主体权利(个保法: 可携带权 / 删除权) ---------------- */

func (s *Server) handleExportCandidate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("ref")
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()

	bundle, err := s.cfg.Store.ExportCandidate(sctx, platform.Tenant(ctx), ref)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "没有该候选人的数据")
		return
	}
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("export_candidate").Inc()
		writeError(w, http.StatusInternalServerError, "导出失败")
		return
	}
	s.audit(ctx, store.AuditDataExport, ref, "导出候选人数据")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="candidate-export.json"`)
	_ = json.NewEncoder(w).Encode(bundle)
}

func (s *Server) handleEraseCandidate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("ref")
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()

	erased, err := s.cfg.Store.EraseCandidate(sctx, platform.Tenant(ctx), ref)
	if err != nil {
		s.metrics.StoreErrors.WithLabelValues("erase_candidate").Inc()
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	// 审计日志不随候选人数据一起删除: 它记录的是"系统发生过什么",
	// 属于平台自身的合规证据, 且不含候选人内容。这一点需要在隐私政策里写明。
	s.audit(ctx, store.AuditDataErase, ref,
		fmt.Sprintf("删除 %d 场会话及其问答/报告/授权记录", erased))
	writeJSON(w, http.StatusOK, map[string]any{
		"candidate_ref": ref, "erased_sessions": erased,
		"notice": "审计日志中保留的删除记录不含候选人内容",
	})
}
