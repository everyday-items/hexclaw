// goalloop.go 是结构化目标的 **reconciler**——Loop Engineering 的汇聚点：
//
//	#1 Goal 契约 + #2 验收引擎 + #3 独立 checker → 判 done（可机器判定优先）
//	#4 无进展/震荡检测 + 语义化终止（done / capped / escalated）
//	#5 每轮落盘、level-triggered 续跑
//	#6 Step：每个 cron/webhook tick 推进一步，跨 tick 续跑（持续职能）
//	#8 卡死/低置信/高风险 → HITL 升级，而不是静默截断或空转
//
// 它和现状 orchestrate 的关系：orchestrate 是"一轮扇出 + 监工补派"的内层；GoalLoop 是
// 外层 reconciler——把 maker（可由 orchestrate 实现）一轮轮推进、对照形式化目标验收、
// 持久化进度、按语义终止。maker/checker/store/sink 全部注入，便于测试与替换。
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MakeResult 是 maker 推进一轮的产出。
type MakeResult struct {
	Output   string // 本轮产出（喂给 checker 验收）
	HighRisk bool   // 本轮触达高风险动作（由风险闸/maker 标记 → #8 升级判据）
	Tokens   int    // 本轮消耗 token（目标级预算累计）
}

// Maker 推进目标一轮：给定意图 + 上一轮反馈（验收未过的原因/进展），产出本轮结果。
// 真实接线里它会调 orchestrate 扇出或单 agent 一轮；抽象出来便于测试与解耦。
type Maker interface {
	Make(ctx context.Context, intent, feedback string, round int) (MakeResult, error)
}

// MakerFunc 把函数适配成 Maker。
type MakerFunc func(ctx context.Context, intent, feedback string, round int) (MakeResult, error)

// Make 实现 Maker。
func (f MakerFunc) Make(ctx context.Context, intent, feedback string, round int) (MakeResult, error) {
	return f(ctx, intent, feedback, round)
}

// Escalation 是一次 HITL 升级（#8）：loop 处理不了，交回给人。
type Escalation struct {
	GoalID string
	Reason string // "no_progress" | "low_confidence" | "high_risk"
	Round  int
	Detail string
	State  RunState
}

// EscalationSink 接收升级（如桌面通知）。可空。
type EscalationSink interface {
	Escalate(ctx context.Context, e Escalation) error
}

// EscalationFunc 把函数适配成 EscalationSink。
type EscalationFunc func(ctx context.Context, e Escalation) error

// Escalate 实现 EscalationSink。
func (f EscalationFunc) Escalate(ctx context.Context, e Escalation) error { return f(ctx, e) }

// LoopResult 是一次 reconcile（Run 全程，或 Step 一步）的结果。
type LoopResult struct {
	Outcome      Outcome
	Rounds       int
	FinalOutput  string
	FinalVerdict CheckerVerdict
	Escalation   *Escalation
	State        RunState
}

// GoalLoop 是结构化目标 reconciler。
type GoalLoop struct {
	maker   Maker
	checker *Checker
	store   StateStore
	sink    EscalationSink   // 可空：无则升级仅落 state + 随结果返回
	now     func() time.Time // 可注入（测试）；默认 time.Now
}

// NewGoalLoop 构造 reconciler。store 为空用内存存储；sink 可空。
func NewGoalLoop(maker Maker, checker *Checker, store StateStore, sink EscalationSink) *GoalLoop {
	if store == nil {
		store = NewMemStateStore()
	}
	return &GoalLoop{maker: maker, checker: checker, store: store, sink: sink, now: time.Now}
}

// Run 把目标驱动到一个终态（或在升级/触顶处停下）。一次调用跑完所有轮。
// level-triggered：从 store 载入上次状态续跑；已是终态则幂等返回。
func (l *GoalLoop) Run(ctx context.Context, goalIn Goal) (LoopResult, error) {
	unlock := lockGoal(resolveGoalID(goalIn)) // 串行化同一 goal 的并发推进（#7）
	defer unlock()
	goal, st, early, err := l.prepare(ctx, goalIn)
	if early != nil {
		return *early, err
	}
	var last CheckerVerdict
	for st.Round < goal.Budget.MaxRounds {
		// 墙钟闸只约束单次 Run 的连续推进（in-session backstop）；跨 tick 的 Step 由轮数/token/
		// 无进展兜底，不按日历流逝判——否则常驻目标会在第二个 tick 被误判触顶（#2）。
		if goal.Budget.MaxWall > 0 && l.now().UnixNano()-st.StartedUnixNano > int64(goal.Budget.MaxWall) {
			return l.finish(ctx, &st, OutcomeCapped, last, nil)
		}
		terminal, oc, esc, v := l.advance(ctx, &st, goal)
		last = v
		if terminal {
			return l.finish(ctx, &st, oc, v, esc)
		}
		if perr := l.store.Save(ctx, st); perr != nil {
			return LoopResult{Outcome: OutcomeError, State: st}, fmt.Errorf("save run state: %w", perr)
		}
	}
	return l.finish(ctx, &st, OutcomeCapped, last, nil)
}

// Step 推进一轮（#6：cron/webhook 每个 tick 调一次，把单次任务升级成跨 tick 持续职能）。
// running=true 表示还需后续 tick；false 表示已终态、可从调度里摘除。level-triggered：
// 状态全在 store，进程可在 tick 间被杀，下个 tick 续上。
func (l *GoalLoop) Step(ctx context.Context, goalIn Goal) (res LoopResult, running bool, err error) {
	unlock := lockGoal(resolveGoalID(goalIn)) // 串行化同一 goal 的并发 tick（#7）
	defer unlock()
	goal, st, early, perr := l.prepare(ctx, goalIn)
	if early != nil {
		return *early, false, perr // 终态/校验失败：不再 running
	}
	if st.Round >= goal.Budget.MaxRounds {
		r, ferr := l.finish(ctx, &st, OutcomeCapped, CheckerVerdict{}, nil)
		return r, false, ferr
	}
	terminal, oc, esc, v := l.advance(ctx, &st, goal)
	if terminal {
		r, ferr := l.finish(ctx, &st, oc, v, esc)
		return r, false, ferr
	}
	if serr := l.store.Save(ctx, st); serr != nil {
		return LoopResult{Outcome: OutcomeError, State: st}, false, fmt.Errorf("save run state: %w", serr)
	}
	return LoopResult{Outcome: OutcomeRunning, Rounds: st.Round, FinalOutput: st.LastOutput, FinalVerdict: v, State: st}, true, nil
}

// prepare 载入/初始化状态。early != nil 时调用方应直接返回它（校验失败或已终态）。
func (l *GoalLoop) prepare(ctx context.Context, goalIn Goal) (goal Goal, st RunState, early *LoopResult, err error) {
	if verr := goalIn.Validate(); verr != nil {
		return goalIn, RunState{}, &LoopResult{Outcome: OutcomeError}, verr
	}
	goal = goalIn.WithDefaults()
	if strings.TrimSpace(goal.ID) == "" {
		goal.ID = resolveGoalID(goal) // 无 ID 时按意图 + 验收派生稳定 ID，支撑续跑且不撞车（#8）
	}
	loaded, _, lerr := l.store.Load(ctx, goal.ID)
	if lerr != nil {
		return goal, RunState{}, &LoopResult{Outcome: OutcomeError}, fmt.Errorf("load run state: %w", lerr)
	}
	st = loaded
	st.GoalID = goal.ID
	if st.StartedUnixNano == 0 {
		st.StartedUnixNano = l.now().UnixNano()
	}
	if st.Outcome != OutcomeRunning {
		return goal, st, &LoopResult{Outcome: st.Outcome, Rounds: st.Round, FinalOutput: st.LastOutput, State: st}, nil
	}
	return goal, st, nil, nil
}

// advance 跑一轮（含轮前预算闸）。terminal=true 表示应停（oc 为终态）；否则本轮已推进、可继续。
// 墙钟闸不在此处：它只属于单次 Run 的连续推进（见 Run），跨 tick 的 Step 不按日历流逝判（#2）。
func (l *GoalLoop) advance(ctx context.Context, st *RunState, goal Goal) (terminal bool, oc Outcome, esc *Escalation, v CheckerVerdict) {
	if goal.Budget.MaxTokens > 0 && st.TokensUsed >= goal.Budget.MaxTokens {
		return true, OutcomeCapped, nil, CheckerVerdict{}
	}
	feedback := ""
	if n := len(st.History); n > 0 {
		feedback = st.History[n-1].Reason
	}
	mr, merr := l.maker.Make(ctx, goal.Intent, feedback, st.Round+1)
	if merr != nil {
		return true, OutcomeError, nil, CheckerVerdict{}
	}
	st.Round++
	st.TokensUsed += mr.Tokens

	verdict := l.checker.Assess(ctx, goal, mr.Output)
	score := verdict.Verdict.Score // 仅 acceptance 模式有意义

	// #4 进展/震荡检测
	h := hashOutput(mr.Output)
	identical := h == lastHash(*st)
	// 只有"曾有正分却不再提升"才算 score 停滞；二元验收过前恒 0（0<=0）不该被算成停滞，
	// 否则每轮不同的真实尝试会被误判 no_progress 过早升级（#4）。卡死仍由 identical/震荡兜底。
	scoreStalled := verdict.Mode == "acceptance" && st.LastScore > 0 && score <= st.LastScore
	oscillated := st.Round > 1 && containsHash(st.OutputHashes, h)
	noProgress := identical || scoreStalled || oscillated
	if noProgress {
		st.NoProgressStreak++
	} else {
		st.NoProgressStreak = 0
	}
	st.OutputHashes = appendCapped(st.OutputHashes, h, 16)
	st.LastOutput = mr.Output
	st.LastScore = score
	st.History = append(st.History, RoundRecord{
		Round: st.Round, Done: verdict.Done, Score: score,
		Confidence: verdict.Confidence, NoProgress: noProgress, Reason: verdict.Reason,
	})

	if verdict.Done {
		return true, OutcomeDone, nil, verdict
	}
	if reason := l.escalationReason(goal, verdict, mr, st.NoProgressStreak); reason != "" {
		return true, OutcomeEscalated, &Escalation{GoalID: goal.ID, Reason: reason, Round: st.Round, Detail: verdict.Reason}, verdict
	}
	return false, OutcomeRunning, nil, verdict
}

// escalationReason 按升级策略判定（空 = 不升级）。顺序：高风险 > 低置信 > 无进展。
func (l *GoalLoop) escalationReason(goal Goal, v CheckerVerdict, mr MakeResult, streak int) string {
	if goal.Escalation.OnHighRisk && mr.HighRisk {
		return "high_risk"
	}
	if goal.Escalation.MinConfidence > 0 && v.Confidence < goal.Escalation.MinConfidence {
		return "low_confidence"
	}
	if streak >= goal.Escalation.NoProgressRounds {
		return "no_progress"
	}
	return ""
}

// finish 落终态 + best-effort 升级通知。终态落盘失败必须上报调用方——否则续跑会重做
// 已完成的工作 / 重放副作用（#6）。LoopResult 仍带真实终态，error 仅标注"终态未持久"。
func (l *GoalLoop) finish(ctx context.Context, st *RunState, oc Outcome, v CheckerVerdict, esc *Escalation) (LoopResult, error) {
	st.Outcome = oc
	saveErr := l.store.Save(ctx, *st)
	if esc != nil {
		esc.State = *st
		if l.sink != nil {
			_ = l.sink.Escalate(ctx, *esc) // 通知失败不改终态
		}
	}
	res := LoopResult{Outcome: oc, Rounds: st.Round, FinalOutput: st.LastOutput, FinalVerdict: v, Escalation: esc, State: *st}
	if saveErr != nil {
		return res, fmt.Errorf("save terminal state: %w", saveErr)
	}
	return res, nil
}

// resolveGoalID 解析目标的持久化 key：显式 ID 优先，否则按意图 + 验收断言派生稳定 ID。
// 含验收断言（而非仅意图）：同意图不同验收的目标不该共用落盘状态（#8）。
func resolveGoalID(g Goal) string {
	if id := strings.TrimSpace(g.ID); id != "" {
		return id
	}
	return "goal-" + hashGoalIdentity(g)
}

// hashGoalIdentity 对目标的身份字段（意图 + 各条验收断言）取稳定短哈希。
func hashGoalIdentity(g Goal) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(g.Intent)))
	h.Write([]byte{0})
	for _, s := range g.Acceptance {
		fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1f%s\x1f%d\x1f%d\x1f%t\x00",
			s.Kind, s.Value, s.Path, s.Pattern, s.Threshold, s.Weight, s.Optional)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// goalLocks 串行化同一 goalID 的 Run/Step——防跨 tick 重入或并发对同一持久状态做
// read-modify-write 互相覆盖（lost update，-race 照不出的逻辑 TOCTOU）（#7）。
var goalLocks = struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}{m: make(map[string]*sync.Mutex)}

func lockGoal(id string) func() {
	goalLocks.mu.Lock()
	lk := goalLocks.m[id]
	if lk == nil {
		lk = &sync.Mutex{}
		goalLocks.m[id] = lk
	}
	goalLocks.mu.Unlock()
	lk.Lock()
	return lk.Unlock
}

func hashOutput(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:8])
}

func lastHash(st RunState) string {
	if len(st.OutputHashes) == 0 {
		return ""
	}
	return st.OutputHashes[len(st.OutputHashes)-1]
}

func containsHash(hs []string, h string) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}

func appendCapped(hs []string, h string, limit int) []string {
	hs = append(hs, h)
	if len(hs) > limit {
		hs = hs[len(hs)-limit:]
	}
	return hs
}
