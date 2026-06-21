package sqlite

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage"
)

// A soft-deleted session (status = -1) must be invisible to reads and forks,
// matching ListSessions which already filters status >= 0. Otherwise a deleted
// session stays fully readable via GetSession and can be resurrected via
// ForkSession.
func TestBug_DeletedSessionNotReadableOrForkable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateSession(ctx, &storage.Session{
		ID: "s-del", UserID: "u1", Platform: "web", Title: "t",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "m1", SessionID: "s-del", Role: "user", Content: "hi", Metadata: "{}",
	}); err != nil {
		t.Fatalf("save message: %v", err)
	}
	if err := store.DeleteSession(ctx, "s-del"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := store.GetSession(ctx, "s-del"); err != storage.ErrNotFound {
		t.Fatalf("GetSession on a deleted session must return ErrNotFound, got %v", err)
	}
	if _, err := store.ForkSession(ctx, "s-del", "m1", "u1"); err == nil {
		t.Fatalf("ForkSession on a deleted session must fail, got nil (resurrection)")
	}
}
