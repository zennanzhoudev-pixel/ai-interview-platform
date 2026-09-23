// Package orchestrator 是面试流程的编排引擎。
//
// 整体是"双层编排":
//
//	第一层: 阶段状态机(宏观, 确定性) —— 决定面试走到哪一步、每步花多少时间;
//	第二层: 问题 DAG(微观, 动态)   —— 决定问什么、追不追问。
//
// 分层的价值在于可控: 状态机是纯代码, 不可能被模型带偏; 模型只影响
// 阶段内部的局部决策, 出问题也炸不到整体结构。
package orchestrator

import "time"

// Stage 表示一场面试的宏观阶段。
type Stage string

const (
	StageInit            Stage = "INIT"
	StageGreeting        Stage = "GREETING"
	StageResumeDeepDive  Stage = "RESUME_DEEP_DIVE"
	StageTechFundamental Stage = "TECH_FUNDAMENTAL"
	StageScenarioDesign  Stage = "SCENARIO_DESIGN"
	StageCandidateQA     Stage = "CANDIDATE_QA"
	StageWrapUp          Stage = "WRAP_UP"
	StageScoring         Stage = "SCORING"
	StageDone            Stage = "DONE"
)

// StageOrder 固定阶段顺序。状态机只沿这个序列前进, 保证每场面试结构一致,
// 面试官之间才可能对齐(这也是"可横向对比"的前提)。
var StageOrder = []Stage{
	StageInit,
	StageGreeting,
	StageResumeDeepDive,
	StageTechFundamental,
	StageScenarioDesign,
	StageCandidateQA,
	StageWrapUp,
	StageScoring,
	StageDone,
}

// StageSpec 描述一个阶段的时长预算、最少问题数与需要覆盖的能力项。
type StageSpec struct {
	Stage        Stage
	Budget       time.Duration
	MinQuestions int
	Competencies []string
}

// Plan 是一轮面试的配置, 对应库表 interview_plan / interview_round。
// 1 面到 5 面共用同一套引擎, 差异全部体现在 Plan 上。
type Plan struct {
	Round int
	Specs []StageSpec
}

// DefaultPlan 返回按总时长等比例缩放的一份"一面(技术基础面)"配置。
//
// 各阶段比例参考 45 分钟线下技术面的常规节奏:
// 开场 2min / 简历深挖 15min / 技术基础 15min / 场景设计 8min /
// 候选人反问 3min / 收尾 1min。
func DefaultPlan(total time.Duration) Plan {
	share := func(ratio float64) time.Duration {
		return time.Duration(float64(total) * ratio)
	}
	return Plan{
		Round: 1,
		Specs: []StageSpec{
			{
				Stage:        StageGreeting,
				Budget:       share(0.04),
				MinQuestions: 1,
			},
			{
				Stage:        StageResumeDeepDive,
				Budget:       share(0.34),
				MinQuestions: 3,
				Competencies: []string{"project_depth", "tech_choice"},
			},
			{
				Stage:        StageTechFundamental,
				Budget:       share(0.33),
				MinQuestions: 3,
				Competencies: []string{"language_core", "distributed_system"},
			},
			{
				Stage:        StageScenarioDesign,
				Budget:       share(0.18),
				MinQuestions: 1,
				Competencies: []string{"architecture"},
			},
			{
				Stage:  StageCandidateQA,
				Budget: share(0.07),
			},
			{
				Stage:  StageWrapUp,
				Budget: share(0.02),
			},
		},
	}
}

// Spec 返回某个阶段的配置。
func (p Plan) Spec(s Stage) (StageSpec, bool) {
	for _, spec := range p.Specs {
		if spec.Stage == s {
			return spec, true
		}
	}
	return StageSpec{}, false
}

// Next 返回 StageOrder 中 s 的下一个阶段。
func (p Plan) Next(s Stage) Stage {
	for i, st := range StageOrder {
		if st == s && i+1 < len(StageOrder) {
			return StageOrder[i+1]
		}
	}
	return StageDone
}

// FirstStage 返回第一个有配置的阶段。
func (p Plan) FirstStage() Stage {
	if len(p.Specs) > 0 {
		return p.Specs[0].Stage
	}
	return StageDone
}

// Competencies 汇总本场面试计划覆盖的全部能力项, 用于计算覆盖度缺口。
func (p Plan) Competencies() []string {
	var out []string
	seen := make(map[string]bool)
	for _, spec := range p.Specs {
		for _, c := range spec.Competencies {
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
