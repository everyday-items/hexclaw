package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/storage"
)

// SaveProviderProbeReceipt atomically keeps only the latest-started explicit
// probe. A slow older probe cannot overwrite a newer probe that completed first.
func (s *Store) SaveProviderProbeReceipt(
	ctx context.Context,
	receipt *storage.ProviderProbeReceipt,
) (bool, error) {
	if receipt == nil {
		return false, fmt.Errorf("provider probe receipt is nil")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO llm_provider_probe_receipts (
    provider_instance_id,
    config_fingerprint,
    outcome,
    tested_at,
    probe_started_at,
    latency_ms,
    locality,
    message
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(provider_instance_id) DO UPDATE SET
    config_fingerprint = excluded.config_fingerprint,
    outcome = excluded.outcome,
    tested_at = excluded.tested_at,
    probe_started_at = excluded.probe_started_at,
    latency_ms = excluded.latency_ms,
    locality = excluded.locality,
    message = excluded.message
WHERE excluded.probe_started_at > llm_provider_probe_receipts.probe_started_at
`,
		receipt.ProviderInstanceID,
		receipt.ConfigFingerprint,
		receipt.Outcome,
		receipt.TestedAt,
		receipt.ProbeStartedAt,
		receipt.LatencyMS,
		receipt.Locality,
		receipt.Message,
	)
	if err != nil {
		return false, fmt.Errorf("save provider probe receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read provider probe receipt result: %w", err)
	}
	return affected == 1, nil
}

// GetProviderProbeReceipt returns nil when the provider has never completed an
// explicitly persisted probe.
func (s *Store) GetProviderProbeReceipt(
	ctx context.Context,
	providerInstanceID string,
) (*storage.ProviderProbeReceipt, error) {
	var receipt storage.ProviderProbeReceipt
	err := s.db.QueryRowContext(ctx, `
SELECT
    provider_instance_id,
    config_fingerprint,
    outcome,
    tested_at,
    probe_started_at,
    latency_ms,
    locality,
    message
FROM llm_provider_probe_receipts
WHERE provider_instance_id = ?
`, providerInstanceID).Scan(
		&receipt.ProviderInstanceID,
		&receipt.ConfigFingerprint,
		&receipt.Outcome,
		&receipt.TestedAt,
		&receipt.ProbeStartedAt,
		&receipt.LatencyMS,
		&receipt.Locality,
		&receipt.Message,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get provider probe receipt: %w", err)
	}
	return &receipt, nil
}
