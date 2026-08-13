package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBUG20260726035TerminalWorkflowRunProjectsFrozenRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.runsFilePath = filepath.Join(t.TempDir(), "workflow_runs.json")
	s.cfg.LLM.Default = "hexclaw-route"
	s.cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"hexclaw-route": {
			DisplayName: "HexClaw-GPT",
			Model:       "gpt-5.6-sol",
		},
	}
	s.workflowStore.workflows["wf-route-snapshot"] = &WorkflowData{
		ID:   "wf-route-snapshot",
		Name: "route snapshot",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{
				"provider": "hexclaw-route",
				"model":    "gpt-5.6-sol",
				"prompt":   "{{previous}}",
			}},
			map[string]any{"id": "output", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "output"},
		},
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/canvas/workflows/wf-route-snapshot/run",
		strings.NewReader(`{"input":"hello"}`),
	)
	request.SetPathValue("id", "wf-route-snapshot")
	response := httptest.NewRecorder()
	s.handleRunWorkflow(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start run: status=%d body=%s", response.Code, response.Body.String())
	}

	var started WorkflowRun
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started run: %v", err)
	}
	waitForRunCompletion(t, s, started.ID)

	// Mutating the current workflow definition after execution must not rewrite
	// the terminal run's historical route facts.
	s.workflowStore.workflows["wf-route-snapshot"].Nodes[1] = map[string]any{
		"id": "agent", "type": "agent", "data": map[string]any{
			"provider": "Changed-Provider",
			"model":    "changed-model",
			"prompt":   "{{previous}}",
		},
	}

	terminal := getWorkflowRunJSONObject(t, s, started.ID)
	if got := terminal["status"]; got != "completed" {
		t.Fatalf("terminal status=%v, want completed", got)
	}
	if got := terminal["provider_display_name"]; got != "HexClaw-GPT" {
		t.Fatalf("provider_display_name=%v, want frozen HexClaw-GPT", got)
	}
	if got := terminal["model_id"]; got != "gpt-5.6-sol" {
		t.Fatalf("model_id=%v, want frozen gpt-5.6-sol", got)
	}

	reloadedStore := &WorkflowStore{
		workflows:    map[string]*WorkflowData{},
		runs:         map[string]*WorkflowRun{},
		maxRuns:      100,
		runsFilePath: s.workflowStore.runsFilePath,
	}
	reloadedStore.loadRunsFromFile()
	reloadedServer := newWorkflowTestServer()
	reloadedServer.workflowStore = reloadedStore
	reloaded := getWorkflowRunJSONObject(t, reloadedServer, started.ID)
	if got := reloaded["provider_display_name"]; got != "HexClaw-GPT" {
		t.Fatalf("reloaded provider_display_name=%v, want frozen HexClaw-GPT", got)
	}
	if got := reloaded["model_id"]; got != "gpt-5.6-sol" {
		t.Fatalf("reloaded model_id=%v, want frozen gpt-5.6-sol", got)
	}
}

func TestBUG20260726035LegacyWorkflowRunProjectsExplicitNullRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	path := filepath.Join(t.TempDir(), "workflow_runs.json")
	if err := os.WriteFile(path, []byte(`{
		"legacy-no-route-facts": {
			"id": "legacy-no-route-facts",
			"workflow_id": "legacy-workflow",
			"status": "completed"
		}
	}`), 0o640); err != nil {
		t.Fatal(err)
	}
	s.workflowStore = &WorkflowStore{
		workflows:    map[string]*WorkflowData{},
		runs:         map[string]*WorkflowRun{},
		maxRuns:      100,
		runsFilePath: path,
	}
	s.workflowStore.loadRunsFromFile()

	legacy := getWorkflowRunJSONObject(t, s, "legacy-no-route-facts")
	provider, providerPresent := legacy["provider_display_name"]
	if !providerPresent || provider != nil {
		t.Fatalf("legacy provider_display_name must be present null, present=%v value=%v", providerPresent, provider)
	}
	model, modelPresent := legacy["model_id"]
	if !modelPresent || model != nil {
		t.Fatalf("legacy model_id must be present null, present=%v value=%v", modelPresent, model)
	}
}

func TestBUG20260726035PreModelFailureProjectsExplicitNullRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	s.cfg.LLM.Default = "hexclaw-route"
	s.cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"hexclaw-route": {
			DisplayName: "HexClaw-GPT",
			Model:       "gpt-5.6-sol",
		},
	}
	s.workflowStore.workflows["wf-route-pre-model-failure"] = &WorkflowData{
		ID:   "wf-route-pre-model-failure",
		Name: "route pre-model failure",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{
				"provider": "hexclaw-route",
				"model":    "gpt-5.6-sol",
				"prompt":   "{{previous}}",
			}},
			map[string]any{"id": "output", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "output"},
			map[string]any{"source": "output", "target": "agent"},
		},
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/canvas/workflows/wf-route-pre-model-failure/run",
		strings.NewReader(`{"input":"hello"}`),
	)
	request.SetPathValue("id", "wf-route-pre-model-failure")
	response := httptest.NewRecorder()
	s.handleRunWorkflow(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start run: status=%d body=%s", response.Code, response.Body.String())
	}

	var started WorkflowRun
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started run: %v", err)
	}
	waitForRunCompletion(t, s, started.ID)

	terminal := getWorkflowRunJSONObject(t, s, started.ID)
	if got := terminal["status"]; got != "failed" {
		t.Fatalf("terminal status=%v, want failed", got)
	}
	provider, providerPresent := terminal["provider_display_name"]
	if !providerPresent || provider != nil {
		t.Fatalf("pre-model failure provider_display_name must be present null, present=%v value=%v", providerPresent, provider)
	}
	model, modelPresent := terminal["model_id"]
	if !modelPresent || model != nil {
		t.Fatalf("pre-model failure model_id must be present null, present=%v value=%v", modelPresent, model)
	}

	engine := s.engine.(*workflowTestEngine)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.lastMsg != nil {
		t.Fatalf("pre-model failure must not reach engine: %+v", engine.lastMsg.Metadata)
	}
}

func TestBUG20260726035ResumeKeepsPriorActualRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	s.workflowStore.workflows["wf-route-resume"] = &WorkflowData{
		ID:   "wf-route-resume",
		Name: "route resume",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{
				"provider": "changed-provider",
				"model":    "changed-model",
				"prompt":   "{{previous}}",
			}},
			map[string]any{"id": "output", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "output"},
		},
	}
	providerDisplayName := "HexClaw-GPT"
	modelID := "gpt-5.6-sol"
	s.workflowStore.runs["run-route-prior"] = &WorkflowRun{
		ID:                  "run-route-prior",
		WorkflowID:          "wf-route-resume",
		Status:              "failed",
		ProviderDisplayName: &providerDisplayName,
		ModelID:             &modelID,
		Input:               "hello",
		NodeResults: []WorkflowNodeRun{
			{NodeID: "input", Type: "input", Status: nodeStatusCompleted, Output: "hello"},
			{NodeID: "agent", Type: "agent", Status: nodeStatusFailed},
		},
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/canvas/runs/run-route-prior/resume",
		nil,
	)
	request.SetPathValue("id", "run-route-prior")
	response := httptest.NewRecorder()
	s.handleResumeWorkflowRun(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("resume run: status=%d body=%s", response.Code, response.Body.String())
	}

	var resumed WorkflowRun
	if err := json.Unmarshal(response.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resumed run: %v", err)
	}
	waitForRunCompletion(t, s, resumed.ID)

	terminal := getWorkflowRunJSONObject(t, s, resumed.ID)
	if got := terminal["status"]; got != "completed" {
		t.Fatalf("resumed terminal status=%v, want completed", got)
	}
	if got := terminal["provider_display_name"]; got != "HexClaw-GPT" {
		t.Fatalf("resumed provider_display_name=%v, want historical HexClaw-GPT", got)
	}
	if got := terminal["model_id"]; got != "gpt-5.6-sol" {
		t.Fatalf("resumed model_id=%v, want historical gpt-5.6-sol", got)
	}

	engine := s.engine.(*workflowTestEngine)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.lastMsg == nil || engine.lastMsg.Metadata["provider"] != "changed-provider" || engine.lastMsg.Metadata["model"] != "changed-model" {
		t.Fatalf("resumed engine route=%v, want current node route", engine.lastMsg)
	}
}

func TestBUG20260726035ProjectsEngineConfirmedRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	s.cfg.LLM.Default = "requested-provider"
	s.cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"requested-provider": {
			DisplayName: "Requested-Provider",
			Model:       "requested-model",
		},
		"actual-provider": {
			DisplayName: "Actual-Provider",
			Model:       "actual-model",
		},
	}
	engine := &mockEngine{reply: &adapter.Reply{
		Content: "ok",
		Metadata: map[string]string{
			"provider": "actual-provider",
			"model":    "actual-model",
		},
	}}
	s.engine = engine
	s.workflowStore.workflows["wf-route-engine-confirmed"] = &WorkflowData{
		ID:   "wf-route-engine-confirmed",
		Name: "route engine confirmed",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{
				"provider": "requested-provider",
				"model":    "requested-model",
				"prompt":   "{{previous}}",
			}},
			map[string]any{"id": "output", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "output"},
		},
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/canvas/workflows/wf-route-engine-confirmed/run",
		strings.NewReader(`{"input":"hello"}`),
	)
	request.SetPathValue("id", "wf-route-engine-confirmed")
	response := httptest.NewRecorder()
	s.handleRunWorkflow(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start run: status=%d body=%s", response.Code, response.Body.String())
	}

	var started WorkflowRun
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started run: %v", err)
	}
	waitForRunCompletion(t, s, started.ID)

	terminal := getWorkflowRunJSONObject(t, s, started.ID)
	if got := terminal["status"]; got != "completed" {
		t.Fatalf("terminal status=%v, want completed", got)
	}
	if got := terminal["provider_display_name"]; got != "Actual-Provider" {
		t.Fatalf("provider_display_name=%v, want engine-confirmed Actual-Provider", got)
	}
	if got := terminal["model_id"]; got != "actual-model" {
		t.Fatalf("model_id=%v, want engine-confirmed actual-model", got)
	}
	if engine.calls != 1 {
		t.Fatalf("engine calls=%d, want 1", engine.calls)
	}
}

func TestBUG20260726035FailedEngineCallKeepsBoundaryRouteFacts(t *testing.T) {
	s := newWorkflowTestServer()
	s.cfg.LLM.Default = "hexclaw-route"
	s.cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"hexclaw-route": {
			DisplayName: "HexClaw-GPT",
			Model:       "gpt-5.6-sol",
		},
	}
	engine := &mockEngine{err: errors.New("upstream unavailable")}
	s.engine = engine
	s.workflowStore.workflows["wf-route-engine-failure"] = &WorkflowData{
		ID:   "wf-route-engine-failure",
		Name: "route engine failure",
		Nodes: []any{
			map[string]any{"id": "input", "type": "input", "data": map[string]any{"value": "{{input}}"}},
			map[string]any{"id": "agent", "type": "agent", "data": map[string]any{
				"provider": "hexclaw-route",
				"model":    "gpt-5.6-sol",
				"prompt":   "{{previous}}",
			}},
			map[string]any{"id": "output", "type": "output"},
		},
		Edges: []any{
			map[string]any{"source": "input", "target": "agent"},
			map[string]any{"source": "agent", "target": "output"},
		},
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/v1/canvas/workflows/wf-route-engine-failure/run",
		strings.NewReader(`{"input":"hello"}`),
	)
	request.SetPathValue("id", "wf-route-engine-failure")
	response := httptest.NewRecorder()
	s.handleRunWorkflow(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start run: status=%d body=%s", response.Code, response.Body.String())
	}

	var started WorkflowRun
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started run: %v", err)
	}
	waitForRunCompletion(t, s, started.ID)

	terminal := getWorkflowRunJSONObject(t, s, started.ID)
	if got := terminal["status"]; got != "failed" {
		t.Fatalf("terminal status=%v, want failed", got)
	}
	if got := terminal["provider_display_name"]; got != "HexClaw-GPT" {
		t.Fatalf("provider_display_name=%v, want HexClaw-GPT", got)
	}
	if got := terminal["model_id"]; got != "gpt-5.6-sol" {
		t.Fatalf("model_id=%v, want gpt-5.6-sol", got)
	}
	if engine.calls != 1 {
		t.Fatalf("engine calls=%d, want 1", engine.calls)
	}
}

func getWorkflowRunJSONObject(t *testing.T, s *Server, runID string) map[string]any {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/canvas/runs/"+runID, nil)
	request.SetPathValue("id", runID)
	response := httptest.NewRecorder()
	s.handleGetWorkflowRun(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("get run: status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode terminal run: %v", err)
	}
	return payload
}
