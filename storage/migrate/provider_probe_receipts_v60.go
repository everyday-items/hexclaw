package migrate

// ProviderProbeReceiptsV60 persists only the latest explicit connection-test
// receipt for each stable provider identity. The configuration fingerprint is
// irreversible and contains no raw API key.
var ProviderProbeReceiptsV60 = Migration{
	Version:     60,
	Description: "BUG-20260728-011 LLM Provider explicit probe latest receipt",
	SQL: `
CREATE TABLE IF NOT EXISTS llm_provider_probe_receipts (
    provider_instance_id TEXT    PRIMARY KEY,
    config_fingerprint  TEXT    NOT NULL,
    outcome             TEXT    NOT NULL CHECK(outcome IN ('passed', 'failed')),
    tested_at           INTEGER NOT NULL,
    probe_started_at    INTEGER NOT NULL,
    latency_ms          INTEGER NOT NULL DEFAULT 0 CHECK(latency_ms >= 0),
    locality            TEXT    NOT NULL DEFAULT '',
    message             TEXT    NOT NULL DEFAULT ''
);
`,
}
