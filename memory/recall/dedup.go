package recall

import (
	"strings"
	"time"
)

// DedupAction 是 dedupUpsert 对一条新抽取事实的裁决（方案 §4.7 R3 / §7-核心 重点一）。
type DedupAction int

const (
	ActionInsert    DedupAction = iota // 新事实：插入
	ActionUpdate                       // 语义重叠且有新增信息：就地更新（修 P5 只增不整合）
	ActionDiscard                      // 完全重复/低价值：丢弃
	ActionSupersede                    // 同主语不同取值（矛盾）：时序取代（旧条标 invalid 留史，G3）
)

func (a DedupAction) String() string {
	switch a {
	case ActionInsert:
		return "insert"
	case ActionUpdate:
		return "update"
	case ActionDiscard:
		return "discard"
	case ActionSupersede:
		return "supersede"
	default:
		return "unknown"
	}
}

// DedupDecision 是裁决结果。TargetID 为 Update/Supersede 命中的旧条目 ID。
type DedupDecision struct {
	Action   DedupAction
	TargetID string
	Reason   string
}

// DedupOptions 调参（由 eval 调，方案 §G1）。
type DedupOptions struct {
	// JaccardThreshold 相似度阈值，>= 视为语义重叠。<=0 → 0.8（对齐现有 file_memory）。
	JaccardThreshold float64
	// SimFn 相似度度量，nil → 词集 Jaccard。CJK 内容应传 LexicalSim（字符二元组），
	// 否则中文整句被当单 token，词集 Jaccard 恒为 0、近义/重复检不出。
	SimFn func(a, b string) float64
}

func (o DedupOptions) jaccard() float64 {
	if o.JaccardThreshold <= 0 {
		return 0.8
	}
	return o.JaccardThreshold
}

func (o DedupOptions) sim() func(string, string) float64 {
	if o.SimFn != nil {
		return o.SimFn
	}
	return jaccardSimilarity
}

// DedupUpsert 决定一条新抽取事实相对现有库的写入动作（方案 §4.7 / §7-核心 重点一「存得净」）。
//
// 规则（确定性，零 LLM；仅在同租户同角色、当前有效的旧条目中比对）：
//  1. 同非空 Subject 且正文不同 → Supersede（矛盾消解：住北京→住上海，旧条留史）。
//  2. 正文完全相同 / 互为子串且无新增 → Discard。
//  3. 词集 Jaccard ≥ 阈值（语义重叠）但正文不同 → Update（就地整合，修只增不整合）。
//  4. 否则 → Insert。
//
// existing 应为「当前有效」集合（调用方先过 IsCurrentlyValid）；本函数对每条再次校验以防御。
func DedupUpsert(newFact Entry, existing []Entry, now time.Time, opts DedupOptions) DedupDecision {
	nc := normalizeContent(newFact.Content)
	if nc == "" {
		return DedupDecision{Action: ActionDiscard, Reason: "empty content"}
	}
	nSubj := strings.TrimSpace(newFact.Subject)
	thr := opts.jaccard()
	sim := opts.sim()

	var bestUpdate string
	for _, e := range existing {
		if !sameUser(e.UserID, newFact.UserID) || e.Role != newFact.Role {
			continue
		}
		if !IsCurrentlyValid(e, now) {
			continue
		}
		ec := normalizeContent(e.Content)
		if ec == "" {
			continue
		}

		// 1) 同主语不同取值 → 时序取代（矛盾消解优先于一切）。
		if nSubj != "" && strings.EqualFold(strings.TrimSpace(e.Subject), nSubj) {
			if ec == nc {
				return DedupDecision{Action: ActionDiscard, TargetID: e.ID, Reason: "same subject, identical value"}
			}
			return DedupDecision{Action: ActionSupersede, TargetID: e.ID, Reason: "same subject, new value"}
		}

		// 2) 完全相同 / 旧条是新条子串（无新增信息）→ 丢弃。
		if ec == nc || strings.Contains(nc, ec) {
			// 旧条 ⊆ 新条：新条信息更全 → 视作整合更新（保留旧 ID 就地改）。
			if ec != nc {
				return DedupDecision{Action: ActionUpdate, TargetID: e.ID, Reason: "new superset of existing"}
			}
			return DedupDecision{Action: ActionDiscard, TargetID: e.ID, Reason: "exact duplicate"}
		}
		// 新条 ⊂ 旧条（旧条信息更全）→ 丢弃新条。
		if strings.Contains(ec, nc) {
			return DedupDecision{Action: ActionDiscard, TargetID: e.ID, Reason: "existing superset of new"}
		}

		// 3) 语义重叠 → 记一个 update 候选（不立即返回，让 supersede 有机会胜出）。
		if bestUpdate == "" && sim(nc, ec) >= thr {
			bestUpdate = e.ID
		}
	}

	if bestUpdate != "" {
		return DedupDecision{Action: ActionUpdate, TargetID: bestUpdate, Reason: "semantic overlap"}
	}
	return DedupDecision{Action: ActionInsert, Reason: "novel fact"}
}

// ApplySupersede 执行时序取代（G3）：旧条标失效留史，新条接续有效窗口并指回旧条。
// 就地改入参指针，便于调用方落盘。
func ApplySupersede(old *Entry, fresh *Entry, now time.Time) {
	if old == nil || fresh == nil {
		return
	}
	t := now
	old.ValidTo = &t
	old.UpdatedAt = now
	fresh.Supersedes = old.ID
	if fresh.ValidFrom.IsZero() {
		fresh.ValidFrom = now
	}
	if fresh.CreatedAt.IsZero() {
		fresh.CreatedAt = now
	}
	fresh.UpdatedAt = now
}

// normalizeContent 归一正文用于比对：trim + 折叠内部空白 + 小写。
func normalizeContent(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// jaccardSimilarity 基于词集的 Jaccard 相似度（与 memory 包同义，本包自持以保持纯净）。
func jaccardSimilarity(a, b string) float64 {
	setA := map[string]struct{}{}
	for _, w := range strings.Fields(a) {
		setA[w] = struct{}{}
	}
	setB := map[string]struct{}{}
	for _, w := range strings.Fields(b) {
		setB[w] = struct{}{}
	}
	if len(setA) == 0 && len(setB) == 0 {
		return 1
	}
	inter := 0
	for w := range setA {
		if _, ok := setB[w]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
