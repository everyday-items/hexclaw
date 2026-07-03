// pipeline.go 实现 v0.4.0 G2 Skill 7 阶段对齐（feature-flag gated）。
//
// 把现有零散能力（SelectByContext / MatchActivation / ContentLoader / Execute /
// improve.go 评分 & 组合学习）显式串成 7 阶段流水线，对齐 Hermes / Claude Skills
// 的 lifecycle 模型，让上层（agent / event 系统）可观察每一段。
//
// 7 阶段：
//  1. Discovery：按 query+activation 从 registry 召回候选
//  2. Activation：再次确认 when/not_when 满足（兜底，registry 已过滤）
//  3. Loading：通过 ContentLoader 加载完整 Prompt（内置 Skill 退化为 Description）
//  4. Verification：调用 ContentVerifier（如 MarkdownSkill.VerifyContent）做 TOCTOU 校验
//  5. Execution：Skill.Execute 真执行
//  6. Persistence：把 trace（input/output/duration/error）持久化
//  7. Improvement：基于结果触发评分 / 组合学习（外部 hook 注入）
//
// 不做向量召回：Discovery 仍走现有关键词打分（select.go）。向量推迟到 v0.4.x 后续。
//
// flag skill.pipeline.v1 控制是否启用本 Pipeline；OFF 时调用方仍可使用旧 API（
// SelectByContext / Execute），互不影响。
package skill

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagSkillPipelineV1 控制 G2 Skill Pipeline 是否启用。
const FlagSkillPipelineV1 = "skill.pipeline.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagSkillPipelineV1,
		Default:      true,
		Description:  "Run skill execution through the v0.4.0 7-phase pipeline (Discovery → Activation → Loading → Verification → Execution → Persistence → Improvement).",
		Stage:        featureflag.StageGA,
		SinceVersion: "0.4.0",
	})
}

// Phase 标识 7 阶段中的一个。
type Phase string

const (
	PhaseDiscovery    Phase = "discovery"
	PhaseActivation   Phase = "activation"
	PhaseLoading      Phase = "loading"
	PhaseVerification Phase = "verification"
	PhaseExecution    Phase = "execution"
	PhasePersistence  Phase = "persistence"
	PhaseImprovement  Phase = "improvement"
)

// AllPhases 是 7 阶段按规范顺序的列表（用于日志 / UI 展示）。
var AllPhases = []Phase{
	PhaseDiscovery, PhaseActivation, PhaseLoading,
	PhaseVerification, PhaseExecution, PhasePersistence, PhaseImprovement,
}

// ContentVerifier 可选接口：实现该接口的 Skill 会在 Verification 阶段被校验。
// 当前 marketplace.MarkdownSkill 实现 VerifyContent (TOCTOU 防御)；不实现的 Skill
// 该阶段直接通过。
type ContentVerifier interface {
	VerifyContent() error
}

// PhaseObserver 让外部代码观察 pipeline 进度（埋点 / events / metrics）。
//
// OnPhaseEnter 在进入某阶段前调用；OnPhaseExit 在退出（成功/失败）后调用，
// err 非 nil 表示该阶段失败 —— 流水线随后短路返回该错误。
type PhaseObserver interface {
	OnPhaseEnter(ctx context.Context, p Phase, skill Skill)
	OnPhaseExit(ctx context.Context, p Phase, skill Skill, err error, elapsed time.Duration)
}

// PersistenceHook 持久化阶段回调；nil 时 Phase 6 无副作用。
//
// 调用方可注入 SQLite / file 实现把 trace 写入。
type PersistenceHook func(ctx context.Context, trace ExecutionTrace) error

// ImprovementHook 改进阶段回调；nil 时 Phase 7 无副作用。
//
// 调用方可注入 LLM-judge 评分或组合学习触发。
type ImprovementHook func(ctx context.Context, trace ExecutionTrace) error

// ExecutionTrace 是一次 Skill 执行的快照，传给 Persistence/Improvement hook。
type ExecutionTrace struct {
	SkillName string
	Query     string
	Args      map[string]any
	Result    *Result // nil if execution failed
	Err       error
	StartedAt time.Time
	Duration  time.Duration
}

// PipelineOptions 控制单次 pipeline 执行的依赖注入与查询参数。
type PipelineOptions struct {
	Query      string          // 用于 Discovery（关键词召回）
	Activation Activation      // 用于 Activation 兜底
	TopK       int             // Discovery Top-K，<=0 时 1
	Args       map[string]any  // 传给 Skill.Execute
	Observer   PhaseObserver   // 可空
	OnPersist  PersistenceHook // 可空
	OnImprove  ImprovementHook // 可空
}

// PipelineResult 是 7 阶段执行的最终结果。
type PipelineResult struct {
	Skill      Skill
	Result     *Result
	Trace      ExecutionTrace
	PhasesDone []Phase
}

// RunPipeline 在给定 registry 上执行 7 阶段流水线，命中第一个候选 Skill。
//
// flag skill.pipeline.v1 关闭时直接返回 ErrPipelineDisabled —— 调用方应回退到老 API。
//
// 任一阶段返回 error 即短路：Persistence / Improvement 在 Execution 失败时也会被调用
// （为了 audit trail），其它阶段失败则跳过后续。
func RunPipeline(ctx context.Context, reg *DefaultRegistry, opts PipelineOptions) (*PipelineResult, error) {
	if !featureflag.Enabled(ctx, FlagSkillPipelineV1) {
		return nil, ErrPipelineDisabled
	}
	if reg == nil {
		return nil, fmt.Errorf("RunPipeline: nil registry")
	}

	res := &PipelineResult{}
	startedAt := time.Now()

	// Phase 1 — Discovery
	phaseStart := time.Now()
	notifyEnter(ctx, opts.Observer, PhaseDiscovery, nil)
	topK := opts.TopK
	if topK <= 0 {
		topK = 1
	}
	candidates := reg.SelectByContext(opts.Query, opts.Activation, topK)
	res.PhasesDone = append(res.PhasesDone, PhaseDiscovery)
	notifyExit(ctx, opts.Observer, PhaseDiscovery, nil, nil, time.Since(phaseStart))
	if len(candidates) == 0 {
		return nil, fmt.Errorf("pipeline: no candidate skill matched query=%q", opts.Query)
	}
	target := candidates[0]
	res.Skill = target

	// Phase 2 — Activation（兜底再确认）
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhaseActivation, target)
	if !MatchActivation(target, opts.Activation) {
		err := fmt.Errorf("pipeline: skill %q deactivated by when/not_when", target.Name())
		notifyExit(ctx, opts.Observer, PhaseActivation, target, err, time.Since(phaseStart))
		return nil, err
	}
	res.PhasesDone = append(res.PhasesDone, PhaseActivation)
	notifyExit(ctx, opts.Observer, PhaseActivation, target, nil, time.Since(phaseStart))

	// Phase 3 — Loading
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhaseLoading, target)
	if cl, ok := target.(ContentLoader); ok {
		if _, err := cl.LoadContent(); err != nil {
			notifyExit(ctx, opts.Observer, PhaseLoading, target, err, time.Since(phaseStart))
			return nil, fmt.Errorf("pipeline: load content for %q: %w", target.Name(), err)
		}
	}
	res.PhasesDone = append(res.PhasesDone, PhaseLoading)
	notifyExit(ctx, opts.Observer, PhaseLoading, target, nil, time.Since(phaseStart))

	// Phase 4 — Verification（TOCTOU 校验）
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhaseVerification, target)
	if v, ok := target.(ContentVerifier); ok {
		if err := v.VerifyContent(); err != nil {
			notifyExit(ctx, opts.Observer, PhaseVerification, target, err, time.Since(phaseStart))
			return nil, fmt.Errorf("pipeline: verify %q: %w", target.Name(), err)
		}
	}
	res.PhasesDone = append(res.PhasesDone, PhaseVerification)
	notifyExit(ctx, opts.Observer, PhaseVerification, target, nil, time.Since(phaseStart))

	// Phase 5 — Execution
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhaseExecution, target)
	execStart := time.Now()
	result, execErr := target.Execute(ctx, opts.Args)
	duration := time.Since(execStart)
	res.Result = result
	res.Trace = ExecutionTrace{
		SkillName: target.Name(),
		Query:     opts.Query,
		Args:      opts.Args,
		Result:    result,
		Err:       execErr,
		StartedAt: execStart,
		Duration:  duration,
	}
	notifyExit(ctx, opts.Observer, PhaseExecution, target, execErr, time.Since(phaseStart))
	res.PhasesDone = append(res.PhasesDone, PhaseExecution)

	// Phase 6 — Persistence（即使 Execution 失败也要 audit）
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhasePersistence, target)
	if opts.OnPersist != nil {
		if err := opts.OnPersist(ctx, res.Trace); err != nil {
			// persistence 失败不阻断流水线；记录后继续
			notifyExit(ctx, opts.Observer, PhasePersistence, target, err, time.Since(phaseStart))
		} else {
			notifyExit(ctx, opts.Observer, PhasePersistence, target, nil, time.Since(phaseStart))
		}
	} else {
		notifyExit(ctx, opts.Observer, PhasePersistence, target, nil, time.Since(phaseStart))
	}
	res.PhasesDone = append(res.PhasesDone, PhasePersistence)

	// Phase 7 — Improvement
	phaseStart = time.Now()
	notifyEnter(ctx, opts.Observer, PhaseImprovement, target)
	if opts.OnImprove != nil {
		if err := opts.OnImprove(ctx, res.Trace); err != nil {
			notifyExit(ctx, opts.Observer, PhaseImprovement, target, err, time.Since(phaseStart))
		} else {
			notifyExit(ctx, opts.Observer, PhaseImprovement, target, nil, time.Since(phaseStart))
		}
	} else {
		notifyExit(ctx, opts.Observer, PhaseImprovement, target, nil, time.Since(phaseStart))
	}
	res.PhasesDone = append(res.PhasesDone, PhaseImprovement)

	// 整体时长（不返回，仅给 logger 看）；execErr 决定最终错误
	_ = startedAt
	if execErr != nil {
		return res, fmt.Errorf("pipeline execution failed: %w", execErr)
	}
	return res, nil
}

// ErrPipelineDisabled 在 flag 关闭时返回，便于调用方做 fallback。
var ErrPipelineDisabled = fmt.Errorf("skill pipeline disabled (flag %s OFF)", FlagSkillPipelineV1)

func notifyEnter(ctx context.Context, ob PhaseObserver, p Phase, s Skill) {
	if ob == nil {
		return
	}
	defer func() { _ = recover() }()
	ob.OnPhaseEnter(ctx, p, s)
}

func notifyExit(ctx context.Context, ob PhaseObserver, p Phase, s Skill, err error, elapsed time.Duration) {
	if ob == nil {
		return
	}
	defer func() { _ = recover() }()
	ob.OnPhaseExit(ctx, p, s, err, elapsed)
}
