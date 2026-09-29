package orchestrator

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/rag"
	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/resume"
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
	// 追问的决策依据。只在 IsProbe 时非空。
	//
	// 为什么要把它们记在 Turn 上而不是只留在内存里: 追问方向是 AI 自己挑的,
	// 如果不落库, 事后只能看到"它问了这句", 看不到"它凭什么问这句"。
	// 复核 AI 判断时, 这个差别就是"可解释"与"只能相信"的差别。
	ProbeFocus     string         `json:"probe_focus,omitempty"`
	ProbeReference string         `json:"probe_reference,omitempty"`
	Retrieval      []RetrievalHit `json:"retrieval,omitempty"`
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

	// planner 决定追问方向。nil 时回落到"关键词缺失"的默认策略,
	// 保证没有 RAG 依赖时行为与之前完全一致。
	planner ProbePlanner
	// resume 是候选人结构化简历, 用于在追问里引用简历原话(原文定位)。
	resume *resume.Resume

	// pendingTrace 保存"当前这道追问是怎么被挑出来的"。
	// 它在生成追问时写入, 在该轮被作答、落库时取出 —— 这样检索快照只记一次,
	// 而且与它对应的那一轮严格对齐。
	pendingTrace []RetrievalHit
	pendingProbe *Probe
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

// WithProbePlanner 用检索驱动的追问规划器替换默认的"关键词缺失"策略。
func WithProbePlanner(p ProbePlanner) Option {
	return func(e *Engine) {
		if p != nil {
			e.planner = p
		}
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

// SetResume 设置候选人结构化简历, 让追问能引用简历原话。
func (e *Engine) SetResume(r *resume.Resume) { e.resume = r }

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
	return e.submit(answer, took, nil)
}

// Pending 返回当前等待回答的问题。
// 断线重连后用它把问题重新推给候选人, 而不是从头再问一遍。
func (e *Engine) Pending() (id, text string, ok bool) {
	if e.cur == nil {
		return "", "", false
	}
	return e.cur.ID, e.cur.Text, true
}

// Restore 用已记录的问答重放引擎状态, 用于断线重连。
//
// 为什么是"重放"而不是"存快照再反序列化": 引擎的状态(阶段进度、
// 预算消耗、追问深度、已覆盖能力项)是全部历史的函数。与其为每个内部
// 字段维护序列化, 不如重放历史 —— 代码更少, 而且天然不会出现
// "快照漏了一个字段导致恢复后行为不一致"这种最难查的问题。
//
// 重放时会逐轮校验题目 ID: 如果题库或面试计划变过, 恢复出来的状态
// 与当初就不是同一场面试了, 这时必须报错而不是硬撑。
// 已经记录过评分的轮次直接复用原评分, 不重新调用模型 —— 否则每次
// 重连都会把整场面试的模型额度再烧一遍。
func (e *Engine) Restore(turns []Turn) error {
	if e.started {
		return errors.New("orchestrator: 引擎已启动, 不能恢复历史状态")
	}

	d := e.Start()
	for i, want := range turns {
		if d.Action == ActionFinish {
			return fmt.Errorf("orchestrator: 重放第 %d 轮时面试已经结束", i+1)
		}
		if d.QuestionID != want.QuestionID {
			return fmt.Errorf(
				"orchestrator: 第 %d 轮题目不一致(记录 %s, 重放 %s), 题库或面试计划已变更, 无法安全恢复",
				i+1, want.QuestionID, d.QuestionID)
		}
		next, err := e.submit(want.Answer, want.Duration, want.Verdict)
		if err != nil {
			return fmt.Errorf("orchestrator: 重放第 %d 轮失败: %w", i+1, err)
		}
		d = next
	}
	return nil
}

// submit 是 Submit 的内部实现。
// preset 非 nil 表示直接采用已有评分, 不再调用评分器。
func (e *Engine) submit(answer string, took time.Duration, preset *scoring.Verdict) (Decision, error) {
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
	// 把"这道追问当时是怎么选出来的"写进本轮记录。
	if turn.IsProbe && e.pendingProbe != nil {
		turn.ProbeFocus = e.pendingProbe.Focus
		turn.ProbeReference = e.pendingProbe.Reference
		turn.Retrieval = e.pendingTrace
	}
	// 用过即清: 下一轮如果不是追问, 就不该继承上一次的检索快照。
	e.pendingTrace = nil
	e.pendingProbe = nil

	// 只有携带判定要点的考察项才评分。开场寒暄和候选人反问环节不计分,
	// 否则寒暄内容会被打分并污染整体结论。
	switch {
	case len(q.Keywords) == 0:
		// 非考察项: 不评分
	case preset != nil:
		verdict := *preset
		bindTurnID(&verdict, fmt.Sprintf("t_%03d", turn.Index))
		turn.Scored = true
		turn.Verdict = &verdict
	default:
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
	if turn.Scored && e.probeDepth < q.MaxProbe && e.budget.ShouldProbe(q.Importance) {
		if probe, ok := e.decideProbe(q, answer, turn.Verdict.Final, e.probeDepth); ok {
			e.probeDepth++
			if e.probeDepth > e.maxProbeUsed {
				e.maxProbeUsed = e.probeDepth
			}
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

// decideProbe 决定追问方向: 有检索规划器时用它, 否则走默认的关键词缺失策略。
func (e *Engine) decideProbe(parent Question, answer string, res scoring.Result, depth int) (Question, bool) {
	if e.planner != nil {
		p, ok := e.planner.PlanProbe(parent, answer, res)
		if !ok {
			return Question{}, false
		}
		// 记下这次追问的依据与检索快照, 等这一轮被作答时写进 Turn。
		e.pendingProbe = &p
		e.pendingTrace = p.Hits
		return e.ragProbeQuestion(parent, res, depth, p), true
	}
	if len(res.Missing) == 0 {
		return Question{}, false
	}
	return probeFor(parent, res, depth, e.probedFocus), true
}

// ragProbeQuestion 生成一条以参考答案为依据的追问。
// 若候选人简历里恰好写到这个要点, 就优先引用简历原话 —— 这正是"原文定位"。
func (e *Engine) ragProbeQuestion(parent Question, res scoring.Result, depth int, p Probe) Question {
	root := rootQuestionID(parent.ID)
	key := root + "|" + p.Focus
	repeat := e.probedFocus[key]
	e.probedFocus[key] = true

	text := fmt.Sprintf("参考答案里强调了「%s」, 你的回答还没覆盖, 能展开讲讲吗?", p.Reference)
	if quote := e.resumeQuoteFor(p.Reference); quote != "" {
		text = fmt.Sprintf("你在简历里写到「%s」, 展开讲讲。", quote)
	}
	if repeat || depth >= 2 {
		text = fmt.Sprintf("换个角度: 如果现在重新设计, 「%s」这一块你会怎么处理?", p.Reference)
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

// resumeQuoteFor 在简历里找与参考答案要点最相关的原话片段。
func (e *Engine) resumeQuoteFor(refText string) string {
	if e.resume == nil {
		return ""
	}
	refTokens := rag.Tokenize(refText)
	best, bestScore := "", 0
	for _, b := range e.resume.Blocks {
		blockTokens := rag.Tokenize(b.Text)
		score := 0
		for _, a := range refTokens {
			for _, c := range blockTokens {
				if a == c {
					score++
				}
			}
		}
		if score > bestScore {
			best, bestScore = b.Text, score
		}
	}
	if bestScore < 2 {
		return ""
	}
	rs := []rune(best)
	if len(rs) > 48 {
		best = string(rs[:48]) + "…"
	}
	return best
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
	Competency string `json:"competency"`
	// Label 是能力项的中文展示名。库里存英文 key, 展示层用中文标签。
	Label    string `json:"label"`
	Level    string `json:"level"`
	LevelNum int    `json:"level_num"`
	// Score 是等级换算出的 0..100 分, 便于跨场次横向对比。
	Score      int                `json:"score"`
	Confidence float64            `json:"confidence"`
	Turns      int                `json:"turns"`
	Evidence   []scoring.Evidence `json:"evidence"`
	Concerns   []string           `json:"concerns,omitempty"`
}

// Stats 是质量观测指标, 直接对接 Prometheus 与一致性看板。
type Stats struct {
	Turns            int `json:"turns"`
	ScoredTurns      int `json:"scored_turns"`
	Probes           int `json:"probes"`
	Disagreements    int `json:"disagreements"`
	Arbitrations     int `json:"arbitrations"`
	HumanReviewItems int `json:"human_review_items"`
	// Degraded 统计有多少条评分来自备用评分器(通常是规则评分器)。
	// 这个数字出现在每份报告里, 是"评分标准有没有悄悄变化"的报警器。
	Degraded      int     `json:"degraded_scores"`
	MaxProbeDepth int     `json:"max_probe_depth"`
	AvgConfidence float64 `json:"avg_confidence"`
}

// Report 是面试评估报告, 对应库表 report。
//
// Recommendation 只是"建议": 真实系统里录用决策始终由人做,
// AI 的产出是可解释的证据与结构化结论。
type Report struct {
	SessionID      string `json:"session_id"`
	Round          int    `json:"round"`
	DurationSec    int    `json:"duration_sec"`
	BudgetSec      int    `json:"budget_sec"`
	Recommendation string `json:"recommendation"`
	// Score 是 0..100 综合分。等级之外再给分数的理由很实际:
	// 用人部门习惯按分数横向排序, 只给 L3/L4 他们反而会自己换算一次,
	// 而且换算规则各人不同 —— 不如把规则显式写在代码里。
	Score      int         `json:"score"`
	Confidence float64     `json:"confidence"`
	Dimensions []Dimension `json:"dimensions"`
	Gaps       []string    `json:"gaps"`
	Flags      []string    `json:"flags"`
	Stats      Stats       `json:"stats"`
	Turns      []Turn      `json:"turns"`
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
		if v.Final.DegradedFrom != "" {
			rep.Stats.Degraded++
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
			Label:      CompetencyLabel(c),
			Level:      a.best.String(),
			LevelNum:   a.best.Number(),
			Score:      a.best.Score(),
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
		rep.Score = compositeScore(rep.Dimensions)
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
	if rep.Stats.Degraded > 0 {
		flags = append(flags, fmt.Sprintf(
			"DEGRADED_SCORING: %d 条评分由备用评分器(规则匹配)产出, 结论可信度低于大模型评分",
			rep.Stats.Degraded))
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

// compositeScore 计算 0..100 综合分。
//
// 用等权平均而不是加权: 当前题库并没有为能力项定义权重, 凭空给
// "分布式与中间件"设一个 30% 只会让分数看起来更精确, 并不更准确。
// 等权平均至少是可解释的 —— 面试官能一眼算出这个分数是怎么来的。
//
// 它也不豁免任何维度: 某一项只要停在 L1, 那 40 分就会把综合分拉下来,
// 这与 recommend 里"一项不合格即 NO_HIRE"的取向保持一致。
func compositeScore(dims []Dimension) int {
	if len(dims) == 0 {
		return 0
	}
	sum := 0
	for _, d := range dims {
		sum += d.Score
	}
	return int(math.Round(float64(sum) / float64(len(dims))))
}
