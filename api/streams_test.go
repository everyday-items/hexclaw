package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/streamstate"
)

type stubStreamStateProvider struct {
	items map[string]streamstate.Snapshot
}

func (s *stubStreamStateProvider) ListActiveStreams(userID string) []streamstate.Snapshot {
	out := make([]streamstate.Snapshot, 0)
	for _, item := range s.items {
		if item.UserID == userID && !item.Done {
			out = append(out, item)
		}
	}
	return out
}

func (s *stubStreamStateProvider) GetStreamSnapshot(userID, requestID string) (*streamstate.Snapshot, bool) {
	item, ok := s.items[requestID]
	if !ok || item.UserID != userID {
		return nil, false
	}
	copy := item
	return &copy, true
}

func TestServer_ListActiveStreams(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetStreamStateProvider(&stubStreamStateProvider{items: map[string]streamstate.Snapshot{
		"req-active-1": {
			RequestID: "req-active-1",
			SessionID: "sess-active-1",
			UserID:    "desktop-user",
			Content:   "partial",
			Reasoning: "thinking",
			Done:      false,
			Status:    streamstate.StatusStreaming,
			UpdatedAt: time.Now(),
		},
		"req-done-1": {
			RequestID: "req-done-1",
			SessionID: "sess-done-1",
			UserID:    "desktop-user",
			Content:   "done",
			Done:      true,
			Status:    streamstate.StatusCompleted,
			UpdatedAt: time.Now(),
		},
		"req-other-user": {
			RequestID: "req-other-user",
			SessionID: "sess-other-user",
			UserID:    "someone-else",
			Content:   "other",
			Done:      false,
			Status:    streamstate.StatusStreaming,
			UpdatedAt: time.Now(),
		},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/active?user_id=desktop-user", nil)
	w := httptest.NewRecorder()

	srv.handleListActiveStreams(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Streams []streamstate.Snapshot `json:"streams"`
		Total   int                    `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("total = %d, want 1", resp.Total)
	}
	if len(resp.Streams) != 1 {
		t.Fatalf("streams len = %d, want 1", len(resp.Streams))
	}
	if resp.Streams[0].RequestID != "req-active-1" || resp.Streams[0].SessionID != "sess-active-1" {
		t.Fatalf("unexpected active stream payload: %+v", resp.Streams[0])
	}
}

func TestServer_ListActiveStreams_WithoutProviderReturnsEmptyList(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/active?user_id=desktop-user", nil)
	w := httptest.NewRecorder()

	srv.handleListActiveStreams(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Streams []streamstate.Snapshot `json:"streams"`
		Total   int                    `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp.Total != 0 || len(resp.Streams) != 0 {
		t.Fatalf("expected empty stream list, got total=%d len=%d", resp.Total, len(resp.Streams))
	}
}

func TestServer_GetStreamSnapshot(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetStreamStateProvider(&stubStreamStateProvider{items: map[string]streamstate.Snapshot{
		"req-resume-42": {
			RequestID: "req-resume-42",
			SessionID: "sess-resume-42",
			UserID:    "desktop-user",
			Content:   "partial answer",
			Reasoning: "partial reasoning",
			Done:      false,
			Status:    streamstate.StatusStreaming,
			Usage: &adapter.Usage{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
			UpdatedAt: time.Now(),
		},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/req-resume-42?user_id=desktop-user", nil)
	req.SetPathValue("request_id", "req-resume-42")
	w := httptest.NewRecorder()

	srv.handleGetStreamSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snapshot streamstate.Snapshot
	if err := json.NewDecoder(w.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode snapshot failed: %v", err)
	}
	if snapshot.RequestID != "req-resume-42" || snapshot.SessionID != "sess-resume-42" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Content != "partial answer" || snapshot.Reasoning != "partial reasoning" {
		t.Fatalf("snapshot content mismatch: %+v", snapshot)
	}
}

func TestServer_GetStreamSnapshot_MissingRequestID(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetStreamStateProvider(&stubStreamStateProvider{items: map[string]streamstate.Snapshot{}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/?user_id=desktop-user", nil)
	w := httptest.NewRecorder()

	srv.handleGetStreamSnapshot(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_GetStreamSnapshot_RejectsCrossUserRead(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetStreamStateProvider(&stubStreamStateProvider{items: map[string]streamstate.Snapshot{
		"req-secret": {
			RequestID: "req-secret",
			SessionID: "sess-secret",
			UserID:    "owner-user",
			Content:   "private partial",
			Done:      false,
			Status:    streamstate.StatusStreaming,
			UpdatedAt: time.Now(),
		},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/req-secret?user_id=other-user", nil)
	req.SetPathValue("request_id", "req-secret")
	w := httptest.NewRecorder()

	srv.handleGetStreamSnapshot(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user stream read should return 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_GetStreamSnapshot_WithoutProviderReturnsNotFound(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/req-missing?user_id=desktop-user", nil)
	req.SetPathValue("request_id", "req-missing")
	w := httptest.NewRecorder()

	srv.handleGetStreamSnapshot(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
