package orchestrator

import (
	"context"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
)

// ReferencePoint 是参考答案里的一个要点。
type ReferencePoint struct {
	Key  string // 要点名, 用于去重与覆盖度判断
	Text string // 完整表述, 用于生成贴合原文的追问
}

// ReferenceAnswer 是一道题的参考答案与要点。
type ReferenceAnswer struct {
	QuestionID string
	Summary    string
	Points     []ReferencePoint
}

// ReferenceBank 是参考答案库。
//
// 参考答案必须由题目评审流程产出, 而不是让模型现编 —— 追问的"靶子"
// 如果也是模型编的, 整个系统就变成一个自我指涉的循环, 没有任何东西
// 是可复现、可审计的。
type ReferenceBank struct {
	answers []ReferenceAnswer
}

// NewReferenceBank 构造参考答案库。
func NewReferenceBank(answers ...ReferenceAnswer) *ReferenceBank {
	return &ReferenceBank{answers: answers}
}

// ByQuestionID 按题目 ID 取参考答案。
func (b *ReferenceBank) ByQuestionID(id string) (ReferenceAnswer, bool) {
	for _, a := range b.answers {
		if a.QuestionID == id {
			return a, true
		}
	}
	return ReferenceAnswer{}, false
}

// All 返回全部参考答案。
func (b *ReferenceBank) All() []ReferenceAnswer { return b.answers }

// BuildRetriever 把参考答案库的每个要点转成一篇可检索文档, 构造混合检索器。
//
// 检索粒度是"要点"而不是"整道题": 追问要定位到"候选人漏了哪一点",
// 而不是"这道题相关"。每个要点文档的 Meta 记录它属于哪道题。
func (b *ReferenceBank) BuildRetriever(ctx context.Context, emb rag.Embedder, reranker rag.Reranker) (*rag.Retriever, error) {
	r := rag.NewRetriever(emb, reranker)
	for _, ans := range b.answers {
		for _, p := range ans.Points {
			if err := r.AddDoc(ctx, rag.Doc{
				ID:   ans.QuestionID + "::" + p.Key,
				Text: p.Text,
				Meta: map[string]string{"question_id": ans.QuestionID, "point_key": p.Key},
			}); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

// DefaultReferenceBank 返回与 DefaultBank 配套的参考答案库。
func DefaultReferenceBank() *ReferenceBank {
	return NewReferenceBank(
		ReferenceAnswer{
			QuestionID: "q_resume_zset",
			Summary:    "用 ZSet 的 score 承载排序权重, 排名查询走有序结构避免全量排序, 同时要处理内存增长与大 key。",
			Points: []ReferencePoint{
				{Key: "score 排序", Text: "用 ZSet 的 score 承载排序权重, 查询不再全量排序"},
				{Key: "复杂度", Text: "排名查询复杂度降到 O(log n)"},
				{Key: "内存", Text: "内存增长、过期清理与大 key 分片"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_resume_wasm",
			Summary:    "用 WASM 插件运行时复用编译产物, 换取冷启动与隔离能力。",
			Points: []ReferencePoint{
				{Key: "产物复用", Text: "编译产物复用, 避免每次首次对话重新编译"},
				{Key: "隔离", Text: "WASM 沙箱隔离, 比原生插件安全"},
				{Key: "冷启动", Text: "冷启动从 5-8 秒降到 1 秒内"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_resume_flow",
			Summary:    "四阶段流水线做成状态机, 失败只重试当前阶段, 用幂等键避免重复扣费。",
			Points: []ReferencePoint{
				{Key: "状态机", Text: "阶段化状态机, 失败只重试当前阶段"},
				{Key: "幂等", Text: "shot_id 幂等键, 保证重试不重复扣费"},
				{Key: "断点补偿", Text: "断点续跑与定时补偿任务兜底"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_go_gmp",
			Summary:    "GMP 里 P 带本地队列减少锁竞争, work stealing 平衡负载, 抢占基于信号。",
			Points: []ReferencePoint{
				{Key: "本地队列", Text: "P 的本地队列减少全局锁竞争"},
				{Key: "work stealing", Text: "队列空了去别的 P work stealing 平衡负载"},
				{Key: "抢占", Text: "基于信号的抢占"},
				{Key: "用户态", Text: "切换发生在用户态, 省去内核开销"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_go_gc",
			Summary:    "三色标记加混合写屏障, 标记阶段并发执行, STW 只在很短的阶段出现。",
			Points: []ReferencePoint{
				{Key: "三色写屏障", Text: "三色标记 + 混合写屏障"},
				{Key: "并发标记", Text: "标记阶段与用户程序并发执行, 本身不需要 STW"},
				{Key: "STW 变长", Text: "分配速率过高、辅助标记跟不上会让 STW 变长"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_dist_idem",
			Summary:    "用唯一键做去重, 状态机表记录消费状态, 满足最终一致并靠补偿兜底。",
			Points: []ReferencePoint{
				{Key: "唯一键", Text: "业务 ID + 消息 ID 拼唯一键, 靠唯一约束兜底"},
				{Key: "状态机表", Text: "状态机表记录消费状态"},
				{Key: "最终一致", Text: "最终一致 + 定时补偿"},
			},
		},
		ReferenceAnswer{
			QuestionID: "q_arch_limit",
			Summary:    "令牌桶或滑动窗口限流, 热点单独阈值, 容量估算配降级预案, 组件故障时整体放行。",
			Points: []ReferencePoint{
				{Key: "算法取舍", Text: "令牌桶与滑动窗口的取舍"},
				{Key: "热点", Text: "热点接口单独配置阈值"},
				{Key: "容量降级", Text: "容量估算与降级预案, 组件故障时整体放行"},
			},
		},
	)
}
