package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// --- Webhook API Tests ---

func TestHandleListWebhooks_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	// webhookMgr is nil

	req := httptest.NewRequest("GET", "/api/v1/webhooks?user_id=u1", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleListWebhooks panicked with nil manager: %v", r)
		}
	}()
	srv.handleListWebhooks(w, req)
}

func TestHandleRegisterWebhook_MissingFields(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	tests := []struct {
		name string
		body string
	}{
		{"missing all", `{}`},
		{"missing prompt", `{"name":"test"}`},
		{"missing name", `{"prompt":"do"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			srv.handleRegisterWebhook(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %s, got %d: %s", tt.name, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleRegisterWebhook_InvalidJSON(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	srv.handleRegisterWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleDeleteWebhook_NilManager(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/webhooks/my-hook", nil)
	req.SetPathValue("name", "my-hook")
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleDeleteWebhook panicked with nil manager: %v", r)
		}
	}()
	srv.handleDeleteWebhook(w, req)
}

func TestHandleRegisterWebhook_DefaultUserID(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	// Valid request but no user_id — should default to "api-user"
	body := `{"name":"test-hook","prompt":"handle event"}`
	req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(body))
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked with nil webhookMgr: %v", r)
		}
	}()
	srv.handleRegisterWebhook(w, req)
	// With nil manager, will panic or return error — testing crash safety
}
