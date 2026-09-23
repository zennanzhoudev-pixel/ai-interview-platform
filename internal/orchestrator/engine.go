package orchestrator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/scoring"
)

var (
	// ErrFinished 表示会话已结束。
	ErrFinished = errors.New("orchestrator: session already finished")
	// ErrNoPendingQuestion 表示当前没有待回答的问题。
	ErrNoPendingQuestion = errors.New("orchestrator: no pending question")
	// ErrEmptyAnswer 表示提交了空回答。
	ErrEmptyAnswer = errors.New("orchestrator: empty answer")
)

// Action 是引擎在每一轮给出的下一步动作。
type Action string

const (
	// ActionAsk 换一道新题。
	ActionAsk Action = "ASK"
	// ActionProbe 在上一题上继续追问。
	ActionProbe Action = "PROBE"
	// ActionNextStage 进入下一个阶段, 并抛出该阶段首题。
	ActionNextStage Action = "NEXT_STAGE"
	// ActionFinish 面试结束, Decision.Report 携带完整报告。
	ActionFinish Action = "FINISH"
)

// Turn 是一次完整问答的记录, 对应库表 qa_turn。
type Turn struct {
	Index      int              `json:"index"`
	Stage      Stage            `json:"stage"`
	QuestionID string           `json:"question_id"`
	Competency string           `json:"competency,omitempty"`
	Question   string           `json:"question"`
	Answer     string           `json:"answer"`
	Duration   time.Duration    `json:"duration_ms"`
	IsProbe    bool             `json:"is_probe"`
	Scored     bool             `json:"scored"`
	Verdict    *scoring.Verdict `json:"verdict,omitempty"`
}

// Decision 是引擎的输出: 下一步该做什么, 以及本轮刚完成的问答记录。
type Decision struct {
	Action     Action
	Stage      Stage
	QuestionID string
	Question   string
	IsProbe    bool
	Turn       *Turn
	Report     *Report
}

// Engine 是面试编排引擎。
//
// 它是纯内存的确定性实现, 对外部世界零依赖: 真实部署时把内存结构换成
// Redis 快照 + Kafka 事件即可, 决策逻辑一行都不用改。
type Engine struct {
	plan   Plan
	bank   *Bank
	budget *Budget

	primary   scoring.Scorer
	secondary scoring.Scorer
	arbiter   scoring.Scorer
	tolerance int

	sessionID string

	stage        Stage
	cur          *Question
	probeDepth   int
	maxProbeUsed int
	stageElapsed time.Duration
	askedInStage int

	turns       []Turn
	used        map[string]bool
	coverage    map[string]bool
	probedFocus map[string]bool
	started     bool
	finished    bool
}

// Option 用于配置引擎。
type Option func(*Engine)

// WithScorers 配置双模型交叉评分。
// arbiter 可以为 nil, 此时双模型分歧超阈值会直接标记转人工复核。
func WithScorers(primary, secondary, arbiter scoring.Scorer, tolerance int) Option {
	return func(e *Engine) {
		if primary != nil {
			e.primary = primary
		}
		if secondary != nil {
			e.secondary = secondary
		}
		e.arbiter = arbiter
		if tolerance < 0 {
			tolerance = 0
		}
		e.tolerance = tolerance
	}
}

// NewEngine 构造编排引擎。
//
// 默认使用两个同源的规则评分器, 因此在没有任何模型配置的情况下也能完整跑完
// 一场面试 —— 这是离线自测基线的价值: CI 里不依赖网络也能回归评分逻辑。
func NewEngine(plan Plan, bank *Bank, total time.Duration, opts ...Option) *Engine {
	e := &Engine{
		plan:        plan,
		bank:        bank,
		budget:      NewBudget(total),
		primary:     scoring.NewKeywordScorer("rule-baseline-a", 0),
		secondary:   scoring.NewKeywordScorer("rule-baseline-b", 0),
		tolerance:   1,
		used:        make(map[string]bool),
		coverage:    make(map[string]bool),
		probedFocus: make(map[string]bool),
		sessionID:   fmt.Sprintf("s_%d", time.Now().Unix()),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// SessionID 返回本场面试的会话 ID。
func (e *Engine) SessionID() string { return e.sessionID }

// Stage 返回当前阶段。
func (e *Engine) Stage() Stage { return e.stage }

// Elapsed 返回已消耗时间。
func (e *Engine) Elapsed() time.Duration { return e.budget.Elapsed }

// Remaining 返回剩余时间。
func (e *Engine) Remaining() time.Duration { return e.budget.Remaining() }

// Finished 表示会话是否已结束。
func (e *Engine) Finished() bool { return e.finished }

// MaxProbeDepth 返回本场面试实际用到的最深追问层数, 用于校验深度上限。
func (e *Engine) MaxProbeDepth() int { return e.maxProbeUsed }

// Start 进入第一个阶段并抛出首题。
func (e *Engine) Start() Decision {
	if e.started {
		return Decision{Action: ActionFinish, Stage: e.stage}
	}
	e.started = true
	e.stage = e.plan.FirstStage()
	e.stageElapsed = 0
	e.askedInStage = 0

	if q, ok := e.bank.Next(e.stage, e.used); ok {
		return e.ask(q, ActionAsk)
	}
	return e.advance(nil)
}

// Submit 提交一次回答, 返回下一步动作。
//
// 决策顺序本身就是业务规则, 不能调换:
//  1. 该不该继续追问 —— 受最大深度、是否还有缺失要点、剩余时间三重约束;
//  2. 当前阶段该不该收口;
//  3. 本阶段是否还有未问的题;
//  4. 都否, 进入下一阶段。
func (e *Engine) Submit(answer string, took time.Duration) (Decision, error) {
	if e.finished {
		return Decision{}, ErrFinished
	}
	if e.cur == nil {
		return Decision{}, ErrNoPendingQuestion
	}

	answer = strings.TrimSpace(answer)
	if answer == "" {
		return Decision{}, ErrEmptyAnswer
	}
	if took < 0 {
		took = 0
	}

	q := *e.cur
	turn := Turn{
		Index:      len(e.turns) + 1,
		Stage:      e.stage,
		QuestionID: q.ID,
		Competency: q.Competency,
		Question:   q.Text,
		Answer:     answer,
		Duration:   took,
		IsProbe:    e.probeDepth > 0,
	}

	// 只有携带判定要点的考察项才评分。开场寒暄和候选人反问环节不计分,
	// 否则寒暄内容会被打分并污染整体结论。
	if len(q.Keywords) > 0 {
		verdict := scoring.CrossCheck(scoring.Answer{
			QuestionID:   q.ID,
			Competency:   q.Competency,
			Question:     q.Text,
			Text:         answer,
			Keywords:     q.Keywords,
			AntiPatterns: q.AntiPatterns,
		}, e.primary, e.secondary, e.arbiter, e.tolerance)

		// 证据绑定到具体 turn, 报告里才能一键跳回录音位置。
		bindTurnID(&verdict, fmt.Sprintf("t_%03d", turn.Index))
		turn.Scored = true
		turn.Verdict = &verdict
	}

	e.turns = append(e.turns, turn)
	e.budget.Consume(took)
	e.stageElapsed += took
	// 决策 0: 全场时间耗尽 -> 硬收口。
	// 不打断已经开始、已经在途的这一轮回答(它已经被完整记录),
	// 只是不再开新题。真实系统在这里还要写 session_timeout 埋点,
	// 并把未覆盖的能力项写进报告, 交给下一轮面试补。
	if e.budget.Remaining() <= 0 {
		return e.finish(&turn), nil
	}

	// 决策 1: 继续追问?
	if turn.Scored &&
		e.probeDepth < q.MaxProbe &&
		len(turn.Verdict.Final.Missing) > 0 &&
		e.budget.ShouldProbe(q.Importance) {

		e.probeDepth++
		if e.probeDepth > e.maxProbeUsed {
			e.maxProbeUsed = e.probeDepth
		}
		probe := probeFor(q, turn.Verdict.Final, e.probeDepth, e.probedFocus)
		e.cur = &probe
		return Decision{
			Action:     ActionProbe,
			Stage:      e.stage,
			QuestionID: probe.ID,
			Question:   probe.Text,
			IsProbe:    true,
			Turn:       &turn,
		}, nil
	}

	// 决策 2: 当前阶段收口?
	if spec, ok := e.plan.Spec(e.stage); ok && ShouldCloseStage(spec, e.askedInStage, e.stageElapsed) {
		return e.advance(&turn), nil
	}

	// 决策 3: 本阶段还有下一题?
	if next, ok := e.bank.Next(e.stage, e.used); ok {
		d := e.ask(next, ActionAsk)
		d.Turn = &turn
		return d, nil
	}

	// 决策 4: 换阶段。
	return e.advance(&turn), nil
}

func (e *Engine) ask(q Question, action Action) Decision {
	qq := q
	e.cur = &qq
	e.probeDepth = 0
	e.askedInStage++
	e.used[q.ID] = true
	if q.Competency != "" {
		e.coverage[q.Competency] = true
	}
	return Decision{
		Action:     action,
		Stage:      e.stage,
		QuestionID: q.ID,
		Question:   q.Text,
	}
}

// advance 沿 StageOrder 前进, 跳过没有配置或没有可用题目的阶段,
// 直到进入下一个可出题的阶段, 或者结束面试。
func (e *Engine) advance(turn *Turn) Decision {
	for {
		next := e.plan.Next(e.stage)
		e.stage = next
		e.stageElapsed = 0
		e.askedInStage = 0
		e.probeDepth = 0
		e.cur = nil

		if next == StageScoring || next == StageDone {
			return e.finish(turn)
		}
		if q, ok := e.bank.Next(next, e.used); ok {
			d := e.ask(q, ActionNextStage)
			d.Turn = turn
			return d
		}
	}
}

func (e *Engine) finish(turn *Turn) Decision {
	e.finished = true
	e.stage = StageDone
	e.cur = nil
	report := e.Report()
	return Decision{
		Action: ActionFinish,
		Stage:  StageDone,
		Turn:   turn,
		Report: &report,
	}
}

// probeFor 根据回答中缺失的判定要点生成追问。
//
// 真实实现里这一步由"追问 Agent"(大模型)承担: 它要理解候选人为什么漏掉
// 这个点, 并生成贴着上下文的问题。这里给出一个确定性的可测版本,
// 保证离线也能验证"追问到收敛"的完整链路。
func probeFor(parent Question, res scoring.Result, depth int, asked map[string]bool) Question {
	root := rootQuestionID(parent.ID)

	// 优先挑一个本次回答里"还没被追问过"的缺失要点,
	// 避免连续两轮问同一句话。
	focus := ""
	for _, m := range res.Missing {
		if !asked[root+"|"+m] {
			focus = m
			break
		}
	}
	if focus == "" && len(res.Missing) > 0 {
		focus = res.Missing[0]
	}

	text := "这套方案你在取舍上是怎么权衡的?"
	if focus != "" {
		key := root + "|" + focus
		repeat := asked[key]
		asked[key] = true

		text = fmt.Sprintf("你刚才没有提到 %s, 能展开讲讲吗?", focus)
		if repeat || depth >= 2 {
			// 同一个点被追问到第二层时换一种问法, 而不是把原话重复一遍。
			text = fmt.Sprintf("换个角度问: 如果现在重新设计, %s 这一块你会怎么处理?", focus)
		}
	}

	return Question{
		ID:           fmt.Sprintf("%s.p%d", parent.ID, depth),
		Stage:        parent.Stage,
		Competency:   parent.Competency,
		Text:         text,
		Keywords:     append([]string(nil), res.Missing...),
		AntiPatterns: parent.AntiPatterns,
		Importance:   parent.Importance,
		MaxProbe:     parent.MaxProbe,
	}
}

// rootQuestionID 从 "q_resume_zset.p2" 还原出 "q_resume_zset"。
func rootQuestionID(id string) string {
	if i := strings.Index(id, ".p"); i >= 0 {
		return id[:i]
	}
	return id
}

func bindTurnID(v *scoring.Verdict, turnID string) {
	bind := func(r *scoring.Result) {
		for i := range r.Evidence {
			r.Evidence[i].TurnID = turnID
		}
	}
	bind(&v.Primary)
	bind(&v.Secondary)
	bind(&v.Final)
	if v.Arbiter != nil {
		bind(v.Arbiter)
	}
}

// Dimension 是报告里的一个能力项结论。
type Dimension struct {
	Competency string             `json:"competency"`
	Level      string             `json:"level"`
	LevelNum   int                `json:"level_num"`
	Confidence float64            `json:"confidence"`
	Turns      int                `json:"turns"`
	Evidence   []scoring.Evidence `json:"evidence"`
	Concerns   []string           `json:"concerns,omitempty"`
}

// Stats 是质量观测指标, 直接对接 Prometheus 与一致性看板。
type Stats struct {
	Turns            int     `json:"turns"`
	ScoredTurns      int     `json:"scored_turns"`
	Probes           int     `json:"probes"`
	Disagreements    int     `json:"disagreements"`
	Arbitrations     int     `json:"arbitrations"`
	HumanReviewItems int     `json:"human_review_items"`
	MaxProbeDepth    int     `json:"max_probe_depth"`
	AvgConfidence    float64 `json:"avg_confidence"`
}

// Report 是面试评估报告, 对应库表 report。
//
// Recommendation 只是"建议": 真实系统里录用决策始终由人做,
// AI 的产出是可解释的证据与结构化结论。
type Report struct {
	SessionID      string      `json:"session_id"`
	Round          int         `json:"round"`
	DurationSec    int         `json:"duration_sec"`
	BudgetSec      int         `json:"budget_sec"`
	Recommendation string      `json:"recommendation"`
	Confidence     float64     `json:"confidence"`
	Dimensions     []Dimension `json:"dimensions"`
	Gaps           []string    `json:"gaps"`
	Flags          []string    `json:"flags"`
	Stats          Stats       `json:"stats"`
	Turns          []Turn      `json:"turns"`
}

// Report 汇总当前所有问答, 生成结构化评估报告。
func (e *Engine) Report() Report {
	rep := Report{
		SessionID:   e.sessionID,
		Round:       e.plan.Round,
		DurationSec: int(e.budget.Elapsed / time.Second),
		BudgetSec:   int(e.budget.Total / time.Second),
		Turns:       e.turns,
	}

	type agg struct {
		best     scoring.Level
		evidence []scoring.Evidence
		missing  map[string]bool
		matched  map[string]bool
		turns    int
		confSum  float64
	}
	byCompetency := make(map[string]*agg)
	var order []string

	for _, t := range e.turns {
		rep.Stats.Turns++
		if t.IsProbe {
			rep.Stats.Probes++
		}
		if !t.Scored || t.Verdict == nil {
			continue
		}
		rep.Stats.ScoredTurns++

		v := t.Verdict
		if v.Gap > 0 {
			rep.Stats.Disagreements++
		}
		if v.Arbitrated {
			rep.Stats.Arbitrations++
		}
		if v.NeedsHumanReview {
			rep.Stats.HumanReviewItems++
		}
		if t.Competency == "" {
			continue
		}

		a, ok := byCompetency[t.Competency]
		if !ok {
			a = &agg{missing: map[string]bool{}, matched: map[string]bool{}}
			byCompetency[t.Competency] = a
			order = append(order, t.Competency)
		}
		a.turns++
		// 一个能力项被问了多次(含追问)时, 取该能力项上的最佳表现作为结论,
		// 没答上来的要点则保留为"待确认项", 供复面或人工复核使用。
		if v.Final.Level.Number() > a.best.Number() {
			a.best = v.Final.Level
		}
		a.evidence = append(a.evidence, v.Final.Evidence...)
		for _, m := range v.Final.Missing {
			a.missing[m] = true
		}
		for _, m := range v.Final.Matched {
			a.matched[m] = true
		}
		a.confSum += v.Final.Confidence
	}

	sort.Strings(order)
	var levelSum, confSum float64
	for _, c := range order {
		a := byCompetency[c]
		conf := 0.0
		if a.turns > 0 {
			conf = a.confSum / float64(a.turns)
		}
		rep.Dimensions = append(rep.Dimensions, Dimension{
			Competency: c,
			Level:      a.best.String(),
			LevelNum:   a.best.Number(),
			Confidence: round2(conf),
			Turns:      a.turns,
			Evidence:   a.evidence,
			Concerns:   unresolved(a.missing, a.matched),
		})
		levelSum += float64(a.best.Number())
		confSum += conf
	}

	if len(rep.Dimensions) > 0 {
		rep.Recommendation = recommend(levelSum/float64(len(rep.Dimensions)), rep.Dimensions)
		rep.Confidence = round2(confSum / float64(len(rep.Dimensions)))
	} else {
		rep.Recommendation = "UNDETERMINED"
	}
	rep.Stats.MaxProbeDepth = e.maxProbeUsed
	rep.Stats.AvgConfidence = rep.Confidence

	// 覆盖度缺口: 计划里要考察, 但这场没问到的能力项。
	for _, c := range e.plan.Competencies() {
		if !e.coverage[c] {
			rep.Gaps = append(rep.Gaps, c)
		}
	}
	rep.Flags = buildFlags(rep)
	return rep
}

// recommend 把维度等级映射为招聘结论。
//
// 只要有一个能力项停在 L1, 直接 NO_HIRE —— 技术面试里"明显不会"
// 比"整体平庸"更危险。其余按均分分档。
func recommend(avg float64, dims []Dimension) string {
	for _, d := range dims {
		if d.LevelNum <= 1 {
			return "NO_HIRE"
		}
	}
	switch {
	case avg >= 4.0:
		return "STRONG_HIRE"
	case avg >= 3.0:
		return "HIRE"
	case avg >= 2.0:
		return "PASS_WITH_CONCERN"
	default:
		return "NO_HIRE"
	}
}

func buildFlags(rep Report) []string {
	var flags []string
	if rep.Stats.HumanReviewItems > 0 {
		flags = append(flags, fmt.Sprintf(
			"HUMAN_REVIEW_REQUIRED: %d 条评分双模型分歧过大且无可信仲裁结论",
			rep.Stats.HumanReviewItems))
	}
	if rep.Stats.Arbitrations > 0 {
		flags = append(flags, fmt.Sprintf(
			"ARBITRATED: %d 条评分触发了三方仲裁, 建议面试官优先复核",
			rep.Stats.Arbitrations))
	}
	if len(rep.Gaps) > 0 {
		flags = append(flags, fmt.Sprintf(
			"COVERAGE_INCOMPLETE: 能力项未覆盖 %v, 建议补充面试", rep.Gaps))
	}
	return flags
}

func dedupSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// unresolved 返回"被判为缺失、且整场面试都没被答上"的要点。
//
// 已经在后续追问里补上的要点不算缺口: 否则报告会一直挂着候选人
// 后来已经解释清楚的疑点, 面试官看完会觉得系统没在听他说话。
func unresolved(missing, matched map[string]bool) []string {
	var out []string
	for m := range missing {
		if !matched[m] {
			out = append(out, m)
		}
	}
	return dedupSorted(out)
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
