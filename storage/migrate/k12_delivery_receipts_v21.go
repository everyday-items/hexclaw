package migrate

// K12DeliveryReceiptsV21DDL installs the durable DD-024 direct-message
// receipt ledger. Runtime stores never create release schema implicitly.
const K12DeliveryReceiptsV21DDL = `
CREATE TABLE IF NOT EXISTS k12_delivery_receipts (
    delivery_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    object_kind TEXT NOT NULL,
    object_id TEXT NOT NULL,
    binding_id TEXT NOT NULL,
    platform TEXT NOT NULL,
    instance_id TEXT NOT NULL DEFAULT '',
    chat_id TEXT NOT NULL,
    target_label TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('pending','sending','delivered','failed','outcome_unknown')),
    dedupe_key TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    render_manifest_json TEXT NOT NULL DEFAULT '',
    external_message_id TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(agent_name, dedupe_key),
    CHECK(length(trim(object_kind)) > 0),
    CHECK(length(trim(object_id)) > 0),
    CHECK(length(trim(binding_id)) > 0),
    CHECK(length(trim(platform)) > 0),
    CHECK(length(trim(chat_id)) > 0),
    CHECK(length(trim(payload_digest)) > 0),
    CHECK(length(trim(payload_json)) > 0),
    CHECK((status='delivered' AND external_message_id!='') OR status!='delivered')
);
CREATE INDEX IF NOT EXISTS idx_k12_delivery_receipts_owner_object
    ON k12_delivery_receipts(agent_name, object_kind, object_id, created_at);
CREATE INDEX IF NOT EXISTS idx_k12_delivery_receipts_recovery
    ON k12_delivery_receipts(status, updated_at);`
