package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/platform"
)

// 系统自检接口。
//
// 它回答的是运维最常问、而普通健康检查答不了的问题: "现在这套服务到底
// 连上了什么, 又降级了什么?"。健康检查只告诉你进程活着, 而一家企业
// 真正需要知道的是"评分走的是大模型还是规则回退""语音是真连还是前端兜底"
// —— 这些差异不会让接口报错, 但会直接改变面试的质量。

// ProviderPing 是可被自检探测的上游能力。
//
// 用接口而不是直接依赖具体实现: 接入层不应该知道"LLM 是 OpenAI 还是
// 通义千问", 它只需要知道"有个东西可以 ping, ping 不通就标红"。
type ProviderPing interface {
	Ping(ctx context.Context) error
}

// ErrPingUnsupported 表示该能力无法被一次探针验证。
//
// 语音识别就属于这一类: 它的正确性取决于真实音频、采样率与网络条件,
// 用一段静音去"ping"只会得到一个假的绿灯。宁可如实显示"已配置但
// 未验证", 也不要给出一个会让人误以为链路已通的勾。
var ErrPingUnsupported = errors.New("api: 该能力无法用探针验证")

// handleProviderDiagnostics 返回上游能力的连接状态。
//
// 默认只报告"是否配置"(零成本, 可被监控系统高频调用);
// 带 live=1 时才真正发起请求 —— 真实探测要花时间也要花钱,
// 不能挂在探针上被每 5 秒调一次。
func (s *Server) handleProviderDiagnostics(w http.ResponseWriter, r *http.Request) {
	live := r.URL.Query().Get("live") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	items := []map[string]any{
		s.providerStatus(ctx, live, "评分(主)", "llm_primary", s.cfg.Pingers["llm"]),
		s.providerStatus(ctx, live, "评分(复核)", "llm_secondary", s.cfg.Pingers["llm_secondary"]),
		s.providerStatus(ctx, live, "语义向量", "embedding", s.cfg.Pingers["embedding"]),
		s.providerStatus(ctx, live, "语音识别(ASR)", "asr", s.cfg.Pingers["asr"]),
		s.providerStatus(ctx, live, "语音合成(TTS)", "tts", s.cfg.Pingers["tts"]),
	}

	storeDesc := defaultString(s.cfg.StoreKind, "内存(重启丢数据, 仅本地演示)")
	checkpoint := s.cfg.Checkpoint != nil
	sandboxStatus := map[string]any{
		"name": "sandbox", "label": "编程判题沙箱", "configured": s.cfg.Sandbox != nil,
	}
	if s.cfg.Sandbox != nil {
		sandboxStatus["engine"] = s.cfg.Sandbox.Name()
		sandboxStatus["isolated"] = s.cfg.Sandbox.Isolated()
		if !s.cfg.Sandbox.Isolated() {
			sandboxStatus["note"] = "未隔离: 仅限本地开发, 生产必须配置容器运行时"
		}
	}
	recordingStatus := map[string]any{
		"name": "recording", "label": "面试录制存储", "configured": s.cfg.Blobs != nil,
		"retention_days": int(s.cfg.RecordingRetention.Hours() / 24),
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"live":        live,
		"providers":   items,
		"sandbox":     sandboxStatus,
		"recording":   recordingStatus,
		"knowledge":   s.knowledgeStats(ctx),
		"store":       map[string]any{"configured": true, "kind": storeDesc},
		"checkpoint":  map[string]any{"configured": checkpoint},
		"auth":        map[string]any{"required": s.cfg.RequireAuth, "tenant": platform.Tenant(ctx)},
		"ice_servers": s.iceServers(),
	})
}

func (s *Server) providerStatus(ctx context.Context, live bool, label, key string, ping ProviderPing) map[string]any {
	out := map[string]any{
		"name":       key,
		"label":      label,
		"configured": ping != nil,
	}
	if ping == nil {
		out["state"] = "not_configured"
		out["note"] = "未配置, 已按降级策略运行"
		return out
	}
	if !live {
		out["state"] = "configured"
		return out
	}
	start := time.Now()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ping.Ping(pingCtx); err != nil {
		if errors.Is(err, ErrPingUnsupported) {
			out["state"] = "configured_unverified"
			out["note"] = "已配置, 但该能力只能在真实面试中验证"
		} else {
			out["state"] = "error"
			out["error"] = err.Error()
		}
	} else {
		out["state"] = "ok"
	}
	out["latency_ms"] = time.Since(start).Milliseconds()
	return out
}

// handleICEServers 返回 WebRTC 的 ICE 配置。
func (s *Server) handleICEServers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ice_servers": s.iceServers()})
}

// notifyKnowledgeChanged 在题库变更后重建该租户的知识库。
//
// 同步重建(在请求内完成)而不是异步: 重建一份几百条目的索引是毫秒级操作,
// 而异步会让"我改完题目立刻开一场面试"用上旧索引 —— 这种偶发不一致
// 极难复现, 也极难被信任。
func (s *Server) notifyKnowledgeChanged(ctx context.Context) {
	tenant := platform.Tenant(ctx)
	if tenant == "" || s.knowledge == nil {
		return
	}
	s.knowledgeRebuildMu.Lock()
	defer s.knowledgeRebuildMu.Unlock()

	// 先失效再重建: 顺序反过来的话, 构建失败会留下旧索引继续被使用,
	// 而管理界面已经显示"发布成功"。
	s.knowledge.invalidate(tenant)
	buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	corpus := s.knowledge.get(buildCtx, tenant)
	if corpus == nil {
		s.logger.Error("重建知识库失败, 题库改动尚未生效", "tenant", tenant)
		return
	}
	stats := corpus.Stats()
	s.logger.InfoContext(ctx, "知识库已重建",
		"tenant", tenant, "questions", stats.Questions, "points", stats.Points)
}
