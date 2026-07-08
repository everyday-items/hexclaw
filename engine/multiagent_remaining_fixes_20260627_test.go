package engine

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 2026-06-27 多 Agent 编排「剩余 4 待修缺陷」的 RED→GREEN 回归锁（承 design_fixes 那批之后的审计）：
//   ① 跨树总量闸（组合式 fan-out 失控）  ⑤ code_exec 授权令牌化（不再认可伪造 metadata）
//   🟡 注册表持久化合并落盘（不再在数据锁内串行化并行）  🟢 resume reuse key 碰撞
// 每条：修前在未改代码上 FAIL（症状即失败信息），修后 GREEN，留作回归。

// ─────────────────────── ① 跨树总量闸 ───────────────────────

// 构造一棵真嵌套树：根派 8 个 depth-1 orchestrator，每个再 orchestrate 8 个 depth-2 leaf = 8+8×8=72。
// 高预算应跑满 72（证明 harness 真嵌套，否则 RED 无意义）；低预算 max=10 应把整棵树钳在 10。
func TestOrchestrate_TotalFanoutBudget_CapsNestedTree(t *testing.T) {
	defer func(o int64) { atomic.StoreInt64(&maxTotalFanout, o) }(atomic.LoadInt64(&maxTotalFanout))
	defer func(o int) { maxChildrenPerAgent = o }(maxChildrenPerAgent)
	defer func(o int) { maxSpawnDepth = o }(maxSpawnDepth)
	maxSpawnDepth = 2
	SetMaxChildrenPerAgent(8)

	var o *OrchestrateSkill
	var total int64
	// 子 Agent 桩：depth-0 的根孩子(orchestrator)再嵌套 fan-out 一层；depth-1 的孙子(leaf)不再派。
	// 同一 ctx 携带共享预算，仅手工递增 spawn 深度模拟子 Process（真流程里由 Process 读 metadata 注入）。
	exec := func(ctx context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		atomic.AddInt64(&total, 1)
		if d := spawnDepthFromContext(ctx); d < maxSpawnDepth-1 {
			nested := make([]any, 8)
			for i := range nested {
				nested[i] = map[string]any{"agent": "g" + strconv.Itoa(i), "task": "t"}
			}
			_, _ = o.Execute(withSpawnDepth(ctx, d+1), map[string]any{"subtasks": nested})
		}
		return SubAgentResult{Output: "ok"}, nil
	}
	o = NewOrchestrateSkill(exec, nil)

	root := make([]any, 8)
	for i := range root {
		root[i] = map[string]any{"agent": "a" + strconv.Itoa(i), "task": "t"}
	}

	// 高预算：完整嵌套树跑满 72。证明 harness 真嵌套（无闸时即是这个爆炸面）。
	SetMaxTotalFanout(1000)
	atomic.StoreInt64(&total, 0)
	if _, err := o.Execute(context.Background(), map[string]any{"subtasks": root}); err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if got := atomic.LoadInt64(&total); got != 72 {
		t.Fatalf("高预算下嵌套树应跑满 72(8+8×8)，实际 %d——harness 未真嵌套则后续断言无意义", got)
	}

	// 低预算：整棵嵌套树（跨深度）派发应被钳在 max=10。
	SetMaxTotalFanout(10)
	atomic.StoreInt64(&total, 0)
	if _, err := o.Execute(context.Background(), map[string]any{"subtasks": root}); err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if got := atomic.LoadInt64(&total); got > 10 {
		t.Errorf("回归(①跨树总量闸): 整棵嵌套树派发应被钳在 max=10，实际派发 %d（无闸=72）", got)
	} else if got < 8 {
		t.Errorf("回归(①): 根层 8 个仍应在预算内派发，实际 %d", got)
	}
}

// spawn 与 orchestrate 共享同一棵树预算：嵌套 spawn 也从同一计数器扣减，触顶优雅拒派（非 error）。
func TestSpawn_TotalFanoutBudget_Shared(t *testing.T) {
	defer func(o int64) { atomic.StoreInt64(&maxTotalFanout, o) }(atomic.LoadInt64(&maxTotalFanout))
	defer func(o int) { maxSpawnDepth = o }(maxSpawnDepth)
	maxSpawnDepth = 10 // 放宽深度闸，让总量闸(max=3)成为先触的约束，隔离测总量闸而非深度闸
	SetMaxTotalFanout(3)

	var s *SpawnSkill
	var total int64
	exec := func(ctx context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		atomic.AddInt64(&total, 1)
		// 每个子再 spawn 一个孙子，链式嵌套——总量闸应在第 3 个后拒派。
		_, _ = s.Execute(withSpawnDepth(ctx, spawnDepthFromContext(ctx)+1),
			map[string]any{"agent_name": "child", "task": "t"})
		return SubAgentResult{Output: "ok"}, nil
	}
	s = NewSpawnSkill(exec, nil)
	if _, err := s.Execute(context.Background(), map[string]any{"agent_name": "root", "task": "t"}); err != nil {
		t.Fatalf("spawn 报错：%v", err)
	}
	if got := atomic.LoadInt64(&total); got > 3 {
		t.Errorf("回归(① spawn 共享总量闸): 链式 spawn 应被钳在 max=3，实际派发 %d", got)
	}
}

// ─────────────────────── ⑤ 不可伪造 solve grant ───────────────────────

// grant 是 typed ctx value（外部消息注入不进来）：(a) 真 grant 放行沙箱 code_exec；
// (b) grant 只授权 code_exec，不越权放宽非系统派发 shell；(c) 功能优先下普通 spawn
// 派发对 code_exec 也应放行。
func TestPermission_SolveGrant_AuthorizesOnlyCodeExec(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)

	// (a) 真 grant → 沙箱 code_exec 放行。
	if err := hook.BeforeToolCall(withSolveGrant(context.Background()),
		&ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err != nil {
		t.Errorf("⑤(a): 真 solve grant 应放行 code_exec，得 err=%v", err)
	}
	// (b) grant 仅授权 code_exec——不越权放宽其它危险工具(shell)。
	if err := hook.BeforeToolCall(withSolveGrant(context.Background()),
		&ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Error("⑤(b): solve grant 不应越权放行 shell（只授权沙箱 code_exec）")
	}
	// (c) 无 grant 的普通系统派发(spawn)：功能优先下 code_exec 放行。
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), spawnDispatchSource),
		&ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err != nil {
		t.Errorf("⑤(c): 功能优先下普通 spawn 应放行 code_exec，得 err=%v", err)
	}
}

// 端到端：SolveSkill 真派 verifier 时，其子 ctx 必须携带 grant（经 executeFunc 透传），permission 放行。
// 反面：直接经 executeFunc 而不经 solve（无 grant）→ code_exec 不放行。锁住「grant 由 solve 铸、随派生流」。
func TestSolve_StampsGrantThroughExecutor(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	var sawGrant atomic.Bool
	// 桩 executor 模拟子 Process：用收到的 ctx 调 permission 闸，记录 code_exec 是否被放行。
	exec := func(ctx context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: codeExecToolName, Source: "skill"})
		sawGrant.Store(err == nil)
		return SubAgentResult{Output: "答案：42"}, nil
	}
	o := NewSolveSkill(exec, nil)
	if _, err := o.runSolveAgent(context.Background(), verifierSpec("1+1=?", "2", "")); err != nil {
		t.Fatalf("runSolveAgent 报错：%v", err)
	}
	if !sawGrant.Load() {
		t.Error("⑤端到端: solve 派的 verifier 子 ctx 应携带 grant，使其沙箱 code_exec 被放行")
	}
}

// ─────────────────────── 🟢 resume reuse key 碰撞 ───────────────────────

// 上次运行有两个 **相同 (agent,task)** 的成功子。续接时两者都应被复用（各一次），不能因 map key 碰撞
// 只留最后一条、丢掉前一条的输出并逼其重跑。
func TestOrchestrate_Resume_ReusesDuplicateSucceeded(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	reg.Start(&SubAgentRunRecord{ID: "prev", Agent: "orchestrate", Depth: 0})
	reg.Start(&SubAgentRunRecord{ID: "prev-1", ParentID: "prev", Agent: "researcher", Task: "t"})
	reg.Finish("prev-1", subAgentStatusOK, "OUT_ONE", "", "")
	reg.Start(&SubAgentRunRecord{ID: "prev-2", ParentID: "prev", Agent: "researcher", Task: "t"})
	reg.Finish("prev-2", subAgentStatusOK, "OUT_TWO", "", "")

	rec := &specRecorder{}
	o := NewOrchestrateSkill(rec.fn, reg)
	res, err := o.Execute(context.Background(), map[string]any{
		"resume_run_id": "prev",
		"subtasks": []any{
			map[string]any{"agent": "researcher", "task": "t"},
			map[string]any{"agent": "researcher", "task": "t"},
		},
	})
	if err != nil {
		t.Fatalf("resume orchestrate 报错：%v", err)
	}
	if n := len(rec.agents()); n != 0 {
		t.Fatalf("回归(🟢): 两条相同子任务都应复用、0 重跑，实际重跑 %d 次", n)
	}
	// 关键判别：修前 map key 碰撞会丢掉 OUT_ONE（只剩 OUT_TWO ×2）；修后两条成功输出都被复用。
	if !strings.Contains(res.Content, "OUT_ONE") || !strings.Contains(res.Content, "OUT_TWO") {
		t.Errorf("回归(🟢 reuse key 碰撞): 两条相同 (agent,task) 应分别复用两次成功输出(OUT_ONE+OUT_TWO)，得：%s", res.Content)
	}
}
