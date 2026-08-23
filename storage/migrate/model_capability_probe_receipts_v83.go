package migrate

// ModelCapabilityProbeReceiptsV83 按 Provider 实例、模型和探测种类持久化最新显式能力探测回执。
var ModelCapabilityProbeReceiptsV83 = Migration{
	Version:     83,
	Description: "LLM model capability explicit probe latest receipt",
	SQL: `
CREATE TABLE IF NOT EXISTS llm_model_capability_probe_receipts (
    provider_instance_id TEXT    NOT NULL,
    model_id             TEXT    NOT NULL,
    probe_kind           TEXT    NOT NULL,
    config_fingerprint   TEXT    NOT NULL,
    probe_policy_version TEXT    NOT NULL DEFAULT '',
    outcome              TEXT    NOT NULL CHECK(outcome IN ('passed', 'failed')),
    failure_code         TEXT    NOT NULL DEFAULT '',
    tested_at            INTEGER NOT NULL,
    probe_started_at     INTEGER NOT NULL,
    latency_ms           INTEGER NOT NULL DEFAULT 0 CHECK(latency_ms >= 0),
    PRIMARY KEY (provider_instance_id, model_id, probe_kind)
);
`,
}
