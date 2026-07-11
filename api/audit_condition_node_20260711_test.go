package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// C6：canvas condition 节点条件分支路由（完整实现）。
//
// 引擎升级：新增 deadNodes + deactivatedEdges 状态 + 入边 liveness（nodeDeactivatedByBranch），
// condition 节点求值后停用未选中出边，其下游整枝在拓扑序被跳过。schema：
//
//	condition.data = { source, conditions:[{op,value,target}], default }
//
// 本套测试钉死：命中分支 completed、未选中分支 skipped、default 回退、数值运算符、
// 非法配置显式失败、无条件规则直通向后兼容。
//
// 历史：此前为「最小显式化」（condition 一律 failed）。本次实现真正的条件求值+分支路由，
// 取代旧的显式失败语义——condition 现在会真正判断并只跑选中分支。

func runConditionWorkflow(t *testing.T, wf *WorkflowData, input string) WorkflowRun {
	t.Helper()
	s := newWorkflowTestServer()
	s.workflowStore.workflows[wf.ID] = wf
	body := `{"input":` + mustJSONString(input) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/workflows/"+wf.ID+"/run", strings.NewReader(body))
	req.SetPathValue("id", wf.ID)
	w := httptest.NewRecorder()
	s.handleRunWorkflow(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var run WorkflowRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("解析运行响应失败: %v", err)
	}
	waitForRunCompletion(t, s, run.ID)
	return getRunSnapshot(t, s, run.ID)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func nodeStatusOf(run WorkflowRun, nodeID string) *WorkflowNodeRun {
	for i := range run.NodeResults {
		if run.NodeResults[i].NodeID == nodeID {
			return &run.NodeResults[i]
		}
	}
	return nil
}

// 双分支工作流：input → cond →(A / B)→ sink。cond 命中 → A 激活/B 跳过；否则 default=B。
func branchingWorkflow() *WorkflowData {
	return &WorkflowData{
		ID:   "wf-cond",
		Name: "condition-branch",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "cond", "type": "condition", "data": map[string]any{
				"source":     "input",
				"conditions": []any{map[string]any{"op": "eq", "value": "go", "target": "branchA"}},
				"default":    "branchB",
			}},
			map[string]any{"id": "branchA", "type": "input", "data": map[string]any{"value": "took-A"}},
			map[string]any{"id": "branchB", "type": "input", "data": map[string]any{"value": "took-B"}},
			map[string]any{"id": "sink", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "cond"},
			map[string]any{"source": "cond", "target": "branchA"},
			map[string]any{"source": "cond", "target": "branchB"},
			map[string]any{"source": "branchA", "target": "sink"},
			map[string]any{"source": "branchB", "target": "sink"},
		},
	}
}

func TestWorkflowCondition_MatchActivatesBranchAndSkipsOther(t *testing.T) {
	got := runConditionWorkflow(t, branchingWorkflow(), "go") // eq "go" → branchA
	if got.Status != "completed" {
		t.Fatalf("命中分支运行应 completed，实际 %s: %+v", got.Status, got)
	}
	if a := nodeStatusOf(got, "branchA"); a == nil || a.Status != nodeStatusCompleted {
		t.Fatalf("branchA 应 completed（命中激活），实际 %+v", a)
	}
	if b := nodeStatusOf(got, "branchB"); b == nil || b.Status != nodeStatusSkipped {
		t.Fatalf("branchB 应 skipped（未选中整枝跳过），实际 %+v", b)
	}
	if s := nodeStatusOf(got, "sink"); s == nil || s.Status != nodeStatusCompleted {
		t.Fatalf("sink 应 completed（有 branchA live 入边），实际 %+v", s)
	}
	if !strings.Contains(got.Output, "took-A") || strings.Contains(got.Output, "took-B") {
		t.Fatalf("输出应只含激活分支 took-A，实际 %q", got.Output)
	}
}

func TestWorkflowCondition_NoMatchFallsToDefault(t *testing.T) {
	got := runConditionWorkflow(t, branchingWorkflow(), "stop") // 不等于 "go" → default branchB
	if a := nodeStatusOf(got, "branchA"); a == nil || a.Status != nodeStatusSkipped {
		t.Fatalf("branchA 应 skipped（未命中），实际 %+v", a)
	}
	if b := nodeStatusOf(got, "branchB"); b == nil || b.Status != nodeStatusCompleted {
		t.Fatalf("branchB 应 completed（default 激活），实际 %+v", b)
	}
	if !strings.Contains(got.Output, "took-B") || strings.Contains(got.Output, "took-A") {
		t.Fatalf("输出应只含 default 分支 took-B，实际 %q", got.Output)
	}
}

func TestWorkflowCondition_NumericOperator(t *testing.T) {
	wf := branchingWorkflow()
	wf.Nodes[1] = map[string]any{"id": "cond", "type": "condition", "data": map[string]any{
		"source":     "input",
		"conditions": []any{map[string]any{"op": "gt", "value": "5", "target": "branchA"}},
		"default":    "branchB",
	}}
	hi := runConditionWorkflow(t, wf, "9")
	if a := nodeStatusOf(hi, "branchA"); a == nil || a.Status != nodeStatusCompleted {
		t.Fatalf("9>5 应激活 branchA，实际 %+v", a)
	}
	lo := runConditionWorkflow(t, wf, "3")
	if b := nodeStatusOf(lo, "branchB"); b == nil || b.Status != nodeStatusCompleted {
		t.Fatalf("3>5 假 → default branchB，实际 %+v", b)
	}
}

func TestWorkflowCondition_MalformedConfigFails(t *testing.T) {
	wf := branchingWorkflow()
	wf.Nodes[1] = map[string]any{"id": "cond", "type": "condition", "data": map[string]any{
		"conditions": []any{map[string]any{"op": "no_such_op", "value": "x", "target": "branchA"}},
	}}
	got := runConditionWorkflow(t, wf, "go")
	if got.Status != "failed" {
		t.Fatalf("非法 op 应显式 failed（不静默直通），实际 %s", got.Status)
	}
	if c := nodeStatusOf(got, "cond"); c == nil || c.Status != nodeStatusFailed || !strings.Contains(c.Error, "op") {
		t.Fatalf("cond 应 failed 且错误提示 op 不支持，实际 %+v", c)
	}
}

func TestWorkflowCondition_NoConditionsPassthrough(t *testing.T) {
	// 无 conditions 且无 default = 占位/直通，不停用任何分支（向后兼容）。
	wf := branchingWorkflow()
	wf.Nodes[1] = map[string]any{"id": "cond", "type": "condition", "data": map[string]any{}}
	got := runConditionWorkflow(t, wf, "go")
	if got.Status != "completed" {
		t.Fatalf("无条件 condition 应直通 completed，实际 %s: %+v", got.Status, got)
	}
	if a := nodeStatusOf(got, "branchA"); a == nil || a.Status != nodeStatusCompleted {
		t.Fatalf("直通时 branchA 应 completed，实际 %+v", a)
	}
	if b := nodeStatusOf(got, "branchB"); b == nil || b.Status != nodeStatusCompleted {
		t.Fatalf("直通时 branchB 应 completed，实际 %+v", b)
	}
}
