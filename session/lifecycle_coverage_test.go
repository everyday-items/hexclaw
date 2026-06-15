package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// Covers the previously-0% session lifecycle paths flagged by the audit:
// Compactor.Compact / generateSummary / CompactIfNeeded, the file Archiver, and
// Manager.CleanupOldSessions.

func seedMessages(store *mockStore, sessionID string, n int) {
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_ = store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        fmt.Sprintf("%s-msg-%d", sessionID, i),
			SessionID: sessionID,
			Role:      "user",
			Content:   fmt.Sprintf("message %d", i),
			CreatedAt: time.Now(),
		})
	}
}

func TestCompactor_Compact_SummarizesOldAndKeepsRecent(t *testing.T) {
	store := newMockStore()
	seedMessages(store, "sess-1", 60)
	c := NewCompactor(store, DefaultCompactionConfig()) // Max 50, Keep 10

	n, err := c.Compact(context.Background(), "sess-1", &titleSuggestProvider{response: "the summary"})
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if n != 50 {
		t.Fatalf("compacted count = %d, want 50 (60 - keepRecent 10)", n)
	}

	remaining := store.messages["sess-1"]
	if len(remaining) != 11 {
		t.Fatalf("remaining messages = %d, want 11 (10 recent + 1 summary)", len(remaining))
	}
	var summaries int
	for _, m := range remaining {
		if m.Role == "system" && strings.Contains(m.Content, "[上下文摘要] the summary") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("expected exactly one summary message carrying the LLM output, got %d", summaries)
	}
}

func TestCompactor_Compact_ProviderErrorPropagates(t *testing.T) {
	store := newMockStore()
	seedMessages(store, "sess-2", 60)
	c := NewCompactor(store, DefaultCompactionConfig())

	_, err := c.Compact(context.Background(), "sess-2", &titleSuggestProvider{err: errors.New("llm down")})
	if err == nil || !strings.Contains(err.Error(), "生成摘要失败") {
		t.Fatalf("provider failure must surface as a summary error, got: %v", err)
	}
	// Original messages must be untouched when summarization fails.
	if len(store.messages["sess-2"]) != 60 {
		t.Fatalf("messages must be preserved on summary failure, got %d", len(store.messages["sess-2"]))
	}
}

func TestCompactor_Compact_BelowThresholdIsNoOp(t *testing.T) {
	store := newMockStore()
	seedMessages(store, "sess-3", 30) // below the 50 threshold
	c := NewCompactor(store, DefaultCompactionConfig())

	n, err := c.Compact(context.Background(), "sess-3", &titleSuggestProvider{response: "x"})
	if err != nil || n != 0 {
		t.Fatalf("below-threshold Compact = (%d, %v), want (0, nil)", n, err)
	}
	if len(store.messages["sess-3"]) != 30 {
		t.Fatalf("no-op compact must leave all 30 messages, got %d", len(store.messages["sess-3"]))
	}
}

func TestCompactor_Compact_SkipsWhenAlreadyCompacting(t *testing.T) {
	store := newMockStore()
	seedMessages(store, "sess-4", 60)
	c := NewCompactor(store, DefaultCompactionConfig())

	// Simulate a concurrent compaction already in flight for this session.
	c.compacting.Store("sess-4", true)

	n, err := c.Compact(context.Background(), "sess-4", &titleSuggestProvider{response: "x"})
	if err != nil || n != 0 {
		t.Fatalf("re-entrant Compact = (%d, %v), want (0, nil)", n, err)
	}
	if len(store.messages["sess-4"]) != 60 {
		t.Fatalf("guarded compact must not mutate messages, got %d", len(store.messages["sess-4"]))
	}
}

func TestCompactor_CompactIfNeeded(t *testing.T) {
	ctx := context.Background()
	prov := &titleSuggestProvider{response: "summary"}

	// Below threshold: no compaction, no error.
	below := newMockStore()
	seedMessages(below, "s", 5)
	if err := NewCompactor(below, DefaultCompactionConfig()).CompactIfNeeded(ctx, "s", prov); err != nil {
		t.Fatalf("CompactIfNeeded below threshold errored: %v", err)
	}
	if len(below.messages["s"]) != 5 {
		t.Fatalf("below-threshold CompactIfNeeded must be a no-op, got %d", len(below.messages["s"]))
	}

	// Above threshold: compaction runs and shrinks the message set.
	above := newMockStore()
	seedMessages(above, "s", 60)
	if err := NewCompactor(above, DefaultCompactionConfig()).CompactIfNeeded(ctx, "s", prov); err != nil {
		t.Fatalf("CompactIfNeeded above threshold errored: %v", err)
	}
	if len(above.messages["s"]) != 11 {
		t.Fatalf("above-threshold CompactIfNeeded must compact to 11, got %d", len(above.messages["s"]))
	}
}

func TestArchiver_Archive_WritesTranscript(t *testing.T) {
	dir := t.TempDir()
	a := NewArchiver(dir)
	msgs := []*storage.MessageRecord{
		{ID: "m1", Role: "user", Content: "hello", CreatedAt: time.Now()},
		{ID: "m2", Role: "assistant", Content: "hi back", CreatedAt: time.Now()},
	}

	path, err := a.Archive("sess-archive", msgs)
	if err != nil {
		t.Fatalf("Archive failed: %v", err)
	}
	if path == "" {
		t.Fatal("Archive returned an empty path for a non-empty transcript")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("archive file unreadable: %v", err)
	}
	content := string(data)
	for _, want := range []string{"Session Transcript: sess-archive", "hello", "hi back"} {
		if !strings.Contains(content, want) {
			t.Errorf("archive transcript missing %q\n--- content ---\n%s", want, content)
		}
	}
}

func TestArchiver_Archive_EmptyMessagesNoFile(t *testing.T) {
	a := NewArchiver(t.TempDir())
	path, err := a.Archive("sess-empty", nil)
	if err != nil {
		t.Fatalf("Archive of empty transcript errored: %v", err)
	}
	if path != "" {
		t.Fatalf("empty transcript must not write a file, got path %q", path)
	}
}

func TestManager_CleanupOldSessions_DelegatesToStore(t *testing.T) {
	m, _ := newTestManager(t)
	// No sessions seeded, so cleanup is a no-op but must complete without error.
	n, err := m.CleanupOldSessions(context.Background(), 30)
	if err != nil {
		t.Fatalf("CleanupOldSessions errored: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 sessions cleaned on an empty store, got %d", n)
	}
}
