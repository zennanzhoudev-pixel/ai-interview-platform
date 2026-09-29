package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/orchestrator"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/store"
)

func buildCorpus(t *testing.T, tenantItems []store.QuestionItem) *Corpus {
	t.Helper()
	c, err := Build(context.Background(), tenantItems, rag.NewHashingEmbedder(256), rag.NewLocalReranker())
	if err != nil {
		t.Fatalf("构建知识库失败: %v", err)
	}
	return c
}

func TestBuildIndexesEveryReferencePoint(t *testing.T) {
	c := buildCorpus(t, nil)
	stats := c.Stats()
	if stats.Questions < 10 {
		t.Fatalf("内置题库应至少有 10 道题, 实际 %d", stats.Questions)
	}
	if stats.Points == 0 {
		t.Fatal("参考要点应被索引")
	}
	// 每个要点 + 每道题的正文各一篇文档。
	if stats.Documents != stats.Points+stats.Questions {
		t.Fatalf("文档数应为 要点数+题目数: %d != %d + %d",
			stats.Documents, stats.Points, stats.Questions)
	}
	if !strings.Contains(stats.Pipeline, "RRF") {
		t.Fatalf("应如实声明检索链路: %q", stats.Pipeline)
	}
	if stats.Embedder == "" || stats.Reranker == "" {
		t.Fatalf("应记录嵌入器与精排器: %+v", stats)
	}
}

func TestTenantQuestionsTakePrecedenceAndDraftsAreExcluded(t *testing.T) {
	tenant := []store.QuestionItem{
		{
			ID: "t_q_greet", TenantID: "acme", Stage: string(orchestrator.StageGreeting),
			Text: "欢迎参加云杉科技的面试, 请先做个自我介绍。", Status: "published",
			ReferencePoints: []store.ReferencePoint{{Key: "结构", Text: "自我介绍有清晰的结构"}},
		},
		{
			ID: "t_q_draft", TenantID: "acme", Stage: string(orchestrator.StageTechFundamental),
			Text: "这是一道还没评审的草稿题。", Status: "draft",
		},
		{
			ID: "t_q_retired", TenantID: "acme", Stage: string(orchestrator.StageTechFundamental),
			Text: "这是一道已下线的题。", Status: "retired",
		},
	}
	c := buildCorpus(t, tenant)

	// 草稿与下线题绝不能进入面试。
	if _, ok := c.Bank().ByID("t_q_draft"); ok {
		t.Fatal("草稿题不应进入题库")
	}
	if _, ok := c.Bank().ByID("t_q_retired"); ok {
		t.Fatal("已下线题不应进入题库")
	}
	// 租户题应优先于内置题被抽到: 开场阶段第一个拿到的必须是租户题。
	q, ok := c.Bank().Next(orchestrator.StageGreeting, map[string]bool{})
	if !ok {
		t.Fatal("开场阶段应有可用题目")
	}
	if q.ID != "t_q_greet" {
		t.Fatalf("租户题应优先抽到, 实际抽到 %q", q.ID)
	}
	if q.Text != "欢迎参加云杉科技的面试, 请先做个自我介绍。" {
		t.Fatalf("租户题正文被改写: %q", q.Text)
	}
	stats := c.Stats()
	if !strings.Contains(stats.Source, "租户 1 题") {
		t.Fatalf("来源说明应反映租户题数量: %q", stats.Source)
	}
}

func TestSearchReturnsReferencePointsWithProvenance(t *testing.T) {
	c := buildCorpus(t, nil)
	hits, err := c.Search(context.Background(), "缓存和数据库不一致怎么处理, 延迟双删", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("检索应有结果")
	}
	// 结果必须带来源: 面试官要能追问"AI 凭什么问这个"。
	var found bool
	for _, h := range hits {
		if h.QuestionID == "" {
			t.Fatalf("检索结果缺少题目归属: %+v", h)
		}
		if h.Kind == "reference_point" && h.PointKey != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应至少命中一个带要点名的参考要点: %+v", hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("检索分数应为正: %+v", hits[0])
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	c := buildCorpus(t, nil)
	if _, err := c.Search(context.Background(), "   ", 5); err == nil {
		t.Fatal("空检索词应报错")
	}
}

// TestRAGProbePlannerPicksMissedReferencePoint 是"追问方向来自检索"的核心证据。
//
// 断言两件事:
//  1. 回答漏掉要点时, 追问靶子必须是参考答案里的某个要点(而不是题目自带关键词);
//  2. 要点被完整覆盖时, 不应再产生追问。
func TestRAGProbePlannerPicksMissedReferencePoint(t *testing.T) {
	c := buildCorpus(t, nil)
	planner := c.Planner()
	if planner.Name() != "rag" {
		t.Fatalf("应使用检索驱动的追问规划器, 实际 %q", planner.Name())
	}

	q, ok := c.Bank().ByID("q_cache_consistency")
	if !ok {
		t.Fatal("内置题库应包含 q_cache_consistency")
	}
	ref, ok := c.ReferenceBank().ByQuestionID(q.ID)
	if !ok || len(ref.Points) == 0 {
		t.Fatal("该题应有参考答案要点")
	}

	probe, ok := planner.PlanProbe(q, "加个缓存然后设置过期时间就行了", scoring.Result{})
	if !ok {
		t.Fatal("回答明显缺少要点时, 应当产生追问")
	}
	var matched bool
	for _, p := range ref.Points {
		if p.Key == probe.Focus {
			matched = true
			if probe.Reference != p.Text {
				t.Fatalf("追问应引用参考答案原文: %q vs %q", probe.Reference, p.Text)
			}
		}
	}
	if !matched {
		t.Fatalf("追问靶子必须来自参考答案要点, 实际 %q", probe.Focus)
	}

	// 把全部要点都答出来之后, 不应再无中生有地追问。
	var full strings.Builder
	for _, p := range ref.Points {
		full.WriteString(p.Text)
		full.WriteString("。")
	}
	if _, ok := planner.PlanProbe(q, full.String(), scoring.Result{}); ok {
		t.Fatal("要点已被完整覆盖时不应继续追问")
	}
}

func TestStructuralStagesAreAlwaysCovered(t *testing.T) {
	c := buildCorpus(t, nil)
	if missing := c.MissingStructuralStages(); len(missing) != 0 {
		t.Fatalf("内置题库应覆盖全部流程骨架阶段, 缺失: %v", missing)
	}
	stages := c.Stages()
	if len(stages) < 4 {
		t.Fatalf("覆盖阶段过少: %v", stages)
	}
	// 阶段顺序必须与流程一致, 否则界面上的进度条会跳。
	order := make(map[string]int)
	for i, s := range orchestrator.StageOrder {
		order[string(s)] = i
	}
	last := -1
	for _, s := range stages {
		if order[s] <= last {
			t.Fatalf("阶段顺序错乱: %v", stages)
		}
		last = order[s]
	}
}

func TestBuildToleratesItemsWithoutReferencePoints(t *testing.T) {
	// 只有题干、没有参考答案的题仍然可以问, 只是没有 RAG 语料贡献。
	items := []store.QuestionItem{{
		ID: "t_no_ref", TenantID: "acme", Stage: string(orchestrator.StageScenarioDesign),
		Text: "随便设计一个东西。", Status: "published",
	}}
	c := buildCorpus(t, items)
	if _, ok := c.Bank().ByID("t_no_ref"); !ok {
		t.Fatal("没有参考答案的题也应能进入题库")
	}
	// 追问规划器在缺少参考答案时必须安全地返回"不追问", 而不是 panic。
	q, _ := c.Bank().ByID("t_no_ref")
	if _, ok := c.Planner().PlanProbe(q, "不知道", scoring.Result{}); ok {
		t.Fatal("没有参考答案时不应产生检索驱动的追问")
	}
}

func TestSeedItemsArePublishedAndConsistent(t *testing.T) {
	items := SeedItems()
	if len(items) < 10 {
		t.Fatalf("内置题库过少: %d", len(items))
	}
	seen := make(map[string]bool)
	for _, it := range items {
		if seen[it.ID] {
			t.Fatalf("内置题库存在重复 ID: %s", it.ID)
		}
		seen[it.ID] = true
		if it.Status != "published" {
			t.Fatalf("内置题必须是已发布状态: %s -> %s", it.ID, it.Status)
		}
		if it.TenantID != "" {
			t.Fatalf("内置题不应归属任何租户: %s -> %q", it.ID, it.TenantID)
		}
		if strings.TrimSpace(it.Text) == "" || it.Stage == "" {
			t.Fatalf("内置题缺少题干或阶段: %+v", it)
		}
	}
}
