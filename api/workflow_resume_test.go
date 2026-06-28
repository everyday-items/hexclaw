package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
)

// Ph5：run 文件持久化往返——重启后运行历史（含可续接的节点输出）不丢。
func TestWorkflowRuns_PersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow_runs.json")
	ws := &WorkflowStore{
		workflows:    map[string]*WorkflowData{},
		runs:         map[string]*WorkflowRun{},
		maxRuns:      100,
		runsFilePath: path,
	}
	ws.addRun(&WorkflowRun{
		ID:         "run-1",
		WorkflowID: "wf-1",
		Status:     "failed",
		Input:      "hello",
		StartedAt:  time.Now(),
		NodeResults: []WorkflowNodeRun{
			{NodeID: "n1", Type: "agent", Status: nodeStatusCompleted, Output: "done-1"},
			{NodeID: "n2", Type: "agent", Status: nodeStatusFailed, Error: "boom"},
		},
	})

	// 新 store 从同一文件加载 —— 模拟重启。
	ws2 := &WorkflowStore{runs: map[string]*WorkflowRun{}, runsFilePath: path}
	ws2.loadRunsFromFile()

	got, ok := ws2.runs["run-1"]
	if !ok {
		t.Fatalf("重启后应能加载 run-1，实得 %d 条", len(ws2.runs))
	}
	if got.Status != "failed" || got.Input != "hello" {
		t.Errorf("run 字段未还原：%+v", got)
	}
	if len(got.NodeResults) != 2 || got.NodeResults[0].Output != "done-1" {
		t.Errorf("节点结果未还原：%+v", got.NodeResults)
	}
	if len(ws2.runOrder) != 1 || ws2.runOrder[0] != "run-1" {
		t.Errorf("LRU 顺序未重建：%v", ws2.runOrder)
	}
}

// Ph5：续接——已完成节点跳过重跑，直接复用缓存输出并回灌 state，不触达引擎。
func TestExecuteNode_ResumesCompletedNode(t *testing.T) {
	e := &workflowExecutor{
		nodes:    map[string]*workflowNode{},
		order:    map[string]int{},
		nodeRuns: map[string]*WorkflowNodeRun{},
		resumed:  map[string]string{"n1": "cached-output"},
	}
	node := &workflowNode{ID: "n1", Type: "agent", Label: "researcher"}
	state := hexagon.MapState{
		stateKeyNodeOutputs:  map[string]string{},
		stateKeyNodeHandoffs: map[string]string{},
	}

	// server 为 nil：若续接逻辑没跳过、真去调引擎，会 panic/报错——以此证明确实跳过了。
	newState, err := e.executeNode(context.Background(), state, node)
	if err != nil {
		t.Fatalf("续接节点不应执行引擎、不应报错：%v", err)
	}
	if got := stringMapStateValue(newState, stateKeyNodeOutputs)["n1"]; got != "cached-output" {
		t.Errorf("续接应把缓存输出回灌 state，实得 %q", got)
	}
	run := e.nodeRuns["n1"]
	if run == nil || run.Status != nodeStatusCompleted || run.Output != "cached-output" {
		t.Errorf("续接节点应标记 completed + 缓存输出，实得 %+v", run)
	}
}

// 非续接节点（不在 resumed 集）走正常路径——这里用 output 节点验证不被误跳过。
func TestExecuteNode_NonResumedNotSkipped(t *testing.T) {
	e := &workflowExecutor{
		nodes:    map[string]*workflowNode{},
		order:    map[string]int{},
		nodeRuns: map[string]*WorkflowNodeRun{},
		resumed:  map[string]string{"n1": "cached"},
	}
	// output 节点直接透传 input，不触引擎；它不在 resumed 里，应正常执行而非跳过。
	node := &workflowNode{ID: "n2", Type: "output", Label: "out"}
	state := hexagon.MapState{
		stateKeyInput:        "final",
		stateKeyNodeOutputs:  map[string]string{},
		stateKeyNodeHandoffs: map[string]string{},
	}
	newState, err := e.executeNode(context.Background(), state, node)
	if err != nil {
		t.Fatalf("output 节点不应报错：%v", err)
	}
	if got := stringStateValue(newState, stateKeyWorkflowOutput); got != "final" {
		t.Errorf("非续接 output 节点应正常透传 input，实得 %q", got)
	}
}
