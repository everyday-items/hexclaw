package storage

import "context"

// ProviderProbeReceipt is the latest explicit connectivity probe fact for one
// stable LLM provider instance. ConfigFingerprint is an irreversible digest;
// raw provider credentials are never stored in this record.
type ProviderProbeReceipt struct {
	ProviderInstanceID string
	ConfigFingerprint  string
	Outcome            string
	TestedAt           int64
	ProbeStartedAt     int64
	LatencyMS          int64
	Locality           string
	Message            string
}

// ProviderProbeReceiptStore is an optional storage capability. Callers using a
// legacy or non-SQLite Store remain stateless instead of pretending persistence
// succeeded.
type ProviderProbeReceiptStore interface {
	SaveProviderProbeReceipt(context.Context, *ProviderProbeReceipt) (bool, error)
	GetProviderProbeReceipt(context.Context, string) (*ProviderProbeReceipt, error)
}
