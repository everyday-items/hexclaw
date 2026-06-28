package recall

import "time"

// 闭环「生命周期状态机」（§6ter / §6bis.C）：确定性阈值转移，零 LLM。
// 由反思（Reflect）周期遍历执行；用户/manage_memory 可手动 pin（锁常驻）/archive 覆盖。

// Transition 是一次生命周期评估的转移结果。
type Transition int

const (
	Stay    Transition = iota // 不变
	Promote                   // T2 检索 → T1 常驻（高频或硬类型）
	Demote                    // T1 常驻 → T2 检索（长期不召回）
	Archive                   // → 归档（衰减/陈旧/被取代；可恢复，非删）
)

func (t Transition) String() string {
	switch t {
	case Promote:
		return "promote"
	case Demote:
		return "demote"
	case Archive:
		return "archive"
	default:
		return "stay"
	}
}

// LifecyclePolicy 生命周期阈值（可由 eval 调）。
type LifecyclePolicy struct {
	PromoteRecallCount int     // 检索条召回次数 ≥ 此值 → 晋升常驻（§6bis.C，默认 3）
	ArchiveRecency     float64 // 检索条 recency < 且 importance 低 → 归档（默认 0.05 ≈ 半衰数倍）
	ArchiveImportance  float64 // 配合 ArchiveRecency 的 importance 上限（默认 0.55，刚好高于
	//                            「无暗示词普通 fact」基线 0.5 → 陈旧普通事实归档、显著事实保留）
	DemoteRecency float64 // 常驻 preference 长期不召回阈值（默认 0.02）
	LambdaPerDay  float64 // recency 衰减率，<=0 → DefaultLambdaPerDay
}

// DefaultLifecyclePolicy 返回方案默认阈值。
func DefaultLifecyclePolicy() LifecyclePolicy {
	return LifecyclePolicy{
		PromoteRecallCount: 3,
		ArchiveRecency:     0.05,
		ArchiveImportance:  0.55,
		DemoteRecency:      0.02,
		LambdaPerDay:       DefaultLambdaPerDay,
	}
}

// Evaluate 评估单条记忆的生命周期转移（确定性）。
//
//	pinned                         → Stay（逃生口，永不自动移动）
//	被取代/失效（!currentlyValid）   → Archive
//	检索层 且 RecallCount≥阈值        → Promote
//	检索层 且 recency低 且 importance低 → Archive（衰减归档）
//	常驻 preference 长期零召回         → Demote
//	rule/identity/instruction        → Stay（硬类型常驻）
func (p LifecyclePolicy) Evaluate(e Entry, now time.Time) Transition {
	if e.Pinned {
		return Stay
	}
	if !IsCurrentlyValid(e, now) {
		return Archive
	}
	lambda := p.LambdaPerDay
	if lambda <= 0 {
		lambda = DefaultLambdaPerDay
	}
	rec := Recency(e, now, lambda)

	if e.IsResident() {
		// 硬类型常驻不动；preference 长期零召回可降级释放预算。
		if e.Type == TypePreference && e.RecallCount == 0 && rec < p.DemoteRecency {
			return Demote
		}
		return Stay
	}

	// 检索层（fact/context）
	if e.RecallCount >= p.PromoteRecallCount {
		return Promote
	}
	if rec < p.ArchiveRecency && Importance(e) < p.ArchiveImportance {
		return Archive
	}
	return Stay
}
