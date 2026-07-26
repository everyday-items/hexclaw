package sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage"
)

// Step 5.6 evidence:
//   - concurrency/linearizability: independent deletes contend on the same session aggregate;
//   - idempotency/replay: every delete is replayed and must remain a no-op after commit;
//   - deterministic simulation: a start barrier fixes the concurrent phase boundary.
func TestBUG20260726001_ConcurrentDeleteReplayLinearizesSessionStats(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()
	const (
		sessionID = "bug-20260726-001-step56"
		messageN  = 24
	)

	if err := store.CreateSession(ctx, &storage.Session{
		ID:     sessionID,
		UserID: "step56-user",
		Title:  "concurrent delete replay",
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	messageIDs := make([]string, 0, messageN)
	for i := 0; i < messageN; i++ {
		id := fmt.Sprintf("step56-message-%02d", i)
		messageIDs = append(messageIDs, id)
		if err := store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        id,
			SessionID: sessionID,
			Role:      "user",
			Content:   fmt.Sprintf("message-%02d", i),
			PromptTokens:     i + 1,
			CompletionTokens: messageN - i,
		}); err != nil {
			t.Fatalf("SaveMessage(%q) error = %v", id, err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, messageN)
	var wg sync.WaitGroup
	for _, id := range messageIDs {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := store.DeleteMessage(ctx, id); err != nil {
				errs <- fmt.Errorf("first DeleteMessage(%q): %w", id, err)
				return
			}
			if err := store.DeleteMessage(ctx, id); err != nil {
				errs <- fmt.Errorf("replayed DeleteMessage(%q): %w", id, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	session, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.MessageCount != 0 ||
		session.TotalPromptTokens != 0 ||
		session.TotalCompletionTokens != 0 ||
		session.LastMessagePreview != "" {
		t.Fatalf(
			"session stats after concurrent replay deletes = count:%d prompt:%d completion:%d preview:%q, want zero/zero/zero/empty",
			session.MessageCount,
			session.TotalPromptTokens,
			session.TotalCompletionTokens,
			session.LastMessagePreview,
		)
	}

	messages, err := store.ListMessages(ctx, sessionID, messageN+1, 0)
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("ListMessages() length = %d, want 0", len(messages))
	}
}
