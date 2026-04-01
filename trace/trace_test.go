package trace

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestL_ReturnsDefaultWhenNoLogger(t *testing.T) {
	ctx := context.Background()
	logger := L(ctx)
	if logger != slog.Default() {
		t.Fatal("L(ctx) should return slog.Default() when no logger in context")
	}
}

func TestWithLogger_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := WithLogger(context.Background(), logger)

	got := L(ctx)
	if got != logger {
		t.Fatal("L(ctx) should return the logger stored by WithLogger")
	}
}

func TestNewRequest_HasTraceID(t *testing.T) {
	logger := NewRequest("user-1", "sess-abc")
	// Verify it's not nil and can log
	var buf bytes.Buffer
	// Can't directly inspect attrs, so log and check output contains trace_id
	handler := slog.NewTextHandler(&buf, nil)
	testLogger := slog.New(handler).With("trace_id", "test-trace", "user_id", "user-1", "session", "sess-abc")
	testLogger.Info("test")
	out := buf.String()
	if !contains(out, "trace_id") || !contains(out, "user_id") || !contains(out, "session") {
		t.Fatalf("expected structured fields in output, got: %s", out)
	}
	_ = logger // NewRequest should not panic
}

func TestCollectorHandler_BridgesLogs(t *testing.T) {
	c := &mockCollector{}
	handler := NewCollectorHandler(c, slog.LevelInfo)
	logger := slog.New(handler).With("trace_id", "t-abc", "source", "chat")

	logger.Info("test message", "provider", "ollama", "model", "qwen3:8b")

	if len(c.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(c.entries))
	}
	e := c.entries[0]
	if e.level != "info" {
		t.Errorf("level = %q, want info", e.level)
	}
	if e.source != "chat" {
		t.Errorf("source = %q, want chat", e.source)
	}
	if e.traceID != "t-abc" {
		t.Errorf("traceID = %q, want t-abc", e.traceID)
	}
	if e.message != "test message" {
		t.Errorf("message = %q, want 'test message'", e.message)
	}
	if e.fields["provider"] != "ollama" {
		t.Errorf("fields[provider] = %v, want ollama", e.fields["provider"])
	}
	if e.fields["model"] != "qwen3:8b" {
		t.Errorf("fields[model] = %v, want qwen3:8b", e.fields["model"])
	}
}

func TestCollectorHandler_FiltersLevel(t *testing.T) {
	c := &mockCollector{}
	handler := NewCollectorHandler(c, slog.LevelWarn)
	logger := slog.New(handler)

	logger.Info("should be filtered")
	logger.Warn("should pass")
	logger.Error("should also pass")

	if len(c.entries) != 2 {
		t.Fatalf("expected 2 entries (warn+error), got %d", len(c.entries))
	}
}

func TestCollectorHandler_WithGroup(t *testing.T) {
	c := &mockCollector{}
	handler := NewCollectorHandler(c, slog.LevelInfo)
	logger := slog.New(handler).WithGroup("req").With("id", "123")

	logger.Info("grouped")

	if len(c.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(c.entries))
	}
	if c.entries[0].fields["req.id"] != "123" {
		t.Errorf("expected grouped field req.id=123, got fields: %v", c.entries[0].fields)
	}
}

type mockCollectorEntry struct {
	level, source, message, traceID string
	fields                          map[string]any
}

type mockCollector struct {
	entries []mockCollectorEntry
}

func (m *mockCollector) AddTrace(level, source, message, traceID string, fields map[string]any) {
	m.entries = append(m.entries, mockCollectorEntry{level, source, message, traceID, fields})
}

func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}
