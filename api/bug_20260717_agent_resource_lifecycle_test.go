package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type recordingAgentResourceCleaner struct {
	cleaned  []string
	restored []string
	err      error
}

func (c *recordingAgentResourceCleaner) DetachAgentResources(
	_ context.Context,
	agent agentrouter.AgentConfig,
) (func(context.Context) error, error) {
	c.cleaned = append(c.cleaned, agent.Name)
	if c.err != nil {
		return nil, c.err
	}
	return func(context.Context) error {
		c.restored = append(c.restored, agent.Name)
		return nil
	}, nil
}

func TestBug20260717_UnregisterAgentDetachesOwnedResources(t *testing.T) {
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:     "kid",
		Metadata: map[string]string{"scenario": "k12-tutor"},
	}); err != nil {
		t.Fatal(err)
	}
	cleaner := &recordingAgentResourceCleaner{}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentResourceCleaner(cleaner)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/kid", nil)
	req.SetPathValue("name", "kid")
	rec := httptest.NewRecorder()
	srv.handleUnregisterAgent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cleaner.cleaned) != 1 || cleaner.cleaned[0] != "kid" {
		t.Fatalf("owned resources were not detached: %+v", cleaner.cleaned)
	}
	if len(cleaner.restored) != 0 {
		t.Fatalf("successful deletion must not roll resources back: %+v", cleaner.restored)
	}
}

func TestBug20260717_UnregisterAgentCleanupFailureKeepsAgent(t *testing.T) {
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	cleaner := &recordingAgentResourceCleaner{err: errors.New("cron store unavailable")}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentResourceCleaner(cleaner)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/kid", nil)
	req.SetPathValue("name", "kid")
	rec := httptest.NewRecorder()
	srv.handleUnregisterAgent(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := dispatcher.GetAgent("kid"); !ok {
		t.Fatal("cleanup failure must keep the agent registered")
	}
}

func TestBug20260717_UnregisterPersistenceFailureRollsResourcesBack(t *testing.T) {
	store := &failingAgentStore{failDeleteAgnt: true}
	srv, dispatcher := newAgentPersistServer(t, store)
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	cleaner := &recordingAgentResourceCleaner{}
	srv.SetAgentResourceCleaner(cleaner)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/kid", nil)
	req.SetPathValue("name", "kid")
	rec := httptest.NewRecorder()
	srv.handleUnregisterAgent(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cleaner.cleaned) != 1 || len(cleaner.restored) != 1 {
		t.Fatalf("cleanup saga did not compensate: cleaned=%v restored=%v", cleaner.cleaned, cleaner.restored)
	}
	if _, ok := dispatcher.GetAgent("kid"); !ok {
		t.Fatal("persistence failure must keep the agent registered")
	}
}
