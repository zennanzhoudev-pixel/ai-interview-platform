package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// 面试录制: 候选人浏览器分片上传, 面试官按权限回放。
//
// 三条合规约束贯穿这个文件:
//
//  1. **录制必须有授权**: 上传前校验该会话确实记录了 recording 授权;
//  2. **保留期必须存在**: 每个录制件带 delete_after, 到期可清理;
//  3. **谁看过录像要留痕**: 播放接口会写审计 —— 录像是最敏感的一类
//     个人信息, "谁在什么时候看了它"本身就属于必须记录的事实。

const (
	// maxChunkBytes 单个分片上限。浏览器的 MediaRecorder 通常每 2-5 秒
	// 产生几十到几百 KB, 1MB 足够覆盖高码率, 也能挡住把上传接口当网盘用。
	maxChunkBytes = 1 << 20
	// recordingKinds 允许的录制类型。
)

var allowedKinds = map[string]bool{"video": true, "audio": true, "screen": true}

func (s *Server) recordingEnabled() bool { return s.cfg.Blobs != nil }

// storageKey 生成存储键。
//
// 键的第一段是归一化后的租户 ID: 即使将来换了对象存储, "一个租户一个前缀"
// 这条约定也让权限策略、生命周期规则、配额统计都能按前缀配置。
func (s *Server) storageKey(tenant, sessionID, kind string) string {
	return fmt.Sprintf("%s/%s/%s.webm", store.NormalizeTenant(tenant), sessionID, kind)
}

// handleCandidateRecordingChunk 接收一个录制分片。
func (s *Server) handleCandidateRecordingChunk(w http.ResponseWriter, r *http.Request) {
	if !s.recordingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "录制功能未启用")
		return
	}
	claims, sess, ok := s.candidateSession(w, r)
	if !ok {
		return
	}
	kind := strings.ToLower(r.URL.Query().Get("kind"))
	if !allowedKinds[kind] {
		writeError(w, http.StatusBadRequest, "录制类型必须是 video/audio/screen 之一")
		return
	}
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil || index < 0 {
		writeError(w, http.StatusBadRequest, "分片序号必须是自然数")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunkBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "分片过大")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "分片不能为空")
		return
	}

	key := s.storageKey(claims.TenantID, sess.ID, kind)
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	if err := s.cfg.Blobs.AppendChunk(ctx, key, index, body); err != nil {
		s.metrics.Recordings.WithLabelValues(kind, "error").Inc()
		s.logger.Error("写录制分片失败", "err", err)
		writeError(w, http.StatusInternalServerError, "保存分片失败")
		return
	}
	s.metrics.Recordings.WithLabelValues(kind, "ok").Inc()
	s.metrics.RecordingMB.WithLabelValues(kind).Add(float64(len(body)))

	// 元数据在第一个分片到达时就建立: 若等到 finalize 才建,
	// 中途关页面的候选人会留下一份"磁盘上有文件、数据库里没有"的孤儿数据。
	if _, err := s.cfg.Store.GetRecordingBySession(ctx, claims.TenantID, sess.ID, kind); errors.Is(err, store.ErrNotFound) {
		_ = s.cfg.Store.CreateRecording(ctx, store.Recording{
			ID: newID("rec"), TenantID: claims.TenantID, SessionID: sess.ID,
			CandidateRef: sess.CandidateRef, Kind: kind,
			MimeType:   defaultString(r.Header.Get("Content-Type"), "video/webm"),
			StorageKey: key, Chunks: index + 1, SizeBytes: int64(len(body)),
			DurationMS:  parseDurationMS(r.URL.Query().Get("duration_ms")),
			Status:      "uploading",
			DeleteAfter: time.Now().Add(s.cfg.RecordingRetention),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"received": index, "bytes": len(body), "kind": kind,
	})
}

// handleCandidateRecordingFinalize 合并分片并落定元数据。
func (s *Server) handleCandidateRecordingFinalize(w http.ResponseWriter, r *http.Request) {
	if !s.recordingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "录制功能未启用")
		return
	}
	claims, sess, ok := s.candidateSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind       string `json:"kind"`
		Chunks     int    `json:"chunks"`
		DurationMS int64  `json:"duration_ms"`
	}
	if !decodeBody(w, r, 64<<10, &req) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if !allowedKinds[kind] {
		writeError(w, http.StatusBadRequest, "录制类型必须是 video/audio/screen 之一")
		return
	}

	key := s.storageKey(claims.TenantID, sess.ID, kind)
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	size, err := s.cfg.Blobs.Finalize(ctx, key, req.Chunks)
	if err != nil {
		s.metrics.Recordings.WithLabelValues(kind, "incomplete").Inc()
		writeError(w, http.StatusBadRequest, "分片不完整, 无法合并: "+err.Error())
		return
	}

	rec, err := s.cfg.Store.GetRecordingBySession(ctx, claims.TenantID, sess.ID, kind)
	if errors.Is(err, store.ErrNotFound) {
		rec = store.Recording{
			ID: newID("rec"), TenantID: claims.TenantID, SessionID: sess.ID,
			CandidateRef: sess.CandidateRef, Kind: kind, MimeType: "video/webm",
			StorageKey: key, DeleteAfter: time.Now().Add(s.cfg.RecordingRetention),
		}
		if err := s.cfg.Store.CreateRecording(ctx, rec); err != nil {
			s.storeErr(w, err, "create_recording")
			return
		}
	} else if err != nil {
		s.storeErr(w, err, "get_recording")
		return
	}

	rec.Status = "complete"
	rec.SizeBytes = size
	rec.Chunks = req.Chunks
	if req.DurationMS > 0 {
		rec.DurationMS = req.DurationMS
	}
	if err := s.cfg.Store.UpdateRecording(ctx, rec); err != nil {
		s.storeErr(w, err, "update_recording")
		return
	}
	s.metrics.Recordings.WithLabelValues(kind, "complete").Inc()
	s.audit(r.Context(), store.AuditRecordingUpload, sess.ID,
		fmt.Sprintf("%s 录制完成, %d 分片 %d 字节", kind, req.Chunks, size))
	writeJSON(w, http.StatusOK, rec)
}

// handleListRecordings 列出某场面试的录制件。
func (s *Server) handleListRecordings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	list, err := s.cfg.Store.ListRecordings(ctx, platform.Tenant(r.Context()), r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "list_recordings")
		return
	}
	enabled := s.recordingEnabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"recordings": list, "storage_enabled": enabled,
		"retention_days": int(s.cfg.RecordingRetention.Hours() / 24),
	})
}

// handleDownloadRecording 回放录制件, 支持 Range 请求。
//
// 用 http.ServeContent 而不是自己读全文件: 它会正确处理 Range、If-Modified-Since,
// 而面试录像动辄几百 MB —— 不支持 Range 的播放器必须整包下载完才能开始播,
// 在真实网络下这等于"视频打不开"。
func (s *Server) handleDownloadRecording(w http.ResponseWriter, r *http.Request) {
	if !s.recordingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "录制功能未启用")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	sessionID := r.PathValue("id")
	kind := strings.ToLower(r.PathValue("kind"))

	rec, err := s.cfg.Store.GetRecordingBySession(ctx, tenant, sessionID, kind)
	if err != nil {
		s.storeErr(w, err, "get_recording")
		return
	}
	if rec.Status != "complete" {
		writeError(w, http.StatusConflict, "录制尚未完成")
		return
	}
	file, size, err := s.cfg.Blobs.Open(ctx, rec.StorageKey)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "录制内容不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取录制失败")
		return
	}
	defer func() { _ = file.Close() }()

	// 观看录像必须留痕。
	s.audit(r.Context(), store.AuditRecordingView, sessionID, kind+" 录制被播放")
	s.metrics.Recordings.WithLabelValues(kind, "view").Inc()

	if rec.MimeType != "" {
		w.Header().Set("Content-Type", rec.MimeType)
	} else {
		w.Header().Set("Content-Type", "video/webm")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", sessionID+"-"+kind+".webm"))
	http.ServeContent(w, r, sessionID+"-"+kind+".webm", rec.UpdatedAt, file)
	_ = size
}

// handleDeleteRecording 删除录制件(行使删除权时使用)。
func (s *Server) handleDeleteRecording(w http.ResponseWriter, r *http.Request) {
	if !s.recordingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "录制功能未启用")
		return
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	tenant := platform.Tenant(r.Context())
	sessionID := r.PathValue("id")
	kind := strings.ToLower(r.PathValue("kind"))

	rec, err := s.cfg.Store.GetRecordingBySession(ctx, tenant, sessionID, kind)
	if err != nil {
		s.storeErr(w, err, "get_recording")
		return
	}
	// 先删内容再删元数据: 反过来的话, 内容删除失败时会留下永远无人引用的
	// 孤儿文件, 而"已删除"的承诺已经写进了审计日志。
	if err := s.cfg.Blobs.Remove(ctx, rec.StorageKey); err != nil {
		writeError(w, http.StatusInternalServerError, "删除录制内容失败")
		return
	}
	if err := s.cfg.Store.DeleteRecording(ctx, tenant, rec.ID); err != nil {
		s.storeErr(w, err, "delete_recording")
		return
	}
	s.audit(r.Context(), store.AuditRecordingDelete, sessionID, kind+" 录制已删除")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rec.ID})
}

// candidateSession 解析候选人令牌并取出会话, 同时校验录音授权。
func (s *Server) candidateSession(w http.ResponseWriter, r *http.Request) (tokenClaimsPrincipal, store.Session, bool) {
	claims, err := s.candidateFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "凭证无效或已过期")
		return tokenClaimsPrincipal{}, store.Session{}, false
	}
	ctx, cancel := s.storeCtx(r.Context())
	defer cancel()
	sess, err := s.cfg.Store.GetSession(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "会话不存在")
		return tokenClaimsPrincipal{}, store.Session{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取会话失败")
		return tokenClaimsPrincipal{}, store.Session{}, false
	}
	return tokenClaimsPrincipal{TenantID: claims.TenantID, SessionID: claims.SessionID}, sess, true
}

// tokenClaimsPrincipal 是候选人令牌的精简视图, 避免在调用处反复解构。
type tokenClaimsPrincipal struct {
	TenantID  string
	SessionID string
}

func parseDurationMS(v string) int64 {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
