package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestSaveAssistantReply_ReplySafeMetadataSurvivesReload(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "u-reply-meta",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := `{"collection":"mistake","status":"reviewing"}`
	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "已加入错题本", AssistantMeta{
		ReplyMetadata: map[string]string{
			"record":       record,
			"routed_agent": "internal-agent-must-not-leak",
			"secret":       "must-not-persist",
		},
	}); err != nil {
		t.Fatalf("SaveAssistantReply: %v", err)
	}

	messages, err := store.ListMessages(ctx, sess.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		if err := json.Unmarshal([]byte(msg.Metadata), &persisted); err != nil {
			t.Fatalf("decode persisted metadata: %v (%q)", err, msg.Metadata)
		}
	}
	if got, _ := persisted["record"].(string); got != record {
		t.Fatalf("reply-safe record metadata did not survive save/reload: got %q want %q", got, record)
	}
	if _, ok := persisted["routed_agent"]; ok {
		t.Fatal("non-whitelisted routed_agent must not persist in assistant reply metadata")
	}
	if _, ok := persisted["secret"]; ok {
		t.Fatal("unknown metadata must not persist in assistant reply metadata")
	}
}
