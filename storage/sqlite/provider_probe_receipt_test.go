package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage"
)

func TestProviderProbeReceiptStartedAtFencesLateOlderCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "probe-receipt.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}

	newer := &storage.ProviderProbeReceipt{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		ConfigFingerprint:  "new-fingerprint",
		Outcome:            "passed",
		TestedAt:           300,
		ProbeStartedAt:     200,
		LatencyMS:          100,
		Locality:           "cloud",
		Message:            "newer probe",
	}
	persisted, err := store.SaveProviderProbeReceipt(ctx, newer)
	if err != nil || !persisted {
		t.Fatalf("save newer receipt persisted=%v err=%v", persisted, err)
	}

	older := *newer
	older.ConfigFingerprint = "old-fingerprint"
	older.Outcome = "failed"
	older.TestedAt = 400
	older.ProbeStartedAt = 100
	older.Message = "older probe completed late"
	persisted, err = store.SaveProviderProbeReceipt(ctx, &older)
	if err != nil {
		t.Fatalf("save late older receipt: %v", err)
	}
	if persisted {
		t.Fatal("late older receipt reported persisted=true")
	}

	got, err := store.GetProviderProbeReceipt(ctx, newer.ProviderInstanceID)
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if got == nil || got.ConfigFingerprint != newer.ConfigFingerprint ||
		got.Outcome != newer.Outcome || got.ProbeStartedAt != newer.ProbeStartedAt {
		t.Fatalf("receipt=%+v, want newer=%+v", got, newer)
	}
}
