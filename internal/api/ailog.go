package api

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

// AI 日志: 把一场面试里"系统做了什么判断、依据是什么"按时间串起来。
//
// 为什么需要它: 报告给的是结论, 而人在复核时真正想看的往往是过程 ——
// 这一轮为什么追问、评分依据是哪句原话、哪一步降级了、谁看过、谁改过分。
// 这些信息本来就存在于不同的表里(问答、报告、审计), 但散在各处就没人查;
// 串成一条时间线之后, "事后复盘"才真的可行。
//
// 逐轮的检索快照现在真的落库了(turn.retrieval / probe_focus / probe_reference):
// 追问方向来自 RAG 检索, 而"当时检索到了什么、分数多少"会随这一轮一起保存。
// 这一点很关键 —— 如果只在事后重算, 日志展示的是"现在的结果"而不是
// "当时的依据", 而复核要看的恰恰是后者。

// AILogEntry 是时间线上的一条。
type AILogEntry struct {
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Title   string         `json:"title"`
	Detail  string         `json:"detail,omitempty"`
	TurnID  int            `json:"turn_id,omitempty"`
	Actor   string         `json:"actor,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// buildAILog 组装一场面试的 AI 日志。
func (s *Server) buildAILog(sess store.Session, turns []store.Turn, report *store.Report, audit []store.AuditEntry) []AILogEntry {
	entries := make([]AILogEntry, 0, len(turns)*2+len(audit)+4)

	entries = append(entries, AILogEntry{
		At: sess.CreatedAt, Kind: "session", Title: "面试会话创建",
		Detail: "第 " + intToStr(sess.Round) + " 轮 · " + sess.Position + " · 预算 " + intToStr(sess.Minutes) + " 分钟",
		Payload: map[string]any{
			"session_id": sess.ID, "round": sess.Round, "mode": "video", "status": string(sess.Status),
		},
	})

	for i := range turns {
		t := turns[i]
		at := t.CreatedAt
		if at.IsZero() {
			at = sess.CreatedAt
		}
		kind := "question"
		title := "第 " + intToStr(t.Index) + " 轮提问"
		if t.IsProbe {
			kind = "probe"
			// 追问本身就是要复盘的: 它说明上一轮的回答被判定为"没答全"。
			title = "第 " + intToStr(t.Index) + " 轮追问(上轮未覆盖判定要点)"
		}
		probeEntry := AILogEntry{
			At: at, Kind: kind, Title: title, TurnID: t.Index,
			Detail: t.Question,
			Payload: map[string]any{
				"question_id": t.QuestionID, "competency": t.Competency, "stage": t.Stage,
			},
		}
		if t.IsProbe {
			// 追问的依据与当时的检索命中: 这是"它凭什么问这句"的答案。
			probeEntry.Payload["probe_focus"] = t.ProbeFocus
			probeEntry.Payload["probe_reference"] = t.ProbeReference
			if len(t.RetrievalJSON) > 0 {
				var hits []orchestrator.RetrievalHit
				if json.Unmarshal(t.RetrievalJSON, &hits) == nil {
					probeEntry.Payload["retrieval"] = hits
				}
			}
		}
		entries = append(entries, probeEntry)

		entry := AILogEntry{
			At: at.Add(time.Duration(t.DurationMS) * time.Millisecond), Kind: "answer",
			Title: "候选人作答(用时 " + intToStr(int(t.DurationMS/1000)) + " 秒)", TurnID: t.Index,
			Detail: t.Answer,
		}
		if t.Scored {
			entry.Kind = "score"
			entry.Title = "评分: " + t.Level + "(置信度 " + percent(t.Confidence) + ")"
			entry.Payload = map[string]any{
				"level": t.Level, "level_num": t.LevelNum, "confidence": t.Confidence,
				"degraded_from": t.DegradedFrom,
			}
			// 评分依据必须能逐条核对, 因此把证据原话摊平放进日志。
			if len(t.Verdict) > 0 {
				var v scoring.Verdict
				if json.Unmarshal(t.Verdict, &v) == nil {
					quotes := make([]map[string]any, 0, len(v.Final.Evidence))
					for _, e := range v.Final.Evidence {
						quotes = append(quotes, map[string]any{
							"kind": e.Kind, "quote": e.Quote, "matched": e.Matched,
						})
					}
					entry.Payload["evidence"] = quotes
					entry.Payload["missing"] = v.Final.Missing
					// 评分器名(model 字段)要写进日志: 复核时必须知道
					// "这条分是谁打的", 否则无法判断标准是否变了。
					entry.Payload["scorer"] = v.Final.Model
					if v.Final.DegradedFrom != "" {
						entry.Payload["degraded_reason"] = "主评分器不可用, 已回落到 " + v.Final.DegradedFrom
					}
				}
			}
		}
		entries = append(entries, entry)
	}

	if report != nil {
		entries = append(entries, AILogEntry{
			At: report.CreatedAt, Kind: "report", Title: "生成评估报告",
			Detail: "AI 建议 " + report.Recommendation + " · 综合分 " + intToStr(report.Score) +
				" · 置信度 " + percent(report.Confidence),
			Payload: map[string]any{
				"recommendation": report.Recommendation, "score": report.Score,
				"confidence": report.Confidence,
			},
		})
	}

	for _, a := range audit {
		entries = append(entries, AILogEntry{
			At: a.CreatedAt, Kind: auditKind(a.Action), Title: auditTitle(a.Action),
			Detail: a.Detail, Actor: a.Actor,
			Payload: map[string]any{"action": a.Action, "target": a.Target},
		})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	return entries
}

// auditKind 把审计动作映射成时间线上的分类。
func auditKind(action string) string {
	switch action {
	case store.AuditSessionCreate, store.AuditSessionFinish:
		return "session"
	case store.AuditReportView:
		return "access"
	case store.AuditScoreOverride:
		return "override"
	case store.AuditCodeRun:
		return "code"
	case store.AuditRecordingView, store.AuditRecordingUpload, store.AuditRecordingDelete:
		return "recording"
	case store.AuditObserverJoin:
		return "observer"
	}
	if len(action) > 8 && action[:8] == "proctor." {
		// 反作弊信号: 只记录, 不判定 —— 时间线里也只是如实展示。
		return "proctor"
	}
	return "audit"
}

func auditTitle(action string) string {
	switch action {
	case store.AuditSessionCreate:
		return "会话创建"
	case store.AuditSessionFinish:
		return "会话结束"
	case store.AuditReportView:
		return "有人查看了报告"
	case store.AuditScoreOverride:
		return "人工改分"
	case store.AuditCodeRun:
		return "执行了判题沙箱"
	case store.AuditRecordingView:
		return "播放了面试录像"
	case store.AuditObserverJoin:
		return "人类面试官进入面试间"
	case store.AuditDataExport:
		return "导出候选人数据"
	case store.AuditDataErase:
		return "删除候选人数据"
	}
	if len(action) > 8 && action[:8] == "proctor." {
		return "防作弊信号: " + action[8:]
	}
	return action
}

func percent(v float64) string {
	return intToStr(int(v*100+0.5)) + "%"
}

func intToStr(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
