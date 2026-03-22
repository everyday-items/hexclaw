package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

type workflowTestEngine struct {
	lastMsg *adapter.Message
}

func (e *workflowTestEngine) Start(context.Context) error  { return nil }
func (e *workflowTestEngine) Stop(context.Context) error   { return nil }
func (e *workflowTestEngine) Health(context.Context) error { return nil }

func (e *workflowTestEngine) Process(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
	e.lastMsg = msg
	role := ""
	if msg.Metadata != nil {
		role = msg.Metadata["role"]
	}
	return &adapter.Reply{Content: "[" + role + "] " + msg.Content}, nil
}

func (e *workflowTestEngine) ProcessStream(context.Context, *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	return nil, nil
}

func newWorkflowTestServer() *Server {
	return &Server{
		cfg:           config.DefaultConfig(),
		engine:        &workflowTestEngine{},
		logCollector:  NewLogCollector(10),
		workflowStore: &WorkflowStore{workflows: make(map[string]*WorkflowData), runs: make(map[string]*WorkflowRun), maxRuns: 100},
	}
}

func TestWorkflowRun_ParallelStage(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.workflows["wf-parallel"] = &WorkflowData{
		ID:   "wf-parallel",
		Name: "parallel",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "a", "type": "agent", "data": map[string]any{"role": "researcher", "prompt": "A: {{previous}}"}},
			map[string]any{"id": "b", "type": "agent", "data": map[string]any{"role": "writer", "prompt": "B: {{previous}}"}},
			map[string]any{"id": "out", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "a"},
			map[string]any{"source": "input", "target": "b"},
			map[string]any{"source": "a", "target": "out"},
			map[string]any{"source": "b", "target": "out"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/workflows/wf-parallel/run", strings.NewReader(`{"input":"hello"}`))
	req.SetPathValue("id", "wf-parallel")
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
	got := getRunSnapshot(t, s, run.ID)
	if got.Status != "completed" {
		t.Fatalf("期望 completed，实际 %s: %+v", got.Status, got)
	}
	if !strings.Contains(got.Output, "[researcher]") || !strings.Contains(got.Output, "[writer]") {
		t.Fatalf("并行阶段输出不完整: %q", got.Output)
	}
	if len(got.NodeResults) != 4 {
		t.Fatalf("期望 4 个节点结果，实际 %d", len(got.NodeResults))
	}
}

func TestWorkflowRun_AgentHandoff(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.workflows["wf-handoff"] = &WorkflowData{
		ID:   "wf-handoff",
		Name: "handoff",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "handoff", "type": "handoff", "data": map[string]any{"to_agent": "writer"}},
			map[string]any{"id": "research", "type": "agent", "data": map[string]any{"role": "researcher", "prompt": "R: {{previous}}"}},
			map[string]any{"id": "writer", "type": "agent", "data": map[string]any{"role": "writer", "prompt": "W: {{previous}}"}},
			map[string]any{"id": "out", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "handoff"},
			map[string]any{"source": "handoff", "target": "research"},
			map[string]any{"source": "handoff", "target": "writer"},
			map[string]any{"source": "research", "target": "out"},
			map[string]any{"source": "writer", "target": "out"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/workflows/wf-handoff/run", strings.NewReader(`{"input":"hello"}`))
	req.SetPathValue("id", "wf-handoff")
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
	got := getRunSnapshot(t, s, run.ID)
	if got.Status != "completed" {
		t.Fatalf("期望 completed，实际 %s: %+v", got.Status, got)
	}
	if !strings.Contains(got.Output, "[writer]") {
		t.Fatalf("handoff 后应由 writer 处理，实际输出: %q", got.Output)
	}

	results := make(map[string]WorkflowNodeRun)
	for _, item := range got.NodeResults {
		results[item.NodeID] = item
	}
	if results["research"].Status != nodeStatusSkipped {
		t.Fatalf("research 节点应被跳过，实际 %+v", results["research"])
	}
	if results["writer"].AgentRole != "writer" || results["writer"].Status != nodeStatusCompleted {
		t.Fatalf("writer 节点状态不正确: %+v", results["writer"])
	}
	if results["handoff"].HandoffAgent != "writer" {
		t.Fatalf("handoff 节点未记录目标 agent: %+v", results["handoff"])
	}
}

func TestWorkflowRun_CycleFails(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.workflows["wf-cycle"] = &WorkflowData{
		ID:   "wf-cycle",
		Name: "cycle",
		Nodes: []any{
			map[string]any{"id": "a", "type": "noop"},
			map[string]any{"id": "b", "type": "noop"},
		},
		Edges: []any{
			map[string]any{"source": "a", "target": "b"},
			map[string]any{"source": "b", "target": "a"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/workflows/wf-cycle/run", nil)
	req.SetPathValue("id", "wf-cycle")
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
	got := getRunSnapshot(t, s, run.ID)
	if got.Status != "failed" {
		t.Fatalf("期望 failed，实际 %s: %+v", got.Status, got)
	}
	if !strings.Contains(got.Error, "DAG") {
		t.Fatalf("期望 DAG 错误，实际 %q", got.Error)
	}
}

func TestWorkflowRun_PassesExplicitProviderAndModelToEngine(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.workflows["wf-model"] = &WorkflowData{
		ID:   "wf-model",
		Name: "model",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{"provider": "智谱", "model": "glm-5", "prompt": "{{previous}}"}},
			map[string]any{"id": "out", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "out"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/canvas/workflows/wf-model/run", strings.NewReader(`{"input":"hello"}`))
	req.SetPathValue("id", "wf-model")
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

	eng := s.engine.(*workflowTestEngine)
	if eng.lastMsg == nil {
		t.Fatal("workflow engine 未收到消息")
	}
	if got := eng.lastMsg.Metadata["provider"]; got != "智谱" {
		t.Fatalf("provider 未透传，实际 %q", got)
	}
	if got := eng.lastMsg.Metadata["model"]; got != "glm-5" {
		t.Fatalf("model 未透传，实际 %q", got)
	}
}

func waitForRunCompletion(t *testing.T, s *Server, runID string) {
	t.Helper()
	for range 50 {
		run := getRunSnapshot(t, s, runID)
		if run.Status != "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s 超时未完成", runID)
}

func getRunSnapshot(t *testing.T, s *Server, runID string) WorkflowRun {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/canvas/runs/"+runID, nil)
	req.SetPathValue("id", runID)
	s.handleGetWorkflowRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("查询 run 失败: %d %s", w.Code, w.Body.String())
	}
	var run WorkflowRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("解析 run 响应失败: %v", err)
	}
	return run
}
