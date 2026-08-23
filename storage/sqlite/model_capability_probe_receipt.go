package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/storage"
)

// SaveModelCapabilityProbeReceipt 仅在本次探测开始时间更新时写入最新回执。
func (s *Store) SaveModelCapabilityProbeReceipt(
	ctx context.Context,
	receipt *storage.ModelCapabilityProbeReceipt,
) (bool, error) {
	if receipt == nil {
		return false, fmt.Errorf("model capability probe receipt is nil")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO llm_model_capability_probe_receipts (
    provider_instance_id,
    model_id,
    probe_kind,
    config_fingerprint,
	probe_policy_version,
    outcome,
    failure_code,
    tested_at,
    probe_started_at,
    latency_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(provider_instance_id, model_id, probe_kind) DO UPDATE SET
    config_fingerprint = excluded.config_fingerprint,
	probe_policy_version = excluded.probe_policy_version,
    outcome = excluded.outcome,
    failure_code = excluded.failure_code,
    tested_at = excluded.tested_at,
    probe_started_at = excluded.probe_started_at,
    latency_ms = excluded.latency_ms
WHERE excluded.probe_started_at > llm_model_capability_probe_receipts.probe_started_at
`,
		receipt.ProviderInstanceID,
		receipt.ModelID,
		receipt.ProbeKind,
		receipt.ConfigFingerprint,
		receipt.ProbePolicyVersion,
		receipt.Outcome,
		receipt.FailureCode,
		receipt.TestedAt,
		receipt.ProbeStartedAt,
		receipt.LatencyMS,
	)
	if err != nil {
		return false, fmt.Errorf("save model capability probe receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read model capability probe receipt result: %w", err)
	}
	return affected == 1, nil
}

// GetModelCapabilityProbeReceipt 返回同一 Provider、模型和探测种类的最新回执。
func (s *Store) GetModelCapabilityProbeReceipt(
	ctx context.Context,
	providerInstanceID string,
	modelID string,
	probeKind string,
) (*storage.ModelCapabilityProbeReceipt, error) {
	var receipt storage.ModelCapabilityProbeReceipt
	err := s.db.QueryRowContext(ctx, `
SELECT
    provider_instance_id,
    model_id,
    probe_kind,
    config_fingerprint,
	probe_policy_version,
    outcome,
    failure_code,
    tested_at,
    probe_started_at,
    latency_ms
FROM llm_model_capability_probe_receipts
WHERE provider_instance_id = ? AND model_id = ? AND probe_kind = ?
`, providerInstanceID, modelID, probeKind).Scan(
		&receipt.ProviderInstanceID,
		&receipt.ModelID,
		&receipt.ProbeKind,
		&receipt.ConfigFingerprint,
		&receipt.ProbePolicyVersion,
		&receipt.Outcome,
		&receipt.FailureCode,
		&receipt.TestedAt,
		&receipt.ProbeStartedAt,
		&receipt.LatencyMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get model capability probe receipt: %w", err)
	}
	return &receipt, nil
}
