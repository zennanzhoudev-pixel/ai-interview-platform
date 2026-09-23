package scoring

import "testing"

// stubScorer 是一个可控的评分器, 用来构造"双模型分歧"等边界场景。
type stubScorer struct {
	name      string
	level     Level
	hasProof  bool
}

func (s stubScorer) Name() string { return s.name }

func (s stubScorer) Score(a Answer) Result {
	r := Result{Model: s.name, Level: s.level, Confidence: 0.8, Rationale: "stub"}
	if s.hasProof {
		r.Evidence = []Evidence{{
			QuestionID: a.QuestionID,
			Kind:       EvidenceSupport,
			Quote:      "stub evidence",
		}}
	}
	return r.Enforce()
}

func TestKeywordScorerMapsRatioToLevel(t *testing.T) {
	s := NewKeywordScorer("rule", 0)
	got := s.Score(Answer{
		QuestionID: "q1",
		Text:       "我们用了 ZSet 做索引, score 存权重, 排序走有序结构, 内存也做了分片。",
		Keywords:   []string{"ZSet", "score", "排序", "内存"},
	})
	if got.Level != LevelExpert {
		t.Fatalf("四个要点全中应为 L5, 实际 %s", got.Level)
	}
	if len(got.Evidence) != 4 {
		t.Fatalf("每条要点都应绑定证据, 实际 %d 条", len(got.Evidence))
	}
	if len(got.Missing) != 0 {
		t.Fatalf("不应有缺失要点, 实际 %v", got.Missing)
	}
}

func TestKeywordScorerReportsMissingForProbing(t *testing.T) {
	s := NewKeywordScorer("rule", 0)
	got := s.Score(Answer{
		QuestionID: "q1",
		Text:       "用了 ZSet, score 是权重。",
		Keywords:   []string{"ZSet", "score", "排序", "内存"},
	})
	if got.Level != LevelProficient {
		t.Fatalf("命中 2/4 应为 L3, 实际 %s", got.Level)
	}
	if len(got.Missing) != 2 {
		t.Fatalf("应剩下 2 个缺失要点作为追问方向, 实际 %v", got.Missing)
	}
}

func TestKeywordScorerAntiPatternForcesDowngrade(t *testing.T) {
	s := NewKeywordScorer("rule", 0)
	got := s.Score(Answer{
		QuestionID:   "q1",
		Text:         "这个不用管重复消费, 直接写库就行, 幂等的问题业务侧自己处理。",
		Keywords:     []string{"幂等"},
		AntiPatterns: []string{"不用管重复消费"},
	})
	if got.Level != LevelNone {
		t.Fatalf("命中断言反例应直接降到 L1, 实际 %s", got.Level)
	}
	if len(got.AntiHits) != 1 {
		t.Fatalf("应记录 1 条反例, 实际 %v", got.AntiHits)
	}
}

// 这是"抗幻觉"的核心测试: 拿不出证据的分数必须被作废。
func TestResultWithoutEvidenceIsDiscarded(t *testing.T) {
	r := Result{Model: "liar", Level: LevelExpert, Confidence: 0.99}.Enforce()
	if r.Level != LevelUnknown {
		t.Fatalf("无证据的评分应作废, 实际 %s", r.Level)
	}
	if r.Valid() {
		t.Fatal("无证据的结果不应被视为有效")
	}
	if r.Confidence != 0 {
		t.Fatalf("作废后置信度应为 0, 实际 %v", r.Confidence)
	}
}

// 分歧在容忍范围内: 不仲裁, 取保守值。
func TestCrossCheckWithinToleranceTakesConservativeValue(t *testing.T) {
	a := Answer{QuestionID: "q1", Text: "x"}
	v := CrossCheck(a,
		stubScorer{name: "a", level: LevelExpert, hasProof: true},
		stubScorer{name: "b", level: LevelAdvanced, hasProof: true},
		stubScorer{name: "c", level: LevelProficient, hasProof: true},
		1)
	if v.Gap != 1 {
		t.Fatalf("分歧应为 1 级, 实际 %d", v.Gap)
	}
	if v.Arbitrated {
		t.Fatal("容忍范围内不应触发仲裁")
	}
	if v.Final.Level != LevelAdvanced {
		t.Fatalf("应取较低的 L4, 实际 %s", v.Final.Level)
	}
}

// 分歧超阈值且有仲裁模型: 三方取中位数。
func TestCrossCheckBeyondToleranceArbitrates(t *testing.T) {
	a := Answer{QuestionID: "q1", Text: "x"}
	v := CrossCheck(a,
		stubScorer{name: "a", level: LevelExpert, hasProof: true},
		stubScorer{name: "b", level: LevelAware, hasProof: true},
		stubScorer{name: "c", level: LevelProficient, hasProof: true},
		1)
	if v.Gap != 3 {
		t.Fatalf("分歧应为 3 级, 实际 %d", v.Gap)
	}
	if !v.Arbitrated {
		t.Fatal("分歧超阈值应触发仲裁")
	}
	if v.Arbiter == nil {
		t.Fatal("仲裁结果不应为空")
	}
	if v.Final.Level != LevelProficient {
		t.Fatalf("三方中位数应为 L3, 实际 %s", v.Final.Level)
	}
}

// 分歧超阈值且没有仲裁模型: 必须转人工复核, 而不是随便选一个分数。
func TestCrossCheckWithoutArbiterGoesToHumanReview(t *testing.T) {
	a := Answer{QuestionID: "q1", Text: "x"}
	v := CrossCheck(a,
		stubScorer{name: "a", level: LevelExpert, hasProof: true},
		stubScorer{name: "b", level: LevelAware, hasProof: true},
		nil, 1)
	if !v.NeedsHumanReview {
		t.Fatal("无仲裁模型的分歧必须转人工复核")
	}
	if v.Final.Level != LevelAware {
		t.Fatalf("转人工时仍应保守取值, 实际 %s", v.Final.Level)
	}
}

func TestCrossCheckDiscardsEvidenceLessResults(t *testing.T) {
	a := Answer{QuestionID: "q1", Text: "x"}
	v := CrossCheck(a,
		stubScorer{name: "a", level: LevelExpert, hasProof: false},
		stubScorer{name: "b", level: LevelExpert, hasProof: false},
		nil, 1)
	if v.Final.Level != LevelUnknown {
		t.Fatalf("双方都拿不出证据时应作废, 实际 %s", v.Final.Level)
	}
}

func TestLevelBiasClampsAtBoundaries(t *testing.T) {
	cases := []struct {
		in   Level
		bias int
		want Level
	}{
		{LevelProficient, -1, LevelAware},
		{LevelExpert, 3, LevelExpert},
		{LevelNone, -5, LevelNone},
		{LevelUnknown, 2, LevelUnknown},
	}
	for _, c := range cases {
		if got := c.in.Bias(c.bias); got != c.want {
			t.Errorf("%s bias %d = %s, 期望 %s", c.in, c.bias, got, c.want)
		}
	}
}
