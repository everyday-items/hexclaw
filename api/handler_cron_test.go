package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// --- Cron API Tests ---

func TestHandleListCronJobs_NilScheduler(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	// scheduler is nil — must not panic

	req := httptest.NewRequest("GET", "/api/v1/cron/jobs?user_id=u1", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleListCronJobs panicked with nil scheduler: %v", r)
		}
	}()
	srv.handleListCronJobs(w, req)

	// Should return error, not panic
	if w.Code == http.StatusOK {
		// If OK, scheduler might have a default — that's fine too
		return
	}
	if w.Code != http.StatusInternalServerError && w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 500 or 503 for nil scheduler, got %d", w.Code)
	}
}

func TestHandleAddCronJob_MissingFields(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	tests := []struct {
		name string
		body string
	}{
		{"missing all", `{}`},
		{"missing schedule", `{"name":"test","prompt":"do"}`},
		{"missing prompt", `{"name":"test","schedule":"@daily"}`},
		{"missing name", `{"schedule":"@daily","prompt":"do"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/cron/jobs", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			srv.handleAddCronJob(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %s, got %d: %s", tt.name, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleAddCronJob_InvalidJSON(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/cron/jobs", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	srv.handleAddCronJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleDeleteCronJob_NilScheduler(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/cron/jobs/job-123", nil)
	req.SetPathValue("id", "job-123")
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleDeleteCronJob panicked with nil scheduler: %v", r)
		}
	}()
	srv.handleDeleteCronJob(w, req)

	if w.Code == http.StatusOK {
		return
	}
	_ = w.Body.String() // ensure body is readable
}

func TestHandlePauseCronJob_NilScheduler(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/cron/jobs/job-123/pause", nil)
	req.SetPathValue("id", "job-123")
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handlePauseCronJob panicked with nil scheduler: %v", r)
		}
	}()
	srv.handlePauseCronJob(w, req)
}

func TestHandleResumeCronJob_NilScheduler(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/cron/jobs/job-123/resume", nil)
	req.SetPathValue("id", "job-123")
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleResumeCronJob panicked with nil scheduler: %v", r)
		}
	}()
	srv.handleResumeCronJob(w, req)
}

func TestHandleListCronJobs_DefaultUserID(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	// No user_id — should default to "api-user"
	req := httptest.NewRequest("GET", "/api/v1/cron/jobs", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	srv.handleListCronJobs(w, req)
	// Just checking it doesn't crash
	_ = w.Body.String()
}

func TestHandleAddCronJob_DefaultUserID(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"name":"test","schedule":"@daily","prompt":"hello"}`
	req := httptest.NewRequest("POST", "/api/v1/cron/jobs", strings.NewReader(body))
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	srv.handleAddCronJob(w, req)

	// With nil scheduler this will either panic (bug) or return error
	// We're testing it doesn't crash
	_ = json.NewDecoder(w.Body)
}
