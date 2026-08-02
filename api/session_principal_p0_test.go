package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
)

func authenticatedSessionRequest(method, target, body, ownerID string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	return req.WithContext(skill.WithAuthenticatedUser(context.Background(), ownerID))
}

func TestSessionPrincipalP0_CreateIgnoresForgedQueryAndBodyOwner(t *testing.T) {
	store := newTestStoreForAPI(t)
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, store)
	req := authenticatedSessionRequest(
		http.MethodPost,
		"/api/v1/sessions?user_id=query-attacker",
		`{"id":"principal-owned","title":"secure","user_id":"body-attacker"}`,
		"authenticated-owner",
	)
	w := httptest.NewRecorder()

	srv.handleCreateSession(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	session, err := store.GetSession(context.Background(), "principal-owned")
	if err != nil {
		t.Fatal(err)
	}
	if session.UserID != "authenticated-owner" {
		t.Fatalf("session owner=%q, want authenticated principal", session.UserID)
	}
}

func TestSessionPrincipalP0_ForkIgnoresForgedBodyOwner(t *testing.T) {
	store := newTestStoreForAPI(t)
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, store)
	if err := store.CreateSession(context.Background(), newSessionForPrincipalTest("fork-source", "authenticated-owner")); err != nil {
		t.Fatal(err)
	}
	req := authenticatedSessionRequest(
		http.MethodPost,
		"/api/v1/sessions/fork-source/fork",
		`{"message_id":"missing-message","user_id":"body-attacker"}`,
		"authenticated-owner",
	)
	req.SetPathValue("id", "fork-source")
	w := httptest.NewRecorder()

	srv.handleForkSession(w, req)

	// The missing message may make the operation fail, but ownership must pass;
	// a spoofed body owner used by the old implementation failed earlier as 404.
	if w.Code == http.StatusNotFound {
		t.Fatalf("authenticated owner was replaced by forged body user: %s", w.Body.String())
	}
}

func newSessionForPrincipalTest(id, owner string) *storage.Session {
	return &storage.Session{ID: id, UserID: owner, Platform: "web", Title: "principal"}
}
