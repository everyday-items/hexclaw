package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func TestModelCapabilityProbeReceiptMigrationDefinesCompositeIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "model-capability-probe-receipt.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}

	var ddl string
	if err := store.DB().QueryRowContext(ctx, `
SELECT sql
FROM sqlite_master
WHERE type = 'table' AND name = 'llm_model_capability_probe_receipts'
`).Scan(&ddl); err != nil {
		t.Fatalf("read model capability probe receipt schema: %v", err)
	}
	for _, want := range []string{
		"provider_instance_id",
		"model_id",
		"probe_kind",
		"config_fingerprint",
		"probe_policy_version",
		"outcome",
		"failure_code",
		"probe_started_at",
		"PRIMARY KEY (provider_instance_id, model_id, probe_kind)",
	} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("model capability probe receipt schema missing %q: %s", want, ddl)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO llm_model_capability_probe_receipts (
    provider_instance_id, model_id, probe_kind, config_fingerprint,
    outcome, tested_at, probe_started_at
) VALUES ('schema-provider', 'schema-model', 'text', 'schema-fingerprint', 'passed', 2, 1)
`); err != nil {
		t.Fatalf("insert schema default receipt: %v", err)
	}
	var probePolicyVersion string
	if err := store.DB().QueryRowContext(ctx, `
SELECT probe_policy_version
FROM llm_model_capability_probe_receipts
WHERE provider_instance_id = 'schema-provider' AND model_id = 'schema-model' AND probe_kind = 'text'
`).Scan(&probePolicyVersion); err != nil {
		t.Fatalf("read schema default policy version: %v", err)
	}
	if probePolicyVersion != "" {
		t.Fatalf("default probe policy version=%q, want empty", probePolicyVersion)
	}
}

func TestModelCapabilityProbeReceiptUpgradeAddsPolicyColumnAfterV83(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "model-capability-probe-upgrade.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 构造真实的 V82 前缀，确保随后执行的迁移拥有各自历史前置表；
	// 再单独写入旧版 V83 表，以验证 V84 对缺列数据库的升级。
	if err := migrate.Run(ctx, store.DB(), migrate.All[:82]); err != nil {
		t.Fatalf("seed migration prefix through v82: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO schema_migrations (version, description, applied_at) VALUES (83, 'legacy v83', 1);
CREATE TABLE llm_model_capability_probe_receipts (
    provider_instance_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    probe_kind TEXT NOT NULL,
    config_fingerprint TEXT NOT NULL,
    outcome TEXT NOT NULL,
    failure_code TEXT NOT NULL DEFAULT '',
    tested_at INTEGER NOT NULL,
    probe_started_at INTEGER NOT NULL,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider_instance_id, model_id, probe_kind)
);
`); err != nil {
		t.Fatalf("seed legacy v83 schema: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("upgrade legacy v83 schema: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO llm_model_capability_probe_receipts (
    provider_instance_id, model_id, probe_kind, config_fingerprint,
    probe_policy_version, outcome, tested_at, probe_started_at
) VALUES ('upgrade-provider', 'upgrade-model', 'vision', 'upgrade-fingerprint', 'v1', 'passed', 2, 1)
`); err != nil {
		t.Fatalf("write upgraded receipt with policy version: %v", err)
	}
}

func TestModelCapabilityProbeReceiptStartedAtFencesLateOlderCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "model-capability-probe-fence.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}

	newer := &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		ModelID:            "gpt-5.6-terra",
		ProbeKind:          "vision",
		ConfigFingerprint:  "new-fingerprint",
		ProbePolicyVersion: "policy-v1",
		Outcome:            "passed",
		TestedAt:           300,
		ProbeStartedAt:     200,
		LatencyMS:          100,
	}
	persisted, err := store.SaveModelCapabilityProbeReceipt(ctx, newer)
	if err != nil || !persisted {
		t.Fatalf("save newer receipt persisted=%v err=%v", persisted, err)
	}

	older := *newer
	older.ConfigFingerprint = "old-fingerprint"
	older.ProbePolicyVersion = "old-policy"
	older.Outcome = "failed"
	older.FailureCode = "upstream_429"
	older.TestedAt = 400
	older.ProbeStartedAt = 100
	persisted, err = store.SaveModelCapabilityProbeReceipt(ctx, &older)
	if err != nil {
		t.Fatalf("save late older receipt: %v", err)
	}
	if persisted {
		t.Fatal("late older receipt reported persisted=true")
	}

	got, err := store.GetModelCapabilityProbeReceipt(
		ctx,
		newer.ProviderInstanceID,
		newer.ModelID,
		newer.ProbeKind,
	)
	if err != nil {
		t.Fatalf("get vision receipt: %v", err)
	}
	if got == nil || got.ConfigFingerprint != newer.ConfigFingerprint ||
		got.ProbePolicyVersion != newer.ProbePolicyVersion ||
		got.Outcome != newer.Outcome || got.FailureCode != newer.FailureCode ||
		got.ProbeStartedAt != newer.ProbeStartedAt {
		t.Fatalf("receipt=%+v, want newer=%+v", got, newer)
	}

	toolsReceipt := *newer
	toolsReceipt.ProbeKind = "tools"
	toolsReceipt.ConfigFingerprint = "tools-fingerprint"
	toolsReceipt.ProbeStartedAt = 500
	persisted, err = store.SaveModelCapabilityProbeReceipt(ctx, &toolsReceipt)
	if err != nil || !persisted {
		t.Fatalf("save tools receipt persisted=%v err=%v", persisted, err)
	}
	got, err = store.GetModelCapabilityProbeReceipt(
		ctx,
		newer.ProviderInstanceID,
		newer.ModelID,
		toolsReceipt.ProbeKind,
	)
	if err != nil {
		t.Fatalf("get tools receipt: %v", err)
	}
	if got == nil || got.ConfigFingerprint != toolsReceipt.ConfigFingerprint ||
		got.ProbeKind != toolsReceipt.ProbeKind {
		t.Fatalf("tools receipt=%+v, want=%+v", got, toolsReceipt)
	}
}
