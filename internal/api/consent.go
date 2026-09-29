package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/privacy"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 候选人侧的数据处理同意。
//
// 为什么把同意做成"面试开始前的一道硬门槛"而不是创建会话时的一个复选框:
// 复选框是**招聘方**勾的, 而同意必须由**候选人本人**做出。由招聘方代勾
// 会把"我们征得了同意"变成一句没有依据的话, 而系统里却留下了看起来
// 完整的同意记录 —— 这比没有记录更糟。
//
// 因此这里有两条路径, 且都指向同一张表:
//   - ATS 已代收同意: 建会话时写入, 候选人直接进房间;
//   - 招聘方在后台点"开始面试": 会话创建时不写同意, 候选人进房间后
//     必须先确认, 否则 WebSocket 不会开始面试。

// handleCandidateConsent 记录候选人本人的授权。
func (s *Server) handleCandidateConsent(w http.ResponseWriter, r *http.Request) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		return
	}
	var req struct {
		Recording bool `json:"recording"`
		Scoring   bool `json:"scoring"`
	}
	if !decodeBody(w, r, 16<<10, &req) {
		return
	}
	if !req.Recording || !req.Scoring {
		writeError(w, http.StatusBadRequest, "录音与 AI 评分两项都需要明确同意才能开始面试")
		return
	}

	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return
	}

	now := time.Now().UTC()
	ip := privacy.MaskIP(clientIP(r))
	ua := privacy.MaskUserAgent(r.UserAgent())
	for _, scope := range []string{"recording", "scoring"} {
		if err := s.cfg.Store.SaveConsent(ctx, store.Consent{
			TenantID: sess.TenantID, SessionID: sess.ID, CandidateID: sess.CandidateRef,
			Scope: scope, AgreedAt: now, IP: ip, UserAgent: ua,
		}); err != nil {
			s.storeErr(w, err, "save_consent")
			return
		}
	}
	s.audit(r.Context(), store.AuditConsentWrite, sess.ID,
		"候选人本人在面试间确认录音与评分授权")
	writeJSON(w, http.StatusOK, map[string]any{
		"consented": true, "scopes": []string{"recording", "scoring"}, "agreed_at": now,
	})
}

// consentsComplete 判断某个会话是否已取得全部必需授权。
//
// 判定标准是"存在 recording 与 scoring 两条记录", 而不是"有一条记录":
// 只同意录音、不同意评分的情况下开面, 等于在未获授权时处理个人信息。
func (s *Server) consentsComplete(ctx context.Context, tenantID, sessionID string) bool {
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	list, err := s.cfg.Store.ListConsents(sctx, tenantID, sessionID)
	if err != nil {
		// 读不到授权记录时按"未授权"处理(失败关闭), 而不是放行。
		// 这是安全相关的默认值选择: 宁可挡住一场面试, 也不要违规开面。
		return false
	}
	have := map[string]bool{}
	for _, c := range list {
		if !c.AgreedAt.IsZero() {
			have[c.Scope] = true
		}
	}
	return have["recording"] && have["scoring"]
}

// consentScopes 返回已记录的授权范围, 用于候选人界面展示"我同意过什么"。
func (s *Server) consentScopes(ctx context.Context, tenantID, sessionID string) map[string]any {
	sctx, cancel := s.storeCtx(ctx)
	defer cancel()
	list, err := s.cfg.Store.ListConsents(sctx, tenantID, sessionID)
	if err != nil {
		return map[string]any{"required": true, "scopes": []string{}}
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, map[string]any{
			"scope":     c.Scope,
			"agreed_at": c.AgreedAt,
			"source_ip": c.IP,
		})
	}
	return map[string]any{"required": false, "scopes": out}
}
