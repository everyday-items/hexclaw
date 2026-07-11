package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type blockingRegisterStore struct {
	failingAgentStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockingRegisterStore) SaveAgent(context.Context, *agentrouter.AgentConfig) error {
	close(s.entered)
	<-s.release
	return nil
}

func TestHandleRegisterAgent_SerializesWithPersistedRestore(t *testing.T) {
	dispatcher := agentrouter.New()
	store := &blockingRegisterStore{entered: make(chan struct{}), release: make(chan struct{})}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name":"new-child"}`))
	w := httptest.NewRecorder()
	registerDone := make(chan struct{})
	go func() {
		srv.handleRegisterAgent(w, req)
		close(registerDone)
	}()
	<-store.entered

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- dispatcher.UpdateAgentPersisted("new-child", func(current agentrouter.AgentConfig) (agentrouter.AgentConfig, error) {
			current.Metadata = map[string]string{"k12.grade_term": "六年级上"}
			return current, nil
		}, func(*agentrouter.AgentConfig) error { return nil })
	}()
	select {
	case err := <-restoreDone:
		t.Fatalf("restore escaped in-flight register persistence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(store.release)
	select {
	case <-registerDone:
	case <-time.After(time.Second):
		t.Fatal("registration remained blocked")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", w.Code, w.Body.String())
	}
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}
	cfg, ok := dispatcher.GetAgent("new-child")
	if !ok || cfg.Metadata["k12.grade_term"] != "六年级上" {
		t.Fatalf("restore lost after concurrent registration: %+v ok=%v", cfg, ok)
	}
}
