package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type auditDeleteRuleFailStore struct{ failingAgentStore }

func (*auditDeleteRuleFailStore) DeleteRule(context.Context, int) error {
	return errAgentStoreDown
}

func TestAudit20260711UnregisterFailureDoesNotPublishMemoryDeletion(t *testing.T) {
	srv, dispatcher := newAgentPersistServer(t, &failingAgentStore{failDeleteAgnt: true})
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "assistant"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/assistant", nil)
	req.SetPathValue("name", "assistant")
	w := httptest.NewRecorder()

	srv.handleUnregisterAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("persist failure status=%d, want 500", w.Code)
	}
	if _, ok := dispatcher.GetAgent("assistant"); !ok {
		t.Error("failed persistence still published the in-memory deletion")
	}
}

func TestAudit20260711SetDefaultFailureDoesNotPublishMemoryChange(t *testing.T) {
	srv, dispatcher := newAgentPersistServer(t, &failingAgentStore{failSetDefault: true})
	for _, name := range []string{"alpha", "beta"} {
		if err := dispatcher.Register(agentrouter.AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/default", strings.NewReader(`{"name":"beta"}`))
	w := httptest.NewRecorder()

	srv.handleSetDefaultAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("persist failure status=%d, want 500", w.Code)
	}
	if got := dispatcher.DefaultAgent(); got != "alpha" {
		t.Errorf("failed persistence published default=%q, want previous default alpha", got)
	}
}

func TestAudit20260711DeleteRuleFailureReturns500AndKeepsMemoryRule(t *testing.T) {
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.AddRule(agentrouter.Rule{ID: 9, Platform: "web", AgentName: "assistant"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(&auditDeleteRuleFailStore{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/rules/9", nil)
	req.SetPathValue("id", "9")
	w := httptest.NewRecorder()

	srv.handleDeleteRule(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("DeleteRule persistence failure status=%d, want 500", w.Code)
	}
	if rules := dispatcher.ListRules(); len(rules) != 1 || rules[0].ID != 9 {
		t.Errorf("DeleteRule persistence failure still removed memory rule: %+v", rules)
	}
}
