package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// BUG-20260726-001: deleting messages used to remove only the message rows.
// The denormalized session count/tokens/preview kept their historical values,
// so the sidebar could show 53 while the conversation API returned 8.
func TestBUG20260726001_DeleteMessageReconcilesSessionStatsAtomically(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()
	const sessionID = "session-delete-stats"
	if err := store.CreateSession(ctx, &storage.Session{
		ID: sessionID, UserID: "user-1", Platform: "desktop", Title: "统计回归",
	}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []*storage.MessageRecord{
		{ID: "m1", SessionID: sessionID, Role: "user", Content: "第一条", Metadata: "{}", PromptTokens: 1, CompletionTokens: 2},
		{ID: "m2", SessionID: sessionID, Role: "assistant", Content: "第二条", Metadata: "{}", PromptTokens: 3, CompletionTokens: 4},
		{ID: "m3", SessionID: sessionID, Role: "user", Content: "第三条", Metadata: "{}", PromptTokens: 5, CompletionTokens: 6},
	} {
		if err := store.SaveMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}

	assertStats := func(wantCount, wantPrompt, wantCompletion int, wantPreview string) {
		t.Helper()
		session, err := store.GetSession(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.MessageCount != wantCount ||
			session.TotalPromptTokens != wantPrompt ||
			session.TotalCompletionTokens != wantCompletion ||
			session.LastMessagePreview != wantPreview {
			t.Fatalf(
				"session stats = count:%d prompt:%d completion:%d preview:%q, want %d/%d/%d/%q",
				session.MessageCount,
				session.TotalPromptTokens,
				session.TotalCompletionTokens,
				session.LastMessagePreview,
				wantCount,
				wantPrompt,
				wantCompletion,
				wantPreview,
			)
		}
	}

	assertStats(3, 9, 12, "第三条")
	if err := store.DeleteMessage(ctx, "m3"); err != nil {
		t.Fatal(err)
	}
	assertStats(2, 4, 6, "第二条")
	if err := store.DeleteMessage(ctx, "m2"); err != nil {
		t.Fatal(err)
	}
	assertStats(1, 1, 2, "第一条")
	if err := store.DeleteMessage(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	assertStats(0, 0, 0, "")
}

func TestBUG20260726001_V43RepairsHistoricalSessionStatsOnUpgrade(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "historical-drift.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Build a genuine V42 database. Deleting migration ledger rows from a V57
	// schema would leave later physical columns in place and replay V47 against
	// an impossible database state instead of exercising the V43 repair.
	if err := migrate.Run(ctx, store.DB(), migrate.All[:42]); err != nil {
		t.Fatal(err)
	}
	const sessionID = "historical-drift"
	if err := store.CreateSession(ctx, &storage.Session{
		ID: sessionID, UserID: "user-1", Platform: "desktop", Title: "历史漂移",
	}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []*storage.MessageRecord{
		{ID: "old-1", SessionID: sessionID, Role: "user", Content: "旧一", Metadata: "{}", PromptTokens: 2, CompletionTokens: 3},
		{ID: "old-2", SessionID: sessionID, Role: "assistant", Content: "旧二", Metadata: "{}", PromptTokens: 5, CompletionTokens: 7},
	} {
		if err := store.SaveMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `
		UPDATE sessions
		SET message_count=53,
			total_prompt_tokens=999,
			total_completion_tokens=888,
			last_message_preview='已不存在的历史消息'
		WHERE id=?`, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Init(ctx); err != nil {
		t.Fatal(err)
	}
	session, err := reopened.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.MessageCount != 2 ||
		session.TotalPromptTokens != 7 ||
		session.TotalCompletionTokens != 10 ||
		session.LastMessagePreview != "旧二" {
		t.Fatalf(
			"V43未修复历史统计: count=%d prompt=%d completion=%d preview=%q",
			session.MessageCount,
			session.TotalPromptTokens,
			session.TotalCompletionTokens,
			session.LastMessagePreview,
		)
	}
}
