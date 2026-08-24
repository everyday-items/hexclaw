package migrate

// K12IMInboundReceiptsV88 为 direct IM 图片建立独立的耐久入站聚合。
// 收据、原始图片与可恢复调度必须在同一事务内创建，回调只能在事务提交后 ACK。
var K12IMInboundReceiptsV88 = Migration{
	Version:     88,
	Description: "K12 durable direct IM photo admission and recovery",
	SQL: `
CREATE TABLE IF NOT EXISTS k12_im_inbound_receipts (
    receipt_id          TEXT PRIMARY KEY,
    owner_scope         TEXT NOT NULL,
    agent_name          TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    binding_id          TEXT NOT NULL,
    platform            TEXT NOT NULL,
    instance_id         TEXT NOT NULL,
    chat_id             TEXT NOT NULL,
    provider_message_id TEXT NOT NULL,
    command_digest      TEXT NOT NULL,
    command_json        TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    UNIQUE(platform, instance_id, chat_id, provider_message_id),
    CHECK(length(trim(owner_scope)) > 0),
    CHECK(length(trim(agent_name)) > 0),
    CHECK(length(trim(binding_id)) > 0),
    CHECK(length(trim(platform)) > 0),
    CHECK(length(trim(instance_id)) > 0),
    CHECK(length(trim(chat_id)) > 0),
    CHECK(length(trim(provider_message_id)) > 0),
    CHECK(length(trim(command_digest)) > 0),
    CHECK(length(trim(command_json)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_k12_im_inbound_receipts_owner
    ON k12_im_inbound_receipts(agent_name, created_at, receipt_id);

CREATE TABLE IF NOT EXISTS k12_im_inbound_assets (
    asset_id       TEXT PRIMARY KEY,
    receipt_id     TEXT NOT NULL UNIQUE
        REFERENCES k12_im_inbound_receipts(receipt_id) ON DELETE CASCADE,
    asset_name     TEXT NOT NULL,
    asset_mime     TEXT NOT NULL,
    byte_size      INTEGER NOT NULL CHECK(byte_size > 0),
    content_digest TEXT NOT NULL,
    asset_bytes    BLOB NOT NULL CHECK(length(asset_bytes) > 0),
    created_at     INTEGER NOT NULL,
    CHECK(length(trim(asset_name)) > 0),
    CHECK(asset_mime LIKE 'image/%'),
    CHECK(length(trim(content_digest)) > 0)
);

CREATE TABLE IF NOT EXISTS k12_im_inbound_dispatches (
    dispatch_id         TEXT PRIMARY KEY,
    receipt_id          TEXT NOT NULL UNIQUE
        REFERENCES k12_im_inbound_receipts(receipt_id) ON DELETE CASCADE,
    processing_status   TEXT NOT NULL DEFAULT 'admitted'
        CHECK(processing_status IN ('admitted','image_task_submitted','final_artifact_ready')),
    routing_decision    TEXT NOT NULL DEFAULT 'pending'
        CHECK(routing_decision IN ('pending','regrade','new_submission','asked_user')),
    confirmation_status TEXT NOT NULL DEFAULT 'not_required'
        CHECK(confirmation_status IN ('not_required','waiting','confirmed')),
    image_task_id       TEXT NOT NULL DEFAULT '',
    final_artifact_id   TEXT NOT NULL DEFAULT '',
    reply_status        TEXT NOT NULL DEFAULT 'pending'
        CHECK(reply_status IN ('pending','ready','bound','delivered')),
    delivery_batch_id   TEXT NOT NULL DEFAULT '',
    version             INTEGER NOT NULL DEFAULT 0 CHECK(version >= 0),
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    CHECK((processing_status = 'admitted' AND image_task_id = '' AND final_artifact_id = '') OR
          (processing_status = 'image_task_submitted' AND image_task_id != '' AND final_artifact_id = '') OR
          (processing_status = 'final_artifact_ready' AND image_task_id != '' AND final_artifact_id != '')),
    CHECK((routing_decision = 'pending' AND confirmation_status = 'not_required') OR
          (routing_decision = 'asked_user' AND confirmation_status = 'waiting') OR
          (routing_decision IN ('regrade','new_submission') AND
              confirmation_status IN ('not_required','confirmed'))),
    CHECK((reply_status IN ('pending','ready') AND delivery_batch_id = '') OR
          (reply_status IN ('bound','delivered') AND delivery_batch_id != '')),
    CHECK(reply_status = 'pending' OR processing_status = 'final_artifact_ready')
);
CREATE INDEX IF NOT EXISTS idx_k12_im_inbound_dispatches_recovery
    ON k12_im_inbound_dispatches(reply_status, processing_status, updated_at, dispatch_id);
CREATE INDEX IF NOT EXISTS idx_k12_im_inbound_dispatches_confirmation
    ON k12_im_inbound_dispatches(confirmation_status, updated_at, dispatch_id);
`,
}
