// canary.go 实现 v0.4.0 Gate #47 Canary 发布流程。
//
// Canary（金丝雀发布）按百分比逐步放量：5% → 25% → 50% → 100%。每阶段间检查
// HealthGate（错误率 / latency / 用户反馈），健康才推进，否则回滚到上一阶段。
//
// Rollout 是单次发布的状态机：
//
//	Pending → Stage(5%) → Stage(25%) → Stage(50%) → Stage(100%) → Completed
//	            ↓ 任一阶段 health 不通过
//	          RolledBack
//
// 本包只提供框架 + 内存版状态机；与具体路由层（feature flag / load balancer）的
// 集成由调用方按 Rollout.CurrentPercent() 自行决定哪些用户走新版本。
package release

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CanaryStage 是 canary 发布的单个阶段。Percent 是该阶段允许放量的总比例（0-100）。
type CanaryStage struct {
	Name    string
	Percent int
	// MinDuration 是进入该阶段后最少停留时间（监控冷却期）。早于该时长的 Advance 调用
	// 会被拒绝，避免运维一冲到底。0 表示不强制等待。
	MinDuration time.Duration
}

// DefaultStages 返回经典 5% → 25% → 50% → 100% 四阶段定义。
func DefaultStages() []CanaryStage {
	return []CanaryStage{
		{Name: "canary-5", Percent: 5, MinDuration: 10 * time.Minute},
		{Name: "canary-25", Percent: 25, MinDuration: 30 * time.Minute},
		{Name: "canary-50", Percent: 50, MinDuration: 30 * time.Minute},
		{Name: "ga-100", Percent: 100, MinDuration: 0},
	}
}

// HealthGate 在 Advance 前被调用 —— 决定当前阶段是否健康可推进。
//
// 实现可以读 Prometheus / events / 用户反馈系统：返回 nil 表示健康，error 表示
// 不健康；调用方据此决定继续等待 / 主动 Rollback。
type HealthGate interface {
	Check(ctx context.Context, currentStage CanaryStage) error
}

// HealthFunc 把普通函数适配为 HealthGate。
type HealthFunc func(ctx context.Context, stage CanaryStage) error

// Check 实现 HealthGate。
func (f HealthFunc) Check(ctx context.Context, stage CanaryStage) error {
	if f == nil {
		return nil
	}
	return f(ctx, stage)
}

// RolloutState 是 rollout 的当前状态。
type RolloutState string

const (
	RolloutPending    RolloutState = "pending"
	RolloutInProgress RolloutState = "in_progress"
	RolloutCompleted  RolloutState = "completed"
	RolloutRolledBack RolloutState = "rolled_back"
	RolloutFailed     RolloutState = "failed"
)

// Rollout 是一次发布的完整状态机。线程安全。
type Rollout struct {
	mu         sync.Mutex
	now        func() time.Time
	stages     []CanaryStage
	currentIdx int // -1 表示 Pending；>=0 表示当前已进入的 stage 索引
	state      RolloutState
	startedAt  map[int]time.Time // 每个 stage 进入时刻
	gate       HealthGate
	err        error
}

// NewRollout 构造一个 rollout。stages 至少 1 个；HealthGate 可空（永远通过）。
func NewRollout(stages []CanaryStage, gate HealthGate) (*Rollout, error) {
	if len(stages) == 0 {
		return nil, errors.New("rollout: at least one stage required")
	}
	for i, s := range stages {
		if s.Percent < 0 || s.Percent > 100 {
			return nil, fmt.Errorf("rollout: stage[%d] %q percent %d out of range", i, s.Name, s.Percent)
		}
		if i > 0 && s.Percent <= stages[i-1].Percent {
			return nil, fmt.Errorf("rollout: stage[%d] percent %d must exceed previous %d",
				i, s.Percent, stages[i-1].Percent)
		}
	}
	return &Rollout{
		now:        time.Now,
		stages:     append([]CanaryStage{}, stages...),
		currentIdx: -1,
		state:      RolloutPending,
		startedAt:  map[int]time.Time{},
		gate:       gate,
	}, nil
}

// State 返回当前状态。
func (r *Rollout) State() RolloutState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// CurrentPercent 返回当前激活的放量百分比。Pending = 0，Completed = 100。
func (r *Rollout) CurrentPercent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentIdx < 0 {
		return 0
	}
	return r.stages[r.currentIdx].Percent
}

// CurrentStage 返回当前阶段（Pending 时返回 zero value）。
func (r *Rollout) CurrentStage() CanaryStage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentIdx < 0 {
		return CanaryStage{}
	}
	return r.stages[r.currentIdx]
}

// Advance 推进到下一阶段。若已在最后阶段，状态变 Completed。
//
// 推进规则：
//  1. 当前阶段必须满足 MinDuration（否则返回错误）
//  2. HealthGate.Check 必须返回 nil
//  3. Pending 状态下首次 Advance 会进入第一个 stage
func (r *Rollout) Advance(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RolloutCompleted {
		return errors.New("rollout: already completed")
	}
	if r.state == RolloutRolledBack || r.state == RolloutFailed {
		return fmt.Errorf("rollout: terminal state %s", r.state)
	}

	// 已在某个 stage —— 校验 MinDuration 与 health
	if r.currentIdx >= 0 {
		cur := r.stages[r.currentIdx]
		if dwell := r.now().Sub(r.startedAt[r.currentIdx]); dwell < cur.MinDuration {
			return fmt.Errorf("rollout: stage %q dwell time %v < required %v",
				cur.Name, dwell, cur.MinDuration)
		}
		if r.gate != nil {
			if err := r.gate.Check(ctx, cur); err != nil {
				return fmt.Errorf("rollout: health gate failed for %q: %w", cur.Name, err)
			}
		}
	}

	next := r.currentIdx + 1
	if next >= len(r.stages) {
		r.state = RolloutCompleted
		return nil
	}
	r.currentIdx = next
	r.startedAt[next] = r.now()
	r.state = RolloutInProgress
	if next == len(r.stages)-1 && r.stages[next].Percent == 100 {
		// 进入了最后 100% 阶段 —— 立即标 Completed（不再要求二次 Advance）
		r.state = RolloutCompleted
	}
	return nil
}

// Rollback 回退到上一阶段（或 Pending 状态）。
//
//	stage(50%) → Rollback → stage(25%)
//	stage(5%)  → Rollback → Pending（无更早阶段）
//
// Rollback 不检查 MinDuration —— 紧急情况立即生效。
func (r *Rollout) Rollback(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RolloutCompleted {
		return errors.New("rollout: cannot rollback completed rollout (use new release)")
	}
	if r.currentIdx < 0 {
		return errors.New("rollout: nothing to rollback (still pending)")
	}
	r.currentIdx--
	r.state = RolloutRolledBack
	return nil
}

// MarkFailed 把 rollout 标为失败终态（health 严重故障 / 人工放弃）。
func (r *Rollout) MarkFailed(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = RolloutFailed
	r.err = errors.New(reason)
}

// Err 返回 MarkFailed 时记录的原因。
func (r *Rollout) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
