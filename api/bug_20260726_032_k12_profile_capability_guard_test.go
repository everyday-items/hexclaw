package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type bug20260726032AgentStore struct {
	failingAgentStore
	saveCalls int
}

func (s *bug20260726032AgentStore) SaveAgent(
	_ context.Context,
	_ *agentrouter.AgentConfig,
) error {
	s.saveCalls++
	return nil
}

func newBug20260726032AgentServer(
	t *testing.T,
) (*Server, *agentrouter.Dispatcher, *bug20260726032AgentStore) {
	t.Helper()
	dispatcher := agentrouter.New()
	store := &bug20260726032AgentStore{}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(store)
	srv.SetAgentMetadataGuard(func(metadata map[string]string) error {
		return k12.ValidateProfileGradeTerm(metadata[k12.MetaKeyGradeTerm])
	})
	return srv, dispatcher, store
}

func bug20260726032AgentRequest(
	t *testing.T,
	srv *Server,
	method string,
	name string,
	gradeTerm string,
) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name,
		"metadata": map[string]string{
			"scenario":       "k12-tutor",
			"k12.grade_term": gradeTerm,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/api/v1/agents/"+name, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	if method == http.MethodPost {
		srv.handleRegisterAgent(rec, req)
	} else {
		req.SetPathValue("name", name)
		srv.handleUpdateAgent(rec, req)
	}
	return rec
}

func TestBUG20260726032_GenericAgentCreateAcceptsEveryPrimaryGradeTerm(t *testing.T) {
	primary := []string{
		"一年级上", "一年级下", "二年级上", "二年级下", "三年级上", "三年级下",
		"四年级上", "四年级下", "五年级上", "五年级下", "六年级上", "六年级下",
	}
	for index, gradeTerm := range primary {
		t.Run(gradeTerm, func(t *testing.T) {
			srv, dispatcher, store := newBug20260726032AgentServer(t)
			name := fmt.Sprintf("primary-%d", index)
			if rec := bug20260726032AgentRequest(
				t, srv, http.MethodPost, name, gradeTerm,
			); rec.Code != http.StatusOK {
				t.Fatalf("create %s status=%d body=%s", gradeTerm, rec.Code, rec.Body.String())
			}
			if store.saveCalls != 1 {
				t.Fatalf("create %s saveCalls=%d want 1", gradeTerm, store.saveCalls)
			}
			got, ok := dispatcher.GetAgent(name)
			if !ok || got.Metadata["k12.grade_term"] != gradeTerm {
				t.Fatalf("persisted grade=%q ok=%v want %q",
					got.Metadata["k12.grade_term"], ok, gradeTerm)
			}
		})
	}
}

func TestBUG20260726032_GenericAgentCreateRejectsFutureGradesWithoutSideEffects(
	t *testing.T,
) {
	unsupported := []string{
		"初一", "初二", "初三", "高一", "高二", "高三",
	}
	for index, gradeTerm := range unsupported {
		t.Run("create_"+gradeTerm, func(t *testing.T) {
			srv, dispatcher, store := newBug20260726032AgentServer(t)
			name := fmt.Sprintf("future-create-%d", index)
			rec := bug20260726032AgentRequest(t, srv, http.MethodPost, name, gradeTerm)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create %s status=%d want 400 body=%s",
					gradeTerm, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "须为小学 12 档：一年级上～六年级下") {
				t.Fatalf("create %s diagnostic unstable: %s", gradeTerm, rec.Body.String())
			}
			if store.saveCalls != 0 {
				t.Fatalf("create %s called persistence %d times", gradeTerm, store.saveCalls)
			}
			if _, ok := dispatcher.GetAgent(name); ok {
				t.Fatalf("create %s left an in-memory agent", gradeTerm)
			}
		})
	}
}
