package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
)

type scripted func(questionID string) (string, time.Duration)

// runSession 完整跑一场面试, 直到引擎给出 FINISH。
func runSession(t *testing.T, total time.Duration, s scripted) *Engine {
	t.Helper()

	engine := NewEngine(DefaultPlan(total), DefaultBank(), total)
	decision := engine.Start()
	for i := 0; i < 500; i++ {
		if decision.Action == ActionFinish {
			break
		}
		answer, took := s(decision.QuestionID)
		next, err := engine.Submit(answer, took)
		if err != nil {
			t.Fatalf("第 %d 轮提交失败: %v", i, err)
		}
		decision = next
	}

	if !engine.Finished() {
		t.Fatal("面试未在限定轮数内结束")
	}
	if decision.Report == nil {
		t.Fatal("面试结束时应当携带报告")
	}
	return engine
}

func TestSessionRunsToCompletion(t *testing.T) {
	engine := runSession(t, 45*time.Minute, func(string) (string, time.Duration) {
		return "这块用的是 ZSet 做索引, score 存权重, 排序和内存都做过评估, 指标降了 70%。", 2 * time.Minute
	})

	rep := engine.Report()
	if rep.Recommendation == "UNDETERMINED" {
		t.Fatal("面试结束后应当给出录用建议")
	}
	if rep.Stats.ScoredTurns == 0 {
		t.Fatal("应当存在已评分轮次")
	}
	if rep.Stats.Turns <= rep.Stats.ScoredTurns {
		t.Fatal("开场寒暄与候选人反问环节不应计分")
	}
	if len(rep.Turns) == 0 {
		t.Fatal("应当有问答记录")
	}
	if engine.Stage() != StageDone {
		t.Fatalf("结束时阶段应为 DONE, 实际 %s", engine.Stage())
	}
}

func TestProbeDepthIsCapped(t *testing.T) {
	engine := runSession(t, 45*time.Minute, func(string) (string, time.Duration) {
		return "这个我记不太清了, 当时是照着文档做的。", 30 * time.Second
	})

	if engine.MaxProbeDepth() > 2 {
		t.Fatalf("追问深度 %d 超过上限 2", engine.MaxProbeDepth())
	}
	if engine.Report().Stats.Probes == 0 {
		t.Fatal("答不到要点时应当产生追问")
	}
}

// 超时保护: 引擎可以因为"这一轮回答已经在途"而略微超出预算,
// 但不能无限超下去, 也不能因为超时丢失已完成轮次的记录。
func TestGlobalBudgetHardCutoff(t *testing.T) {
	const total = 30 * time.Minute
	const answerTime = 9 * time.Minute

	engine := runSession(t, total, func(string) (string, time.Duration) {
		return "细节记不清了。", answerTime
	})

	if engine.Elapsed() > total+answerTime {
		t.Fatalf("超时保护失效: 已用 %s, 预算 %s", engine.Elapsed(), total)
	}
	if len(engine.Report().Turns) == 0 {
		t.Fatal("超时收口也应保留已完成的轮次")
	}
}

// 长时间回答挤占预算时, 必须显式报告"哪些能力项没问到",
// 而不是给出一份看起来完整、实际上有空洞的报告。
func TestCoverageGapsAreReported(t *testing.T) {
	engine := runSession(t, 30*time.Minute, func(string) (string, time.Duration) {
		return "细节记不清了。", 9 * time.Minute
	})

	rep := engine.Report()
	if len(rep.Gaps) == 0 {
		t.Fatal("预算被挤占时应报告未覆盖的能力项")
	}

	var flagged bool
	for _, f := range rep.Flags {
		if strings.HasPrefix(f, "COVERAGE_INCOMPLETE") {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("覆盖度不足应体现在 flags 中, 实际 %v", rep.Flags)
	}
}

// 报告里的每条评分都必须能回溯到具体轮次和候选人原话。
func TestEveryScoreCarriesEvidenceBoundToTurn(t *testing.T) {
	engine := runSession(t, 45*time.Minute, func(string) (string, time.Duration) {
		return "用了 ZSet 和 score, 内存也考虑过。", 2 * time.Minute
	})

	scored := 0
	for _, turn := range engine.Report().Turns {
		if !turn.Scored || turn.Verdict == nil {
			continue
		}
		scored++
		if len(turn.Verdict.Final.Evidence) == 0 {
			t.Fatalf("第 %d 轮评分没有绑定证据", turn.Index)
		}
		for _, e := range turn.Verdict.Final.Evidence {
			if e.TurnID == "" {
				t.Fatalf("第 %d 轮证据未绑定 turn, 报告无法回溯录音", turn.Index)
			}
			if e.Quote == "" {
				t.Fatalf("第 %d 轮证据缺少候选人原话", turn.Index)
			}
		}
	}
	if scored == 0 {
		t.Fatal("应当存在已评分轮次")
	}
}

func TestShouldProbeRespectsRemainingTime(t *testing.T) {
	b := NewBudget(100 * time.Minute)
	b.Consume(85 * time.Minute)

	if b.Remaining() != 15*time.Minute {
		t.Fatalf("剩余时间计算错误: %s", b.Remaining())
	}
	if b.ShouldProbe(ImportanceMedium) {
		t.Fatal("剩余时间不足 20% 时不应继续追问次要考点")
	}
	if !b.ShouldProbe(ImportanceHigh) {
		t.Fatal("剩余时间紧张时, 高重要性考点仍应保留追问")
	}
}

func TestBudgetExhaustedStopsProbing(t *testing.T) {
	b := NewBudget(time.Minute)
	b.Consume(2 * time.Minute)

	if b.Remaining() != 0 {
		t.Fatalf("预算耗尽后剩余时间应为 0, 实际 %s", b.Remaining())
	}
	if b.ShouldProbe(ImportanceHigh) {
		t.Fatal("预算耗尽后不应再追问")
	}
}

func TestPlanScalesWithTotalDuration(t *testing.T) {
	short := DefaultPlan(20 * time.Minute)
	long := DefaultPlan(60 * time.Minute)

	sc, _ := short.Spec(StageResumeDeepDive)
	lc, _ := long.Spec(StageResumeDeepDive)
	if lc.Budget <= sc.Budget {
		t.Fatalf("阶段预算应随时长等比缩放: 20min=%s, 60min=%s", sc.Budget, lc.Budget)
	}
	if len(long.Competencies()) != len(short.Competencies()) {
		t.Fatal("能力项集合不应随时长变化")
	}
}

func TestSubmitOnFinishedSessionIsRejected(t *testing.T) {
	engine := runSession(t, 20*time.Minute, func(string) (string, time.Duration) {
		return "不知道。", 30 * time.Second
	})
	if _, err := engine.Submit("再说一句", time.Second); err != ErrFinished {
		t.Fatalf("已结束的会话应拒绝提交, 实际错误 %v", err)
	}
}

// 断线重连必须能还原出与中断时完全一致的状态, 包括待回答的问题。
func TestRestoreReproducesSessionState(t *testing.T) {
	plan := DefaultPlan(45 * time.Minute)
	bank := DefaultBank()

	original := NewEngine(plan, bank, 45*time.Minute)
	d := original.Start()
	for i := 0; i < 3; i++ {
		next, err := original.Submit("用了 ZSet 和 score, 内存也考虑过。", 2*time.Minute)
		if err != nil {
			t.Fatalf("原始会话第 %d 轮失败: %v", i+1, err)
		}
		d = next
	}
	// 断线时引擎正在等待回答的那个问题
	pendingQuestionID := d.QuestionID
	pendingStage := d.Stage
	if pendingQuestionID == "" {
		t.Fatal("测试需要断线时仍有待回答的问题")
	}

	recorded := original.Report().Turns

	restored := NewEngine(plan, bank, 45*time.Minute)
	if err := restored.Restore(recorded); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	if restored.Stage() != pendingStage {
		t.Fatalf("恢复后的阶段应为 %s, 实际 %s", pendingStage, restored.Stage())
	}
	id, _, ok := restored.Pending()
	if !ok {
		t.Fatal("恢复后应当有一个待回答的问题")
	}
	if id != pendingQuestionID {
		t.Fatalf("恢复后应继续问 %s, 实际 %s", pendingQuestionID, id)
	}

	gotTurns := restored.Report().Turns
	if len(gotTurns) != len(recorded) {
		t.Fatalf("恢复后应有 %d 轮记录, 实际 %d", len(recorded), len(gotTurns))
	}
	for i := range recorded {
		if gotTurns[i].QuestionID != recorded[i].QuestionID {
			t.Fatalf("第 %d 轮题目不一致: %s vs %s", i+1, gotTurns[i].QuestionID, recorded[i].QuestionID)
		}
		if gotTurns[i].Scored != recorded[i].Scored {
			t.Fatalf("第 %d 轮是否计分不一致", i+1)
		}
	}
}

// 恢复时复用已记录的评分, 不重新调用评分器。
// 这条约束直接关系到成本: 每次重连都重打一遍分, 会把模型额度烧穿。
func TestRestoreReusesRecordedVerdictWithoutRescoring(t *testing.T) {
	plan := DefaultPlan(30 * time.Minute)
	bank := DefaultBank()

	counting := &countingScorer{inner: scoring.NewKeywordScorer("counting", 0)}
	original := NewEngine(plan, bank, 30*time.Minute,
		WithScorers(counting, scoring.NewKeywordScorer("b", 0), nil, 1))
	_ = original.Start()
	for i := 0; i < 3; i++ {
		if _, err := original.Submit("用了 ZSet, score 是权重。", time.Minute); err != nil {
			t.Fatalf("原始会话第 %d 轮失败: %v", i+1, err)
		}
	}
	callsAfterOriginal := counting.count
	if callsAfterOriginal == 0 {
		t.Fatal("原始会话应当调用过评分器")
	}

	restored := NewEngine(plan, bank, 30*time.Minute,
		WithScorers(counting, scoring.NewKeywordScorer("b", 0), nil, 1))
	if err := restored.Restore(original.Report().Turns); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if counting.count != callsAfterOriginal {
		t.Fatalf("恢复不应重新评分: 原 %d 次, 恢复后 %d 次", callsAfterOriginal, counting.count)
	}
}

// 题库或面试计划变更后, 旧记录重放出来的问题会对不上, 必须直接报错。
func TestRestoreFailsWhenQuestionBankChanged(t *testing.T) {
	plan := DefaultPlan(45 * time.Minute)
	original := NewEngine(plan, DefaultBank(), 45*time.Minute)
	_ = original.Start()
	for i := 0; i < 3; i++ {
		if _, err := original.Submit("回答", time.Minute); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i+1, err)
		}
	}

	// 换成一个完全不同的题库
	changed := NewBank(Question{ID: "q_other", Stage: StageGreeting, Text: "另一个问题"})
	restored := NewEngine(plan, changed, 45*time.Minute)
	if err := restored.Restore(original.Report().Turns); err == nil {
		t.Fatal("题库变更后恢复必须报错, 而不是悄悄换一套题目继续面试")
	}
}

func TestRestoreAfterStartIsRejected(t *testing.T) {
	engine := NewEngine(DefaultPlan(time.Minute), DefaultBank(), time.Minute)
	_ = engine.Start()
	if err := engine.Restore(nil); err == nil {
		t.Fatal("已启动的引擎不应允许恢复")
	}
}

// countingScorer 记录评分被调用的次数。
type countingScorer struct {
	inner scoring.Scorer
	count int
}

func (c *countingScorer) Name() string { return c.inner.Name() }

func (c *countingScorer) Score(a scoring.Answer) scoring.Result {
	c.count++
	return c.inner.Score(a)
}
