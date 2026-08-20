package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func newReasoningPolicyAgentServer(t *testing.T) (*Server, *agentrouter.Dispatcher, *agentrouter.SQLiteStore) {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatalf("create SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init SQLite store: %v", err)
	}
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("init Agent store: %v", err)
	}
	dispatcher := agentrouter.New()
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)
	return srv, dispatcher, agentStore
}

func decodeReasoningPolicyAgents(t *testing.T, body []byte) []agentrouter.AgentConfig {
	t.Helper()
	var response struct {
		Agents []agentrouter.AgentConfig `json:"agents"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode agents: %v", err)
	}
	return response.Agents
}

func TestAgentReasoningPolicyCreateUpdateListAndReloadRoundTrip(t *testing.T) {
	srv, dispatcher, agentStore := newReasoningPolicyAgentServer(t)

	create := registerAgentReq(t, srv, `{"name":"planner","reasoning_policy":{"mode":"effort","effort":"high"}}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	created, ok := dispatcher.GetAgent("planner")
	if !ok || created.ReasoningPolicy == nil ||
		created.ReasoningPolicy.Mode != config.ReasoningPolicyModeEffort ||
		created.ReasoningPolicy.Effort != config.ReasoningEffortHigh {
		t.Fatalf("created policy = %+v", created)
	}

	list := httptest.NewRecorder()
	srv.handleListAgents(list, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	agents := decodeReasoningPolicyAgents(t, list.Body.Bytes())
	if len(agents) != 1 || agents[0].ReasoningPolicy == nil ||
		*agents[0].ReasoningPolicy != *created.ReasoningPolicy {
		t.Fatalf("list policy = %+v, want %+v", agents, created.ReasoningPolicy)
	}

	update := updateAgentReq(t, srv, "planner", `{"reasoning_policy":{"mode":"off"}}`)
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
	updated, _ := dispatcher.GetAgent("planner")
	if updated.ReasoningPolicy == nil || updated.ReasoningPolicy.Mode != config.ReasoningPolicyModeOff ||
		updated.ReasoningPolicy.Effort != "" {
		t.Fatalf("updated policy = %+v", updated.ReasoningPolicy)
	}
	unchanged := updateAgentReq(t, srv, "planner", `{"display_name":"Planner 2"}`)
	if unchanged.Code != http.StatusOK {
		t.Fatalf("unrelated update status=%d body=%s", unchanged.Code, unchanged.Body.String())
	}
	updated, _ = dispatcher.GetAgent("planner")
	if updated.ReasoningPolicy == nil || updated.ReasoningPolicy.Mode != config.ReasoningPolicyModeOff {
		t.Fatalf("omitted policy was not preserved: %+v", updated.ReasoningPolicy)
	}

	persisted, defaultName, err := agentStore.LoadAgents(context.Background())
	if err != nil {
		t.Fatalf("reload agents: %v", err)
	}
	reloadedDispatcher := agentrouter.New()
	reloadedDispatcher.LoadAll(persisted, defaultName, nil)
	reloadedServer := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	reloadedServer.SetAgentRouter(reloadedDispatcher)
	reloadedList := httptest.NewRecorder()
	reloadedServer.handleListAgents(reloadedList, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	agents = decodeReasoningPolicyAgents(t, reloadedList.Body.Bytes())
	if len(agents) != 1 || agents[0].ReasoningPolicy == nil ||
		agents[0].ReasoningPolicy.Mode != config.ReasoningPolicyModeOff || agents[0].ReasoningPolicy.Effort != "" {
		t.Fatalf("reloaded list policy = %+v", agents)
	}
}

func TestAgentReasoningPolicyMissingDefaultsToInherit(t *testing.T) {
	srv, dispatcher, _ := newReasoningPolicyAgentServer(t)
	w := registerAgentReq(t, srv, `{"name":"legacy"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := dispatcher.GetAgent("legacy")
	if got.ReasoningPolicy == nil || got.ReasoningPolicy.Mode != config.ReasoningPolicyModeInherit ||
		got.ReasoningPolicy.Effort != "" {
		t.Fatalf("missing policy = %+v, want inherit", got.ReasoningPolicy)
	}
}

func TestAgentReasoningPolicyRejectsInvalidCombinations(t *testing.T) {
	tests := []string{
		`{"name":"invalid","reasoning_policy":{"mode":"effort"}}`,
		`{"name":"invalid","reasoning_policy":{"mode":"off","effort":"high"}}`,
		`{"name":"invalid","reasoning_policy":{"mode":"effort","effort":"extreme"}}`,
		`{"name":"invalid","reasoning_policy":{"mode":"sometimes"}}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			srv, dispatcher, _ := newReasoningPolicyAgentServer(t)
			w := registerAgentReq(t, srv, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid create status=%d body=%s", w.Code, w.Body.String())
			}
			if _, ok := dispatcher.GetAgent("invalid"); ok {
				t.Fatal("invalid policy registered Agent")
			}
		})
	}

	srv, dispatcher, _ := newReasoningPolicyAgentServer(t)
	if w := registerAgentReq(t, srv, `{"name":"stable","reasoning_policy":{"mode":"on"}}`); w.Code != http.StatusOK {
		t.Fatalf("seed create status=%d body=%s", w.Code, w.Body.String())
	}
	w := updateAgentReq(t, srv, "stable", `{"reasoning_policy":{"mode":"on","effort":"high"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := dispatcher.GetAgent("stable")
	if got.ReasoningPolicy == nil || got.ReasoningPolicy.Mode != config.ReasoningPolicyModeOn || got.ReasoningPolicy.Effort != "" {
		t.Fatalf("invalid update mutated policy: %+v", got.ReasoningPolicy)
	}
}
