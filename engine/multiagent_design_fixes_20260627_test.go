package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 2026-06-27 多 Agent 设计缺陷修复的 RED→GREEN 回归锁（审计发现 ①②③⑤⑥⑦）。
// 修前：每条在未改代码上 FAIL（症状即失败信息）；修后 GREEN，留作回归。

// ① synthesizer 不得无条件宣称「已核对冲突」——它只是 LLM 文本归并，可能漏检/编造冲突取舍
// （与 solve 的「高置信」徽标 AP-122 同根：宣称了不可靠的验证）。
func TestOrchestrate_SynthesizerNoFalseConflictClaim(t *testing.T) {
	body := synthesizedBody("归并后的连贯结论……", 3)
	if strings.Contains(body, "已核对冲突") {
		t.Errorf("回归(设计①): synthesizer 不应无条件宣称『已核对冲突』(LLM 归并可能漏检/编造)，得：%s", body)
	}
	if !strings.Contains(body, "归并") {
		t.Errorf("回归(设计①): 仍应说明这是归并结论，得：%s", body)
	}
}

// ② supervisor 反馈环必须能看到「失败子任务」——否则对失败失明，可能据成功部分误判 done。
func TestOrchestrate_SupervisorPromptSurfacesFailures(t *testing.T) {
	results := []agentResult{
		{Agent: "researcher", Task: "查资料A", Output: "A 的结果"},
		{Agent: "coder", Task: "实现模块B", Err: errors.New("编译失败")},
	}
	p := buildSupervisorPrompt("完成 A 与 B 两件事", results)
	// 修前：失败子任务被 skip，prompt 完全不提 → 监工看不到 B 失败。
	if !strings.Contains(p, "实现模块B") {
		t.Errorf("回归(设计②): 监工 prompt 应告知失败子任务(实现模块B)，否则反馈环对失败失明，得：%s", p)
	}
	if !strings.Contains(p, "失败") {
		t.Errorf("回归(设计②): 监工 prompt 应标注失败状态，得：%s", p)
	}
}

// supExec.countOf 数某 agent 被派的次数（③ 去重断言用）。
func (s *supExec) countOf(agent string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.calls {
		if a == agent {
			n++
		}
	}
	return n
}

// ③ supervisor 补派的子任务若与「已派过的」重复，必须程序级去重——不能只靠 prompt 一句口头约束。
func TestOrchestrate_Supervisor_DedupsRedispatch(t *testing.T) {
	SetOrchestrateSynthesis(false)
	defer func(o int) { maxSupervisorRounds = o }(maxSupervisorRounds)
	SetMaxSupervisorRounds(3)

	// 监工补派含一个「已派过的」子任务(a0/t0 重复) + 一个新的(b/t1)。
	se := &supExec{supDecisions: []string{`{"done":false,"reason":"缺","next":[{"agent":"a0","task":"t0"},{"agent":"b","task":"t1"}]}`}}
	o := NewOrchestrateSkill(se.fn, nil)
	_, err := o.Execute(context.Background(), map[string]any{
		"goal":       "g",
		"max_rounds": float64(3),
		"subtasks":   []any{map[string]any{"agent": "a0", "task": "t0"}},
	})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if n := se.countOf("a0"); n != 1 {
		t.Errorf("回归(设计③): 已派过的 a0/t0 被监工重派后不应再跑(去重)，实跑 %d 次，calls=%v", n, se.calls)
	}
	if !se.called("b") {
		t.Errorf("回归(设计③): 新子任务 b 应正常派发，calls=%v", se.calls)
	}
}

// ⑤ grant 仍是 solve 内部授权的权威来源；source=solve 来自 metadata，不能单独作为
// code_exec 授权依据。
func TestPermission_ForgedSolveSourceTopLevelDeniedWithoutGrant(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	// 伪造 source=solve 但没有 grant → 拒绝。
	ctxForged := withSpawnDepth(withSystemDispatch(context.Background(), solveDispatchSource), maxSpawnDepth)
	if err := hook.BeforeToolCall(ctxForged, &ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err == nil {
		t.Error("伪造 source=solve 不应放行 code_exec；必须携带 solve grant")
	}
	// 真 solve 派生：携带不可伪造 grant → 放行沙箱 code_exec。
	ctxReal := withSolveGrant(context.Background())
	if err := hook.BeforeToolCall(ctxReal, &ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err != nil {
		t.Errorf("回归(设计⑤根治): 真 solve grant 应放行沙箱 code_exec，得 err=%v", err)
	}
}

// ⑥ isTransientErr 不得把含数字子串(如"500 items")的普通错误误判为瞬时可重试。
func TestResilience_TransientNotOverbroad(t *testing.T) {
	if isTransientErr(errors.New("solver computed 500 items but the result mismatched")) {
		t.Error("回归(设计⑥): 含'500'数量的普通错误不应判为瞬时可重试(误重试浪费)")
	}
	// 真瞬时仍要 true（防过度收紧）。
	for _, e := range []string{
		"openai api error: 429 rate limit reached",
		"context deadline exceeded",
		"api response status 502 bad gateway",
		"service unavailable",
		"connection reset by peer",
	} {
		if !isTransientErr(errors.New(e)) {
			t.Errorf("回归(设计⑥): 真瞬时错误 %q 仍应判瞬时", e)
		}
	}
}

// ⑦ spawn 子 Agent 非超时失败应优雅降级为部分结果(与 orchestrate 一致)，不返回硬错中断工具调用。
func TestSpawn_FailureGracefulNotHardError(t *testing.T) {
	failExec := func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		return SubAgentResult{}, errors.New("permanent boom") // 非瞬时、非超时
	}
	s := NewSpawnSkill(failExec, NewSubAgentRegistry(""))
	res, err := s.Execute(context.Background(), map[string]any{"agent_name": "coder", "task": "做点事"})
	if err != nil {
		t.Errorf("回归(设计⑦): spawn 子 Agent 失败应优雅降级(与 orchestrate 一致)，不应返回硬错，得 err=%v", err)
	}
	if res == nil || (!strings.Contains(res.Content, "失败") && !strings.Contains(res.Content, "fail")) {
		t.Errorf("回归(设计⑦): 失败应在结果正文说明而非中断，得：%v", res)
	}
}
