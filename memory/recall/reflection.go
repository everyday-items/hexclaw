package recall

import (
	"sort"
	"strings"
	"time"
)

// 闭环「整合/反思」（§4.4.2 / §7.3 / §7-核心 修正2）：**严格机械化** —— 反思只产出
// 「搬运/标记」型操作（去重 / 时序取代 / 晋升 / 降级 / 归档），**绝不 LLM 改写条目内容**。
// Op 结构本身只携带条目 ID（无 Content 字段），从类型上保证「不改写内容」这条硬约束。

// OpAction 反思操作类型。
type OpAction int

const (
	OpDedup     OpAction = iota // TargetID 是 OtherID 的近重复 → 删并（保留 OtherID）
	OpSupersede                 // TargetID 被 OtherID 时序取代（旧条标失效，留史）
	OpPromote                   // TargetID 晋升常驻
	OpDemote                    // TargetID 降级检索
	OpArchive                   // TargetID 归档
)

func (a OpAction) String() string {
	switch a {
	case OpDedup:
		return "dedup"
	case OpSupersede:
		return "supersede"
	case OpPromote:
		return "promote"
	case OpDemote:
		return "demote"
	case OpArchive:
		return "archive"
	default:
		return "unknown"
	}
}

// Op 是一条机械化反思操作。**只含 ID，不含内容** —— 类型层面杜绝 LLM 改写（修正2）。
type Op struct {
	Action   OpAction
	TargetID string
	OtherID  string // dedup 的保留方 / supersede 的新条
}

// Reflect 对一组当前有效记忆做保守机械整合，返回操作列表（不落盘——由单写器据此 apply）。
//
// 步骤：① 两两去重/时序取代（同租户同角色）② 逐条生命周期转移。确定性、零 LLM。
func Reflect(entries []Entry, now time.Time, policy LifecyclePolicy, dedup DedupOptions) []Op {
	sim := dedup.sim()
	thr := dedup.jaccard()
	handled := make(map[string]bool) // 已被去重/取代消费的条目不再做生命周期

	var ops []Op
	for i := 0; i < len(entries); i++ {
		a := entries[i]
		if handled[a.ID] {
			continue
		}
		for j := i + 1; j < len(entries); j++ {
			b := entries[j]
			if handled[b.ID] || a.UserID != b.UserID || a.Role != b.Role {
				continue
			}
			as, bs := strings.TrimSpace(a.Subject), strings.TrimSpace(b.Subject)
			ac, bc := normalizeContent(a.Content), normalizeContent(b.Content)

			// 同主语异值 → 时序取代较早者（留史）。
			if as != "" && strings.EqualFold(as, bs) && ac != bc {
				old, fresh := a, b
				if fresh.CreatedAt.Before(old.CreatedAt) {
					old, fresh = b, a
				}
				ops = append(ops, Op{Action: OpSupersede, TargetID: old.ID, OtherID: fresh.ID})
				handled[old.ID] = true
				if old.ID == a.ID {
					break // a 已被取代，停止其内层比较
				}
				continue
			}
			// 近重复 → 保留召回更高（次之较早）者，另一条去重。
			if sim(ac, bc) >= thr {
				keep, drop := a, b
				if drop.RecallCount > keep.RecallCount {
					keep, drop = b, a
				}
				ops = append(ops, Op{Action: OpDedup, TargetID: drop.ID, OtherID: keep.ID})
				handled[drop.ID] = true
				if drop.ID == a.ID {
					break
				}
			}
		}
	}

	for _, e := range entries {
		if handled[e.ID] {
			continue
		}
		switch policy.Evaluate(e, now) {
		case Promote:
			ops = append(ops, Op{Action: OpPromote, TargetID: e.ID})
		case Demote:
			ops = append(ops, Op{Action: OpDemote, TargetID: e.ID})
		case Archive:
			ops = append(ops, Op{Action: OpArchive, TargetID: e.ID})
		}
	}

	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].Action != ops[j].Action {
			return ops[i].Action < ops[j].Action
		}
		return ops[i].TargetID < ops[j].TargetID
	})
	return ops
}
