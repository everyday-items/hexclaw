package migrate

// All 定义所有数据库迁移，按版本顺序执行。
//
// 设计原则：
//   - 所有 DDL 集中在此文件，不再分散到各模块 Init()
//   - 每个迁移必须幂等（IF NOT EXISTS / IF NOT EXISTS 列名检查）
//   - ALTER TABLE ADD COLUMN 在 SQLite 中天然幂等（列已存在时报错，被 migrate.Run 捕获跳过）
//   - 时间戳统一为 DATETIME（Go time.Time 兼容），索引和比较在 SQLite 中高效
//   - 所有列显式 NOT NULL + DEFAULT，避免隐式 NULL
//   - 每个表有 meta TEXT 字段用于扩展（参考生产表的 JSON meta 列）
//
// 生产数据质量设计：
//   - messages 表增加 model_name / prompt_tokens / completion_tokens / finish_reason / latency_ms
//     → 确保每条消息可追踪到模型、成本、性能
//   - messages 表增加 request_id
//     → 支持幂等写入和请求级关联（tool_calls ↔ tool_result）
//   - messages 表增加 content_type
//     → 区分 text / multimodal_json，避免导出时类型信息丢失
//   - sessions 表增加冗余统计字段
//     → 会话列表无需 JOIN 即可展示消息数、token 汇总、最后消息摘要
var All = []Migration{
	{
		Version:     1,
		Description: "初始 schema（会话、消息、成本、缓存、知识库、定时任务、Webhook、Agent、平台实例）",
		SQL: `
-- ========== 会话 ==========
CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT    PRIMARY KEY,
    user_id           TEXT    NOT NULL,
    platform          TEXT    NOT NULL DEFAULT 'web',
    instance_id       TEXT    NOT NULL DEFAULT '',
    chat_id           TEXT    NOT NULL DEFAULT '',
    title             TEXT    NOT NULL DEFAULT '',
    parent_session_id TEXT    NOT NULL DEFAULT '',
    branch_message_id TEXT    NOT NULL DEFAULT '',
    status            INTEGER NOT NULL DEFAULT 1,
    message_count          INTEGER NOT NULL DEFAULT 0,
    total_prompt_tokens    INTEGER NOT NULL DEFAULT 0,
    total_completion_tokens INTEGER NOT NULL DEFAULT 0,
    last_message_preview   TEXT    NOT NULL DEFAULT '',
    meta              TEXT    NOT NULL DEFAULT '{}',
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_parent     ON sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_scope      ON sessions(user_id, platform, instance_id, chat_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_status     ON sessions(status, updated_at);

-- ========== 消息 ==========
CREATE TABLE IF NOT EXISTS messages (
    id                TEXT    PRIMARY KEY,
    session_id        TEXT    NOT NULL REFERENCES sessions(id),
    parent_id         TEXT    NOT NULL DEFAULT '',
    role              TEXT    NOT NULL,
    content           TEXT    NOT NULL,
    content_type      TEXT    NOT NULL DEFAULT 'text',
    metadata          TEXT    NOT NULL DEFAULT '{}',
    feedback          TEXT    NOT NULL DEFAULT '',
    model_name        TEXT    NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    finish_reason     TEXT    NOT NULL DEFAULT '',
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    request_id        TEXT    NOT NULL DEFAULT '',
    meta              TEXT    NOT NULL DEFAULT '{}',
    created_at        DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);
CREATE INDEX IF NOT EXISTS idx_messages_parent_id  ON messages(parent_id);
CREATE INDEX IF NOT EXISTS idx_messages_request_id ON messages(request_id);

-- ========== 消息全文搜索 ==========
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages
BEGIN INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content); END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages
BEGIN INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content); END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages
BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- ========== 成本记录 ==========
CREATE TABLE IF NOT EXISTS cost_records (
    id                TEXT    PRIMARY KEY,
    user_id           TEXT    NOT NULL,
    session_id        TEXT    NOT NULL DEFAULT '',
    message_id        TEXT    NOT NULL DEFAULT '',
    provider          TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost              REAL    NOT NULL DEFAULT 0,
    meta              TEXT    NOT NULL DEFAULT '{}',
    created_at        DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cost_records_user_id    ON cost_records(user_id);
CREATE INDEX IF NOT EXISTS idx_cost_records_created_at ON cost_records(created_at);
CREATE INDEX IF NOT EXISTS idx_cost_records_session    ON cost_records(session_id);

-- ========== LLM 缓存 ==========
CREATE TABLE IF NOT EXISTS llm_cache (
    key        TEXT    PRIMARY KEY,
    response   TEXT    NOT NULL,
    provider   TEXT    NOT NULL,
    model      TEXT    NOT NULL,
    hit_count  INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_llm_cache_expires ON llm_cache(expires_at);

-- ========== 知识库文档 ==========
CREATE TABLE IF NOT EXISTS kb_documents (
    id            TEXT    PRIMARY KEY,
    title         TEXT    NOT NULL,
    content       TEXT    NOT NULL,
    source        TEXT    NOT NULL DEFAULT '',
    source_type   TEXT    NOT NULL DEFAULT 'manual',
    chunk_count   INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL DEFAULT 'indexed',
    deleted       INTEGER NOT NULL DEFAULT 0,
    error_message TEXT    NOT NULL DEFAULT '',
    meta          TEXT    NOT NULL DEFAULT '{}',
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kb_documents_deleted ON kb_documents(deleted);

-- ========== 知识库分块 ==========
CREATE TABLE IF NOT EXISTS kb_chunks (
    id          TEXT    PRIMARY KEY,
    doc_id      TEXT    NOT NULL,
    content     TEXT    NOT NULL,
    chunk_index INTEGER NOT NULL,
    embedding   BLOB,
    created_at  DATETIME NOT NULL,
    FOREIGN KEY (doc_id) REFERENCES kb_documents(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_kb_chunks_doc ON kb_chunks(doc_id);

-- ========== 知识库全文搜索 ==========
CREATE VIRTUAL TABLE IF NOT EXISTS kb_chunks_fts USING fts5(
    content,
    chunk_id UNINDEXED
);

-- ========== 定时任务 ==========
CREATE TABLE IF NOT EXISTS cron_jobs (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    type        TEXT    NOT NULL DEFAULT 'cron',
    schedule    TEXT    NOT NULL,
    prompt      TEXT    NOT NULL,
    user_id     TEXT    NOT NULL,
    platform    TEXT    NOT NULL DEFAULT '',
    chat_id     TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'active',
    last_run_at DATETIME,
    next_run_at DATETIME NOT NULL,
    run_count   INTEGER NOT NULL DEFAULT 0,
    meta        TEXT    NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_cron_jobs_user   ON cron_jobs(user_id);
CREATE INDEX IF NOT EXISTS idx_cron_jobs_status ON cron_jobs(status, next_run_at);

-- ========== 定时任务执行记录 ==========
CREATE TABLE IF NOT EXISTS cron_job_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'success',
    result      TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    run_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_cron_job_runs_job ON cron_job_runs(job_id, run_at);

-- ========== Webhook ==========
CREATE TABLE IF NOT EXISTS webhooks (
    id            TEXT    PRIMARY KEY,
    name          TEXT    UNIQUE NOT NULL,
    type          TEXT    NOT NULL DEFAULT 'generic',
    secret        TEXT    NOT NULL DEFAULT '',
    prompt        TEXT    NOT NULL,
    user_id       TEXT    NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_event_at DATETIME,
    event_count   INTEGER NOT NULL DEFAULT 0,
    meta          TEXT    NOT NULL DEFAULT '{}',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_webhooks_user ON webhooks(user_id);

-- ========== Agent 定义 ==========
CREATE TABLE IF NOT EXISTS agents (
    name          TEXT    PRIMARY KEY,
    display_name  TEXT    NOT NULL DEFAULT '',
    description   TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL DEFAULT '',
    provider      TEXT    NOT NULL DEFAULT '',
    system_prompt TEXT    NOT NULL DEFAULT '',
    skills        TEXT    NOT NULL DEFAULT '[]',
    max_tokens    INTEGER NOT NULL DEFAULT 0,
    temperature   REAL    NOT NULL DEFAULT 0,
    metadata      TEXT    NOT NULL DEFAULT '{}',
    is_default    INTEGER NOT NULL DEFAULT 0,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- ========== Agent 路由规则 ==========
CREATE TABLE IF NOT EXISTS agent_rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    platform    TEXT    NOT NULL DEFAULT '',
    instance_id TEXT    NOT NULL DEFAULT '',
    user_id     TEXT    NOT NULL DEFAULT '',
    chat_id     TEXT    NOT NULL DEFAULT '',
    agent_name  TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    priority    INTEGER NOT NULL DEFAULT 0,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_agent_rules_agent ON agent_rules(agent_name);

-- ========== 平台实例 ==========
CREATE TABLE IF NOT EXISTS platform_instances (
    id            TEXT    PRIMARY KEY,
    provider      TEXT    NOT NULL,
    name          TEXT    NOT NULL UNIQUE,
    enabled       INTEGER NOT NULL DEFAULT 1,
    mode          TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL DEFAULT 'stopped',
    config_json   TEXT    NOT NULL DEFAULT '{}',
    last_event_at DATETIME,
    last_error    TEXT    NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_platform_instances_provider ON platform_instances(provider);

-- ========== 平台事件去重 ==========
CREATE TABLE IF NOT EXISTS platform_events (
    instance_name TEXT    NOT NULL,
    event_id      TEXT    NOT NULL,
    created_at    DATETIME NOT NULL,
    PRIMARY KEY (instance_name, event_id)
);
CREATE INDEX IF NOT EXISTS idx_platform_events_created_at ON platform_events(created_at);
`,
	},
}
