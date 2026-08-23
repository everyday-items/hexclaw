package k12_test

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUG20260724012CreativeEntryRejectsCompleteRevision(t *testing.T) {
	entry := k12.ImageTaskCreativeEntry{
		Kind:          k12.CreativeWorkEntryRevision,
		TaskIntent:    k12.ImageTaskIntentArtwork,
		WorkID:        "legacy-work-1",
		BaseVersionID: "v1",
	}
	if err := entry.Validate(); err == nil {
		t.Fatal("complete revision creative_entry was accepted; only new_work may be written")
	}
}
