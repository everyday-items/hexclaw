package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

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
-- 5 元组唯一约束：防 Sync 重复写入导致指数膨胀（历史 bug：373MB data.db 来源于此）
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_rules_unique
    ON agent_rules(platform, instance_id, user_id, chat_id, agent_name);

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
	{
		Version:     2,
		Description: "v0.3.12 H8 全表 UNIQUE 约束审计：kb_documents / cron_jobs 防重复累积",
		SQL: `
-- ========== v0.3.12 H8：kb_documents 防同文件多次上传累积 ==========
-- 场景：家长同一教材 PDF 重传一次 → 产生 2 条记录但 id 不同；未来 Sync 逻辑可能累积
-- 策略：(source, title) 非空组合唯一；先 dedupe 历史重复
-- tiebreak 用 rowid（deterministic，保留最早插入的那条），避免 created_at 毫秒级碰撞
-- 导致多行同时匹配 MAX → UNIQUE INDEX 创建失败
DELETE FROM kb_documents
WHERE source != ''
  AND rowid NOT IN (
    SELECT MIN(rowid) FROM kb_documents
    WHERE source != ''
    GROUP BY source, title
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique
    ON kb_documents(source, title) WHERE source != '';

-- ========== v0.3.12 H8：cron_jobs 防同用户重复声明 ==========
-- 场景：家长通过配置 + API 双写同名任务 → 重复触发
-- 策略：(user_id, name) 唯一；同样用 MIN(rowid) 做 deterministic tiebreak
-- NULL 保护：GROUP BY 对 NULL 的处理 SQLite 会把所有 NULL 归一组；
-- 虽然 schema 里 user_id/name 都是 NOT NULL DEFAULT ''，历史数据不一定严格遵守，显式过滤
DELETE FROM cron_jobs
WHERE user_id IS NOT NULL AND name IS NOT NULL
  AND rowid NOT IN (
    SELECT MIN(rowid) FROM cron_jobs
    WHERE user_id IS NOT NULL AND name IS NOT NULL
    GROUP BY user_id, name
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cron_jobs_user_name
    ON cron_jobs(user_id, name);
`,
	},
	{
		Version:     3,
		Description: "v0.4.0 A7 模型工具调用能力探测缓存",
		SQL: `
-- 每个 (provider, model) 一条记录；30 天 TTL 由应用层判断
-- tool_call 取值：0=unknown / 1=good / 2=partial / 3=bad（对应 llmrouter.ReliabilityLevel）
CREATE TABLE IF NOT EXISTS model_capabilities (
    provider_name   TEXT     NOT NULL,
    model_name      TEXT     NOT NULL,
    tool_call       INTEGER  NOT NULL DEFAULT 0,
    last_probe      DATETIME NOT NULL,
    probe_error     TEXT     NOT NULL DEFAULT '',
    PRIMARY KEY (provider_name, model_name)
);
`,
	},
	{
		Version:     4,
		Description: "v0.4.0 E3 H8 真表 UNIQUE 收口：kb_chunks(doc_id, chunk_index) 防重复入库膨胀",
		SQL: `
-- ========== v0.4.0 E3：kb_chunks 防同文档同位置重复 chunk 累积 ==========
-- 场景：知识库 ingestion 失败 retry / 重新分块 → 旧 chunk 未删导致 (doc_id, chunk_index) 多行
-- 业务唯一键：(doc_id, chunk_index)
-- 策略：先 dedupe 历史重复（用 MIN(rowid) 做 deterministic tiebreak），再 CREATE UNIQUE INDEX
-- 注意：kb_chunks_fts FTS5 trigger 会随主表 DELETE 自动同步（content='kb_chunks' 的 external content）
DELETE FROM kb_chunks
WHERE rowid NOT IN (
    SELECT MIN(rowid) FROM kb_chunks
    GROUP BY doc_id, chunk_index
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_chunks_doc_index
    ON kb_chunks(doc_id, chunk_index);

-- 注：messages 表 id 是 PRIMARY KEY，业务唯一性已由 PK 保证，无需补复合 UNIQUE。
--     E3 评审时一度计划补 messages(session_id, seq_no)，但 schema 无 seq_no 字段，
--     且 id 已是 PK，重复持久化会被 PK 冲突拒绝，无需复合 UNIQUE。
`,
	},
	{
		Version:     5,
		Description: "v0.4.1 cron v2 脚本编译架构：删 prompt NOT NULL + 加 spec_json/source_prompt 与执行结果字段",
		Func:        migrateCronV2,
	},
	{
		Version:     6,
		Description: "v0.4.3 §11.8 交互层：Prompt 库（一库三 type）+ 记忆薄版（standing/fact）",
		SQL: `
CREATE TABLE IF NOT EXISTS prompts (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL DEFAULT 'prompt',   -- prompt | command
	title      TEXT NOT NULL,
	body_md    TEXT NOT NULL DEFAULT '',
	args_json  TEXT NOT NULL DEFAULT '',          -- 轻量参数声明（command 用，$ARGUMENTS 文本替换）
	tool_scope TEXT NOT NULL DEFAULT '',
	model      TEXT NOT NULL DEFAULT '',
	category   TEXT NOT NULL DEFAULT '',
	enabled    INTEGER NOT NULL DEFAULT 1,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS memories (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL DEFAULT 'fact',      -- standing（每轮全量注入）| fact（命中注入）
	content    TEXT NOT NULL,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`,
	},
	{
		Version:     7,
		Description: "BUG-20260626 图片持久化：messages.attachments 独立列（图片 base64 不再塞进 64KB 截断的 metadata，重载后图片不丢）",
		// ALTER ADD COLUMN 幂等：列已存在时报错被 migrate.Run 捕获跳过。
		SQL: `ALTER TABLE messages ADD COLUMN attachments TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version:     8,
		Description: "v0.5.0 M1-6 记录本原语：agent_records 通用记录表（领域中性；场景包通过 RecordSchemaRegistry 声明记录集，平台不认识 K12）",
		// 设计见 架构设计和执行计划-v0.5.0.md §5.2.3 / §6.4-6.5。
		// 通用基建列 typed；领域字段(knowledgePoint/errorCause 等)进 fields_json，平台不 typed。
		// 隔离键 = agent_name(REFERENCES agents(name)=不可变 PK)，display_name 才是可变显示名。
		SQL: `
CREATE TABLE IF NOT EXISTS agent_records (
    record_id         TEXT     PRIMARY KEY,
    agent_name        TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    collection        TEXT     NOT NULL,
    schema_version    INTEGER  NOT NULL DEFAULT 1,
    status            TEXT     NOT NULL,
    fields_json       TEXT     NOT NULL DEFAULT '{}',
    dedupe_key        TEXT     NOT NULL,
    tags_json         TEXT     NOT NULL DEFAULT '[]',
    due_at            INTEGER,
    source_session_id TEXT     NOT NULL DEFAULT '',
    version           INTEGER  NOT NULL DEFAULT 0,
    created_at        INTEGER  NOT NULL,
    updated_at        INTEGER  NOT NULL
);
-- 多孩隔离 + 记录集 + 状态：复习列表/派生指标的主查询路径
CREATE INDEX IF NOT EXISTS idx_agent_records_scope ON agent_records(agent_name, collection, status);
-- 复习队列：按到期排序（仅索引有 due 的行，避免全表）
CREATE INDEX IF NOT EXISTS idx_agent_records_due ON agent_records(agent_name, collection, due_at) WHERE due_at IS NOT NULL;
-- 幂等写：同实例+记录集内 dedupe_key 唯一（防同题重复入库膨胀）
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_records_dedupe ON agent_records(agent_name, collection, dedupe_key);
`,
	},
}

// migrateCronV2 把 cron v1 schema 升级到 v2（详见 .claude/cron-script-compilation-design.md）。
//
// 用 Func 而非 SQL 是因为：
//   - SQLite 不支持 conditional ALTER（基于 pragma 结果分支）
//   - 需要根据现有 cron_jobs 列状态动态构造 SELECT/INSERT
//   - DDL（ALTER/DROP/CREATE）在事务里行为复杂，分阶段更稳
//
// 流程：
//  1. cron_jobs 不存在 → CREATE 全新 v2 schema 就行（migrate v1 应该已建表，这里兜底）
//  2. cron_jobs 已存在但缺 spec_json 列（v1 schema）→ 表重建迁移
//  3. cron_jobs 已含 spec_json（v2 部分迁移过的旧库）→ 检查是否还有 prompt 列；有就重建
//  4. cron_job_runs 同理补 stdout/stderr/exit_code/data_json
func migrateCronV2(ctx context.Context, db *sql.DB) error {
	// — cron_jobs —
	hasJobs, err := tableExists(ctx, db, "cron_jobs")
	if err != nil {
		return fmt.Errorf("检查 cron_jobs: %w", err)
	}
	if !hasJobs {
		// v1 schema 应该已建表；兜底直接建 v2 schema
		if _, err := db.ExecContext(ctx, cronJobsV2DDL); err != nil {
			return fmt.Errorf("创建 v2 cron_jobs: %w", err)
		}
	} else {
		hasSpecJSON, _ := columnExists(ctx, db, "cron_jobs", "spec_json")
		hasPrompt, _ := columnExists(ctx, db, "cron_jobs", "prompt")
		// 任一不一致都走表重建（涵盖：v1 schema / 半 v2 / 完全 v2 但 prompt 残留）
		if !hasSpecJSON || hasPrompt {
			if err := rebuildCronJobs(ctx, db, hasSpecJSON); err != nil {
				return fmt.Errorf("重建 cron_jobs: %w", err)
			}
		}
	}

	// — cron_job_runs —
	hasRuns, err := tableExists(ctx, db, "cron_job_runs")
	if err != nil {
		return fmt.Errorf("检查 cron_job_runs: %w", err)
	}
	if !hasRuns {
		if _, err := db.ExecContext(ctx, cronJobRunsV2DDL); err != nil {
			return fmt.Errorf("创建 v2 cron_job_runs: %w", err)
		}
	} else {
		for _, col := range []struct{ name, def string }{
			{"stdout", "TEXT NOT NULL DEFAULT ''"},
			{"stderr", "TEXT NOT NULL DEFAULT ''"},
			{"exit_code", "INTEGER NOT NULL DEFAULT 0"},
			{"data_json", "TEXT NOT NULL DEFAULT ''"},
		} {
			has, _ := columnExists(ctx, db, "cron_job_runs", col.name)
			if has {
				continue
			}
			stmt := fmt.Sprintf(`ALTER TABLE cron_job_runs ADD COLUMN %s %s`, col.name, col.def)
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("ALTER cron_job_runs ADD %s: %w", col.name, err)
			}
		}
	}
	return nil
}

const cronJobsV2DDL = `CREATE TABLE cron_jobs (
    id            TEXT     PRIMARY KEY,
    name          TEXT     NOT NULL,
    type          TEXT     NOT NULL DEFAULT 'cron',
    schedule      TEXT     NOT NULL,
    spec_json     TEXT     NOT NULL DEFAULT '',
    source_prompt TEXT     NOT NULL DEFAULT '',
    user_id       TEXT     NOT NULL,
    platform      TEXT     DEFAULT '',
    chat_id       TEXT     DEFAULT '',
    status        TEXT     NOT NULL DEFAULT 'active',
    last_run_at   DATETIME,
    next_run_at   DATETIME NOT NULL,
    run_count     INTEGER  DEFAULT 0,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
)`

const cronJobRunsV2DDL = `CREATE TABLE cron_job_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'success',
    result      TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    run_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    stdout      TEXT    NOT NULL DEFAULT '',
    stderr      TEXT    NOT NULL DEFAULT '',
    exit_code   INTEGER NOT NULL DEFAULT 0,
    data_json   TEXT    NOT NULL DEFAULT ''
)`

// rebuildCronJobs 重建 cron_jobs：保留 spec_json 非空的 v2 任务，丢弃 v1 任务。
// hasSpecJSON 控制 INSERT SELECT 是否引用旧 spec_json 列。
func rebuildCronJobs(ctx context.Context, db *sql.DB, hasSpecJSON bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE cron_jobs_v5_new (
        id            TEXT     PRIMARY KEY,
        name          TEXT     NOT NULL,
        type          TEXT     NOT NULL DEFAULT 'cron',
        schedule      TEXT     NOT NULL,
        spec_json     TEXT     NOT NULL DEFAULT '',
        source_prompt TEXT     NOT NULL DEFAULT '',
        user_id       TEXT     NOT NULL,
        platform      TEXT     DEFAULT '',
        chat_id       TEXT     DEFAULT '',
        status        TEXT     NOT NULL DEFAULT 'active',
        last_run_at   DATETIME,
        next_run_at   DATETIME NOT NULL,
        run_count     INTEGER  DEFAULT 0,
        created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
    )`); err != nil {
		return err
	}

	if hasSpecJSON {
		// 已 v2 部分迁移过：保留 spec_json 非空的 v2 任务
		if _, err := tx.ExecContext(ctx, `INSERT INTO cron_jobs_v5_new
            (id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status,
             last_run_at, next_run_at, run_count, created_at)
            SELECT id, name, COALESCE(type,'cron'), schedule,
                   COALESCE(spec_json,''), COALESCE(source_prompt,''),
                   user_id, COALESCE(platform,''), COALESCE(chat_id,''), COALESCE(status,'active'),
                   last_run_at, next_run_at, COALESCE(run_count,0),
                   COALESCE(created_at, CURRENT_TIMESTAMP)
            FROM cron_jobs
            WHERE spec_json IS NOT NULL AND spec_json != ''`); err != nil {
			return err
		}
	}
	// 否则纯 v1 schema：v1 任务在 v2 架构下无法运行（没编译 Spec），不复制即丢弃

	if _, err := tx.ExecContext(ctx, `DROP TABLE cron_jobs`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE cron_jobs_v5_new RENAME TO cron_jobs`); err != nil {
		return err
	}
	return tx.Commit()
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	return n > 0, err
}

func columnExists(ctx context.Context, db *sql.DB, table, col string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}
