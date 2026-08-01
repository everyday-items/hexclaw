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
		Description: "v0.3.12 H8 唯一约束占位：无损隔离延后到兼容 schema 的 v4/v5 Func",
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
		Description: "v0.4.0 E3/H8 Knowledge parent/chunk 无损隔离与唯一约束",
		Func:        migrateKnowledgeUniqueV4,
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
		// 设计见 架构设计-v0.5.0.md §5.1 数据分层 / §6.5 后端边界。
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
	{
		Version:     9,
		Description: "v0.5.0 §6.9 类型化存储 + Outbox：k12_* 专表建表 + agent_records K12 数据迁移 + outbox_events（ADR-K12-006/013 一次切换）",
		// 设计见 架构设计-v0.5.0.md §6.9/§6.14/§6.15 与 §5.5 字段表。
		//   - 聚合根专表复刻 agent_records 的通用基建列（record_id/agent_name/status/dedupe_key/
		//     due_at/version/时间戳），领域字段从 fields_json 拍平为 typed 列（json_extract）；
		//   - 练习项 / 作品版本 / 形成性反馈拆子表（§6.9 表清单：k12_practice_set_items /
		//     k12_creative_work_versions / k12_work_feedback），子表随聚合根整体读写；
		//   - 数据迁移幂等（INSERT OR IGNORE），迁完删除 agent_records 中 K12 collection 行
		//     （一次切换不留双轨；agent_records 表保留给其他场景）；
		//   - outbox_events 与领域事实同库同事务（§6.15 禁止拆两个库）；消费者以
		//     (consumer, event_id) 去重（outbox_consumptions），at-least-once + dead-letter。
		// 边界申报：本迁移文件按 §6.9 明文要求承载 k12_* DDL 与 collection 常量字面量，
		// 是平台迁移层对场景表的唯一知情点（engine/records 仍零领域词）。
		SQL: k12TypedV9SQL,
	},
	{
		Version:     10,
		Description: "v0.5.0 §6.2/§6.3 ScenarioManifest v2：scenario_installations 安装收据台账（§6.9 清单表补齐）",
		// 设计见 架构设计-v0.5.0.md §6.2（唯一版本化 Manifest）/§6.3（原子安装、升级和卸载）。
		//   - 每场景一行（scenario_id 主键）；manifest_json = 声明头（不含函数资源载荷），
		//     receipt_json = InstallationReceipt（实际创建资源，卸载按它精确清理）；
		//   - 卸载不删行、不删业务数据（k12_* 等表保留）：文档只规定「按 Receipt 清理本场景
		//     资源」，未规定删业务数据 → 采取数据保留策略，仅标记 status=uninstalled；
		//   - 重装/升级走 upsert 复位 installed（见 storage/scenarioinstall）。
		SQL: `
CREATE TABLE IF NOT EXISTS scenario_installations (
    scenario_id      TEXT     PRIMARY KEY,
    version          TEXT     NOT NULL,
    contract_version INTEGER  NOT NULL DEFAULT 1,
    status           TEXT     NOT NULL DEFAULT 'installed',
    mount_path       TEXT     NOT NULL DEFAULT '',
    manifest_json    TEXT     NOT NULL DEFAULT '{}',
    receipt_json     TEXT     NOT NULL DEFAULT '{}',
    installed_at     INTEGER  NOT NULL,
    uninstalled_at   INTEGER,
    updated_at       INTEGER  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scenario_installations_status ON scenario_installations(status);
`,
	},
	{
		Version:     11,
		Description: "BUG-20260718 K12 去重键释放回填：存量已退出活跃空间的行补墓碑键 #released#<record_id>（作品归档死路 / 练习集固化篮截胡）",
		// 运行态修复（RecordSchema.ReleaseDedupeOnStatuses + k12storage 墓碑键）只覆盖
		// 迁移后的状态变更；存量库中已进入释放状态但没有墓碑键的行仍会永久截胡同键新建，
		// 本迁移一次性回填。规则与 k12storage.dedupeReleaseAssign 完全一致：
		//   dedupe_key := dedupe_key || '#released#' || record_id
		//   - 作品：archived；
		//   - 练习集：confirmed/assigned/submitted/graded/closed/cancelled
		//     （confirmed 是 FinalizeBasket 的审计中间态，中途中断的行同样已离开活跃篮）。
		// 幂等可重跑：instr 守卫跳过已含墓碑分隔符的行——包括曾手工 SQL 修过的存量数据
		// （如本机 data.db 的作品行），绝不二次叠加；只动 k12_* 表，其他场景零影响。
		// 墓碑值含各自 record_id，同键多行释放互不碰撞，不会触发 (agent_name,dedupe_key) 唯一索引冲突。
		SQL: `
UPDATE k12_creative_works
SET dedupe_key = dedupe_key || '#released#' || record_id
WHERE status = 'archived'
  AND instr(dedupe_key, '#released#') = 0;

UPDATE k12_practice_sets
SET dedupe_key = dedupe_key || '#released#' || record_id
WHERE status IN ('confirmed','assigned','submitted','graded','closed','cancelled')
  AND instr(dedupe_key, '#released#') = 0;
`,
	},
	{
		Version:     12,
		Description: "v0.5.0 DD-027/DD-028：原子自定义组卷任务与只追加作答照片批次",
		// DD-027：正式后端组卷命令以 k12_practice_generation_jobs 冻结请求与终态，
		// committed 结果和练习篮在同一事务提交；同 agent + idempotency_key 唯一。
		// DD-028：回传照片拆为 append-only 子表，return_id 在卷内唯一，item_ids_json
		// 保留一张照片覆盖多题以及同题被多批照片覆盖的双向证据。
		Func: migrateK12PracticeCommandsV12,
	},
	{
		Version:     13,
		Description: "v0.5.0 DD-004/DD-023：正式卷面号预占与原生两阶段 PrintJob",
		// prepare 在一笔事务内预占 Tutor 内唯一 paper_no 并冻结同源题卷/答案卷；
		// 原生 printed receipt 才能在另一笔事务内同时推进 PrintJob 与 PracticeSet。
		SQL: k12PrintJobsV13DDL,
	},
	{
		Version:     14,
		Description: "v0.5.0 DD-016：作品点评结构化事实持久化",
		// 自由 Markdown 只作为兼容投影；可审计的证据引用、观察维度、限制、建议和
		// 允许动作统一保存在 structured_feedback_json。使用探测式迁移，兼容人工
		// 修复或半迁移数据库并保持安全重入。
		Func: migrateK12StructuredFeedbackV14,
	},
	{
		Version:     15,
		Description: "v0.5.0 DD-020：外部模型调用账本与结果未知态",
		// 模型调用只承诺持久账本和 outcome_unknown 收敛；领域副作用继续由
		// GradingJob checkpoint + Outbox 提供 exactly-once 边界。
		SQL: K12ModelInvocationsV15DDL,
	},
	{
		Version:     16,
		Description: "v0.5.0 DD-005/DD-006：链路级原子切换与增量回滚 journal",
		SQL:         K12CutoverV16DDL,
	},
	{
		Version:     17,
		Description: "v0.5.0 DD-025：archive v3 跨 Tutor restore-as、不可变快照与追加 journal",
		SQL:         K12RestoreAsV17DDL,
	},
	{
		Version:     18,
		Description: "v0.5.0 DD-019：K12 Webhook binding、Receipt、nonce 与审计持久化",
		Func:        migrateK12WebhooksV18,
	},
	{
		Version:     19,
		Description: "v0.5.0 DD-010～DD-012：父子 Problem、独立 Attempt 与 raw/canonical OCR 事实",
		SQL:         K12ProblemAttemptsDDL,
	},
	{
		Version:     20,
		Description: "v0.5.0 DD-013：作文照片 OCR Job、不可变原文与版本化家长确认",
		Func:        migrateK12CreativeWorkOCRV20,
	},
	{
		Version:     21,
		Description: "v0.5.0 DD-024：发送到手机的持久投递回执与结果未知态",
		// HTTP 接受只记为 sending；只有平台状态查询返回 delivered 才进入终态。
		// payload/render/target/binding 一次冻结，使失败重试与重启对账都复用同一事实。
		SQL: K12DeliveryReceiptsV21DDL,
	},
	{
		Version:     22,
		Description: "v0.5.0 DD-019：Webhook Receipt 安全重试证据与尝试计数",
		// 只有应用命令明确写入 retry_safe=1 的 failed Receipt 才能重新派发；
		// 历史数据和结果未知态默认 fail closed，避免盲目重复外部副作用。
		Func: migrateK12WebhookRetryV22,
	},
	KnowledgeIndexV23,
	KnowledgeIngestV24,
	K12GenericPrintJobsV25,
	KnowledgeIngestGenerationsV26,
	KnowledgeDocumentScopeV27,
	KnowledgeIngestCheckpointV28,
	CronIntegrityV29,
	K12GradingResumeV30,
	K12TutoringTipsV31,
	K12ImageTasksV32,
	K12ParentGuideV33,
	K12ManualCreativeIntakeV34,
	K12MistakeArchiveV35,
	K12DeliveryBatchesV36,
	K12CreativeAccumulationV37,
	K12ParentConfirmationV38,
	K12SinglePracticeGenerationV39,
	K12PracticeReturnRegradeV40,
	K12InsightSnapshotV41,
	K12ProblemSourceNumberV42,
	SessionStatsRepairV43,
	K12GradingPhysicalCallsV44,
	K12PrintArtifactRendersV45,
	KnowledgeIngestExecutionV46,
	K12ImageTaskAutomaticDeadlineV47,
	K12ProblemProgressiveV48,
	K12GradingFinalArtifactsV49,
	K12ProblemSourceActionsV50,
	K12ProblemStructuresV51,
	K12WeeklyPracticeV52,
	K12SubjectTextbooksV53,
	K12TextbookBindingV54,
	K12WeeklyAssessmentV55,
	K12WeeklyArithmeticV56,
	K12PracticeCandidateReviewV57,
	K12WeeklyManualTracksV58,
	K12WeeklyManualCountsV59,
	ProviderProbeReceiptsV60,
	K12ImageTaskSolveInvocationsV61,
	K12AgentCascadeV62,
	RememberedPermissionGrantsV63,
	K12ModelInvocationRequestPolicyV64,
	K12ModelPhysicalInvocationsV65,
	K12TextbookCatalogProofV66,
	K12TextbookCatalogWorkerV67,
}

const k12PrintJobsV13DDL = `CREATE TABLE IF NOT EXISTS k12_paper_no_counters (
    agent_name    TEXT    PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
    next_sequence INTEGER NOT NULL DEFAULT 1,
    updated_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS k12_print_jobs (
    print_job_id          TEXT    PRIMARY KEY,
    agent_name            TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    idempotency_key       TEXT    NOT NULL,
    request_digest        TEXT    NOT NULL,
    practice_set_id       TEXT    NOT NULL REFERENCES k12_practice_sets(record_id) ON DELETE CASCADE,
    base_set_version      INTEGER NOT NULL,
    artifact_kind         TEXT    NOT NULL,
    artifact_id           TEXT    NOT NULL,
    question_artifact_id  TEXT    NOT NULL,
    answer_artifact_id    TEXT    NOT NULL,
    paper_no              TEXT    NOT NULL DEFAULT '',
    source_digest         TEXT    NOT NULL DEFAULT '',
    prepared_fields_json  TEXT    NOT NULL DEFAULT '{}',
    status                TEXT    NOT NULL DEFAULT 'preparing',
    attempt_count         INTEGER NOT NULL DEFAULT 1,
    native_job_id         TEXT    NOT NULL DEFAULT '',
    native_receipt_id     TEXT    NOT NULL DEFAULT '',
    printer_snapshot_json TEXT    NOT NULL DEFAULT '{}',
    failure_kind          TEXT    NOT NULL DEFAULT '',
    failure_detail        TEXT    NOT NULL DEFAULT '',
    prepared_at           INTEGER NOT NULL,
    printed_at            INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    version               INTEGER NOT NULL DEFAULT 0,
    UNIQUE (agent_name, idempotency_key),
    UNIQUE (agent_name, practice_set_id, base_set_version),
    UNIQUE (agent_name, paper_no)
);
CREATE INDEX IF NOT EXISTS idx_k12_print_jobs_set
    ON k12_print_jobs(agent_name, practice_set_id, status, updated_at);

-- DD-004: 已固化卷在同一 Tutor 下也必须由数据库拒绝重复卷面号；空值是 draft。
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_practice_sets_paper_no
    ON k12_practice_sets(agent_name, paper_no) WHERE paper_no != '';
`

const k12PracticeCommandsV12DDL = `CREATE TABLE IF NOT EXISTS k12_practice_return_assets (
    set_record_id TEXT    NOT NULL REFERENCES k12_practice_sets(record_id) ON DELETE CASCADE,
    return_index  INTEGER NOT NULL,
    return_id     TEXT    NOT NULL,
    asset_id      TEXT    NOT NULL,
    item_ids_json TEXT    NOT NULL DEFAULT '[]',
    returned_at   INTEGER NOT NULL,
    PRIMARY KEY (set_record_id, return_id),
    UNIQUE (set_record_id, return_index)
);
CREATE INDEX IF NOT EXISTS idx_k12_return_assets_set_time
    ON k12_practice_return_assets(set_record_id, returned_at, return_index);

CREATE TABLE IF NOT EXISTS k12_practice_generation_jobs (
    generation_job_id   TEXT    PRIMARY KEY,
    agent_name          TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    idempotency_key     TEXT    NOT NULL,
    request_digest      TEXT    NOT NULL,
    scope               TEXT    NOT NULL,
    variants_per_source INTEGER NOT NULL,
    difficulty          TEXT    NOT NULL,
    total               TEXT    NOT NULL,
    textbook            TEXT    NOT NULL DEFAULT '',
    status              TEXT    NOT NULL DEFAULT 'queued',
    result_set_id       TEXT    NOT NULL DEFAULT '',
    result_item_ids_json TEXT   NOT NULL DEFAULT '[]',
    deduplicated_count  INTEGER NOT NULL DEFAULT 0,
    failure_reason      TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    UNIQUE (agent_name, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_generation_jobs_scope
    ON k12_practice_generation_jobs(agent_name, status, updated_at);
`

// k12TypedV9SQL V9 迁移全文（建表 + 数据迁移 + 一次切换清理）。
const k12TypedV9SQL = `
-- ========== 错题（§5.5 Mistake；review_policy_version 见 k12_review_policies）==========
CREATE TABLE IF NOT EXISTS k12_mistakes (
    record_id             TEXT     PRIMARY KEY,
    agent_name            TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    schema_version        INTEGER  NOT NULL DEFAULT 1,
    status                TEXT     NOT NULL,
    subject               TEXT     NOT NULL DEFAULT '',
    question              TEXT     NOT NULL DEFAULT '',
    knowledge_point       TEXT     NOT NULL DEFAULT '',
    error_cause           TEXT     NOT NULL DEFAULT '',
    wrong_process         TEXT     NOT NULL DEFAULT '',
    canonical_answer      TEXT     NOT NULL DEFAULT '',
    review_stage          INTEGER  NOT NULL DEFAULT 0,
    last_retried_at       INTEGER  NOT NULL DEFAULT 0,
    spot_check_state      TEXT     NOT NULL DEFAULT '',
    entry_source          TEXT     NOT NULL DEFAULT '',
    review_policy_version TEXT     NOT NULL DEFAULT 'v1',
    dedupe_key            TEXT     NOT NULL,
    tags_json             TEXT     NOT NULL DEFAULT '[]',
    due_at                INTEGER,
    source_session_id     TEXT     NOT NULL DEFAULT '',
    version               INTEGER  NOT NULL DEFAULT 0,
    created_at            INTEGER  NOT NULL,
    updated_at            INTEGER  NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_mistakes_dedupe ON k12_mistakes(agent_name, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_k12_mistakes_scope ON k12_mistakes(agent_name, status);
CREATE INDEX IF NOT EXISTS idx_k12_mistakes_due ON k12_mistakes(agent_name, due_at) WHERE due_at IS NOT NULL;

-- ========== 积累本 ==========
CREATE TABLE IF NOT EXISTS k12_accumulations (
    record_id         TEXT     PRIMARY KEY,
    agent_name        TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    schema_version    INTEGER  NOT NULL DEFAULT 1,
    status            TEXT     NOT NULL,
    subject           TEXT     NOT NULL DEFAULT '',
    entry_type        TEXT     NOT NULL DEFAULT '',
    content           TEXT     NOT NULL DEFAULT '',
    source_ref        TEXT     NOT NULL DEFAULT '',
    review_stage      INTEGER  NOT NULL DEFAULT 0,
    last_retried_at   INTEGER  NOT NULL DEFAULT 0,
    dedupe_key        TEXT     NOT NULL,
    tags_json         TEXT     NOT NULL DEFAULT '[]',
    due_at            INTEGER,
    source_session_id TEXT     NOT NULL DEFAULT '',
    version           INTEGER  NOT NULL DEFAULT 0,
    created_at        INTEGER  NOT NULL,
    updated_at        INTEGER  NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_accum_dedupe ON k12_accumulations(agent_name, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_k12_accum_scope ON k12_accumulations(agent_name, status);
CREATE INDEX IF NOT EXISTS idx_k12_accum_due ON k12_accumulations(agent_name, due_at) WHERE due_at IS NOT NULL;

-- ========== 练习集（§5.5 PracticeSet 卷级字段）==========
CREATE TABLE IF NOT EXISTS k12_practice_sets (
    record_id             TEXT     PRIMARY KEY,
    agent_name            TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    schema_version        INTEGER  NOT NULL DEFAULT 1,
    status                TEXT     NOT NULL,
    source_kind           TEXT     NOT NULL DEFAULT '',
    title                 TEXT     NOT NULL DEFAULT '',
    paper_no              TEXT     NOT NULL DEFAULT '',
    question_artifact_id  TEXT     NOT NULL DEFAULT '',
    answer_artifact_id    TEXT     NOT NULL DEFAULT '',
    skipped_blocked_count INTEGER  NOT NULL DEFAULT 0,
    finalized_at          INTEGER  NOT NULL DEFAULT 0,
    finalized_via         TEXT     NOT NULL DEFAULT '',
    reminder_sent_at      INTEGER  NOT NULL DEFAULT 0,
    reminder_dismissed    INTEGER  NOT NULL DEFAULT 0,
    closed_reason         TEXT     NOT NULL DEFAULT '',
    delivery_status       TEXT     NOT NULL DEFAULT '',
    delivery_target       TEXT     NOT NULL DEFAULT '',
    dedupe_key            TEXT     NOT NULL,
    tags_json             TEXT     NOT NULL DEFAULT '[]',
    due_at                INTEGER,
    source_session_id     TEXT     NOT NULL DEFAULT '',
    version               INTEGER  NOT NULL DEFAULT 0,
    created_at            INTEGER  NOT NULL,
    updated_at            INTEGER  NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_psets_dedupe ON k12_practice_sets(agent_name, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_k12_psets_scope ON k12_practice_sets(agent_name, status);

-- ========== 练习项子表（§5.5 PracticeSetItem；随聚合根整体读写）==========
CREATE TABLE IF NOT EXISTS k12_practice_set_items (
    set_record_id            TEXT    NOT NULL REFERENCES k12_practice_sets(record_id) ON DELETE CASCADE,
    item_index               INTEGER NOT NULL,
    item_id                  TEXT    NOT NULL DEFAULT '',
    source_problem_id        TEXT    NOT NULL DEFAULT '',
    subject                  TEXT    NOT NULL DEFAULT '',
    added_via                TEXT    NOT NULL DEFAULT '',
    question_markdown        TEXT    NOT NULL DEFAULT '',
    expected_answer_markdown TEXT    NOT NULL DEFAULT '',
    verification_status      TEXT    NOT NULL DEFAULT '',
    verification_evidence    TEXT    NOT NULL DEFAULT '',
    blocked_reason           TEXT    NOT NULL DEFAULT '',
    paper_seq                INTEGER NOT NULL DEFAULT 0,
    returned                 INTEGER NOT NULL DEFAULT 0,
    practice_problem_id      TEXT    NOT NULL DEFAULT '',
    result_correct           INTEGER,
    PRIMARY KEY (set_record_id, item_index)
);

-- ========== 作品（§5.5 CreativeWork）==========
CREATE TABLE IF NOT EXISTS k12_creative_works (
    record_id         TEXT     PRIMARY KEY,
    agent_name        TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    schema_version    INTEGER  NOT NULL DEFAULT 1,
    status            TEXT     NOT NULL,
    work_type         TEXT     NOT NULL DEFAULT '',
    title             TEXT     NOT NULL DEFAULT '',
    task              TEXT     NOT NULL DEFAULT '',
    intent            TEXT     NOT NULL DEFAULT '',
    dedupe_key        TEXT     NOT NULL,
    tags_json         TEXT     NOT NULL DEFAULT '[]',
    due_at            INTEGER,
    source_session_id TEXT     NOT NULL DEFAULT '',
    version           INTEGER  NOT NULL DEFAULT 0,
    created_at        INTEGER  NOT NULL,
    updated_at        INTEGER  NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_works_dedupe ON k12_creative_works(agent_name, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_k12_works_scope ON k12_creative_works(agent_name, status);

-- ========== 作品版本子表（§5.5 CreativeWorkVersion）==========
CREATE TABLE IF NOT EXISTS k12_creative_work_versions (
    work_record_id        TEXT    NOT NULL REFERENCES k12_creative_works(record_id) ON DELETE CASCADE,
    version_index         INTEGER NOT NULL,
    version_id            TEXT    NOT NULL DEFAULT '',
    source_asset_id       TEXT    NOT NULL DEFAULT '',
    content_markdown      TEXT    NOT NULL DEFAULT '',
    practice_card_done_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (work_record_id, version_index)
);

-- ========== 形成性反馈子表（§6.9 k12_work_feedback；每版本至多一条）==========
CREATE TABLE IF NOT EXISTS k12_work_feedback (
    work_record_id    TEXT    NOT NULL REFERENCES k12_creative_works(record_id) ON DELETE CASCADE,
    version_index     INTEGER NOT NULL,
    feedback_markdown TEXT    NOT NULL DEFAULT '',
    feedback_source   TEXT    NOT NULL DEFAULT '',
    feedback_skill    TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (work_record_id, version_index)
);

-- ========== 统一批改任务（§5.4 GradingJob；stage=status，幂等键=dedupe_key）==========
CREATE TABLE IF NOT EXISTS k12_grading_jobs (
    record_id              TEXT     PRIMARY KEY,
    agent_name             TEXT     NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    schema_version         INTEGER  NOT NULL DEFAULT 1,
    status                 TEXT     NOT NULL,
    submission_id          TEXT     NOT NULL DEFAULT '',
    source_kind            TEXT     NOT NULL DEFAULT '',
    idempotency_key        TEXT     NOT NULL DEFAULT '',
    confirmed_version      INTEGER  NOT NULL DEFAULT 0,
    confirmation_state     TEXT     NOT NULL DEFAULT '',
    anchor_state           TEXT     NOT NULL DEFAULT '',
    deadline               INTEGER  NOT NULL DEFAULT 0,
    model_snapshot_json    TEXT     NOT NULL DEFAULT '{}',
    stage_checkpoints_json TEXT     NOT NULL DEFAULT '',
    attempt_count          INTEGER  NOT NULL DEFAULT 0,
    failure_kind           TEXT     NOT NULL DEFAULT '',
    retryable              INTEGER  NOT NULL DEFAULT 0,
    failed_stage           TEXT     NOT NULL DEFAULT '',
    dedupe_key             TEXT     NOT NULL,
    tags_json              TEXT     NOT NULL DEFAULT '[]',
    due_at                 INTEGER,
    source_session_id      TEXT     NOT NULL DEFAULT '',
    version                INTEGER  NOT NULL DEFAULT 0,
    created_at             INTEGER  NOT NULL,
    updated_at             INTEGER  NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_gjobs_dedupe ON k12_grading_jobs(agent_name, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_k12_gjobs_scope ON k12_grading_jobs(agent_name, status);

-- ========== 版本化复习策略（§4.6：默认策略 v1 = 1/3/7/14 天，掌握 = 2 次独立做对且间隔 ≥3 天）==========
CREATE TABLE IF NOT EXISTS k12_review_policies (
    policy_version    TEXT    PRIMARY KEY,
    intervals_json    TEXT    NOT NULL,
    mastery_rule_json TEXT    NOT NULL,
    created_at        INTEGER NOT NULL
);
INSERT OR IGNORE INTO k12_review_policies (policy_version, intervals_json, mastery_rule_json, created_at)
VALUES ('v1', '[1,3,7,14]', '{"min_correct":2,"min_gap_days":3}', strftime('%s','now'));

-- ========== Transactional Outbox（§5.6 OutboxEvent / §6.15 单进程 dispatcher）==========
CREATE TABLE IF NOT EXISTS outbox_events (
    event_id        TEXT     PRIMARY KEY,
    agent_name      TEXT     NOT NULL DEFAULT '',
    aggregate_id    TEXT     NOT NULL DEFAULT '',
    event_type      TEXT     NOT NULL,
    payload_version INTEGER  NOT NULL DEFAULT 1,
    payload_json    TEXT     NOT NULL DEFAULT '{}',
    status          TEXT     NOT NULL DEFAULT 'pending',
    attempts        INTEGER  NOT NULL DEFAULT 0,
    last_error      TEXT     NOT NULL DEFAULT '',
    created_at      INTEGER  NOT NULL,
    updated_at      INTEGER  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_status ON outbox_events(status, created_at);

-- 消费者按 event_id 去重（§6.9：每个消费者以 event_id 去重，支持重放）
CREATE TABLE IF NOT EXISTS outbox_consumptions (
    consumer    TEXT    NOT NULL,
    event_id    TEXT    NOT NULL,
    consumed_at INTEGER NOT NULL,
    PRIMARY KEY (consumer, event_id)
);

-- ========== 数据迁移：agent_records → 专表（幂等 INSERT OR IGNORE）==========
INSERT OR IGNORE INTO k12_mistakes
    (record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
     source_session_id, version, created_at, updated_at,
     subject, question, knowledge_point, error_cause, wrong_process, canonical_answer,
     review_stage, last_retried_at, spot_check_state, entry_source)
SELECT record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
       source_session_id, version, created_at, updated_at,
       COALESCE(json_extract(fields_json,'$.subject'),''),
       COALESCE(json_extract(fields_json,'$.question'),''),
       COALESCE(json_extract(fields_json,'$.knowledge_point'),''),
       COALESCE(json_extract(fields_json,'$.error_cause'),''),
       COALESCE(json_extract(fields_json,'$.wrong_process'),''),
       COALESCE(json_extract(fields_json,'$.canonical_answer'),''),
       COALESCE(json_extract(fields_json,'$.review_stage'),0),
       COALESCE(json_extract(fields_json,'$.last_retried_at'),0),
       COALESCE(json_extract(fields_json,'$.spot_check_state'),''),
       COALESCE(json_extract(fields_json,'$.entry_source'),'')
FROM agent_records WHERE collection='错题本';

INSERT OR IGNORE INTO k12_accumulations
    (record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
     source_session_id, version, created_at, updated_at,
     subject, entry_type, content, source_ref, review_stage, last_retried_at)
SELECT record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
       source_session_id, version, created_at, updated_at,
       COALESCE(json_extract(fields_json,'$.subject'),''),
       COALESCE(json_extract(fields_json,'$.entry_type'),''),
       COALESCE(json_extract(fields_json,'$.content'),''),
       COALESCE(json_extract(fields_json,'$.source'),''),
       COALESCE(json_extract(fields_json,'$.review_stage'),0),
       COALESCE(json_extract(fields_json,'$.last_retried_at'),0)
FROM agent_records WHERE collection='积累本';

INSERT OR IGNORE INTO k12_practice_sets
    (record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
     source_session_id, version, created_at, updated_at,
     source_kind, title, paper_no, question_artifact_id, answer_artifact_id,
     skipped_blocked_count, finalized_at, finalized_via, reminder_sent_at,
     reminder_dismissed, closed_reason, delivery_status, delivery_target)
SELECT record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
       source_session_id, version, created_at, updated_at,
       COALESCE(json_extract(fields_json,'$.source_kind'),''),
       COALESCE(json_extract(fields_json,'$.title'),''),
       COALESCE(json_extract(fields_json,'$.paper_no'),''),
       COALESCE(json_extract(fields_json,'$.question_artifact_id'),''),
       COALESCE(json_extract(fields_json,'$.answer_artifact_id'),''),
       COALESCE(json_extract(fields_json,'$.skipped_blocked_count'),0),
       COALESCE(json_extract(fields_json,'$.finalized_at'),0),
       COALESCE(json_extract(fields_json,'$.finalized_via'),''),
       COALESCE(json_extract(fields_json,'$.reminder_sent_at'),0),
       COALESCE(json_extract(fields_json,'$.reminder_dismissed'),0),
       COALESCE(json_extract(fields_json,'$.closed_reason'),''),
       COALESCE(json_extract(fields_json,'$.delivery_status'),''),
       COALESCE(json_extract(fields_json,'$.delivery_target'),'')
FROM agent_records WHERE collection='练习集';

INSERT OR IGNORE INTO k12_practice_set_items
    (set_record_id, item_index, item_id, source_problem_id, subject, added_via,
     question_markdown, expected_answer_markdown, verification_status,
     verification_evidence, blocked_reason, paper_seq, returned, practice_problem_id, result_correct)
SELECT ar.record_id, je.key,
       COALESCE(json_extract(je.value,'$.item_id'),''),
       COALESCE(json_extract(je.value,'$.source_problem_id'),''),
       COALESCE(json_extract(je.value,'$.subject'),''),
       COALESCE(json_extract(je.value,'$.added_via'),''),
       COALESCE(json_extract(je.value,'$.question_markdown'),''),
       COALESCE(json_extract(je.value,'$.expected_answer_markdown'),''),
       COALESCE(json_extract(je.value,'$.verification_status'),''),
       COALESCE(json_extract(je.value,'$.verification_evidence'),''),
       COALESCE(json_extract(je.value,'$.blocked_reason'),''),
       COALESCE(json_extract(je.value,'$.paper_seq'),0),
       COALESCE(json_extract(je.value,'$.returned'),0),
       COALESCE(json_extract(je.value,'$.practice_problem_id'),''),
       json_extract(je.value,'$.result_correct')
FROM agent_records ar, json_each(ar.fields_json,'$.items') je
WHERE ar.collection='练习集';

INSERT OR IGNORE INTO k12_creative_works
    (record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
     source_session_id, version, created_at, updated_at,
     work_type, title, task, intent)
SELECT record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
       source_session_id, version, created_at, updated_at,
       COALESCE(json_extract(fields_json,'$.work_type'),''),
       COALESCE(json_extract(fields_json,'$.title'),''),
       COALESCE(json_extract(fields_json,'$.task'),''),
       COALESCE(json_extract(fields_json,'$.intent'),'')
FROM agent_records WHERE collection='作品';

INSERT OR IGNORE INTO k12_creative_work_versions
    (work_record_id, version_index, version_id, source_asset_id, content_markdown, practice_card_done_at)
SELECT ar.record_id, je.key,
       COALESCE(json_extract(je.value,'$.version_id'),''),
       COALESCE(json_extract(je.value,'$.source_asset_id'),''),
       COALESCE(json_extract(je.value,'$.content_markdown'),''),
       COALESCE(json_extract(je.value,'$.practice_card_done_at'),0)
FROM agent_records ar, json_each(ar.fields_json,'$.versions') je
WHERE ar.collection='作品';

INSERT OR IGNORE INTO k12_work_feedback
    (work_record_id, version_index, feedback_markdown, feedback_source, feedback_skill)
SELECT ar.record_id, je.key,
       COALESCE(json_extract(je.value,'$.feedback'),''),
       COALESCE(json_extract(je.value,'$.feedback_source'),''),
       COALESCE(json_extract(je.value,'$.feedback_skill'),'')
FROM agent_records ar, json_each(ar.fields_json,'$.versions') je
WHERE ar.collection='作品'
  AND (COALESCE(json_extract(je.value,'$.feedback'),'') != ''
    OR COALESCE(json_extract(je.value,'$.feedback_source'),'') != ''
    OR COALESCE(json_extract(je.value,'$.feedback_skill'),'') != '');

INSERT OR IGNORE INTO k12_grading_jobs
    (record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
     source_session_id, version, created_at, updated_at,
     submission_id, source_kind, idempotency_key, confirmed_version, confirmation_state,
     anchor_state, deadline, model_snapshot_json, stage_checkpoints_json,
     attempt_count, failure_kind, retryable, failed_stage)
SELECT record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
       source_session_id, version, created_at, updated_at,
       COALESCE(json_extract(fields_json,'$.submission_id'),''),
       COALESCE(json_extract(fields_json,'$.source_kind'),''),
       COALESCE(json_extract(fields_json,'$.idempotency_key'),''),
       COALESCE(json_extract(fields_json,'$.confirmed_version'),0),
       COALESCE(json_extract(fields_json,'$.confirmation_state'),''),
       COALESCE(json_extract(fields_json,'$.anchor_state'),''),
       COALESCE(json_extract(fields_json,'$.deadline'),0),
       COALESCE(json_extract(fields_json,'$.model_snapshot'),'{}'),
       COALESCE(json_extract(fields_json,'$.stage_checkpoints'),''),
       COALESCE(json_extract(fields_json,'$.attempt_count'),0),
       COALESCE(json_extract(fields_json,'$.failure_kind'),''),
       COALESCE(json_extract(fields_json,'$.retryable'),0),
       COALESCE(json_extract(fields_json,'$.failed_stage'),'')
FROM agent_records WHERE collection='批改任务';

-- ========== 一次切换（ADR-K12-013）：K12 collection 不再留在 agent_records ==========
DELETE FROM agent_records WHERE collection IN ('错题本','积累本','练习集','作品','批改任务');
`

// migrateCronV2 把 cron v1 schema 升级到 v2（详见 .claude/cron-script-compilation-design.md）。
//
// 用 Func 而非 SQL 是因为要先探测列，再在固定连接的事务中原地 DROP prompt。
// 禁止通过 DROP/RENAME 重建父表：foreign_keys=ON 时会级联删除 run/state。
//
// 流程：
//  1. cron_jobs 不存在 → CREATE canonical schema（兜底）
//  2. 原地补 spec_json/source_prompt/meta，再 ALTER DROP prompt
//  3. 原地补 cron_job_runs 执行结果列
//  4. 恢复父/历史索引，并在提交前跑 FK/integrity check
func migrateCronV2(ctx context.Context, db *sql.DB) error {
	return migrateCronV2InPlace(ctx, db)
}

// migrateK12PracticeCommandsV12 对 ADD COLUMN 逐列探测，使 V12 在半迁移/人工修复库上
// 也可安全重入；两张新表及索引继续使用 IF NOT EXISTS。
func migrateK12PracticeCommandsV12(ctx context.Context, db *sql.DB) error {
	for _, col := range []struct {
		name string
		def  string
	}{
		{"generation_job_id", "TEXT NOT NULL DEFAULT ''"},
		{"variant_index", "INTEGER NOT NULL DEFAULT 0"},
		{"requested_difficulty", "TEXT NOT NULL DEFAULT ''"},
		{"actual_difficulty", "TEXT NOT NULL DEFAULT ''"},
	} {
		has, err := columnExists(ctx, db, "k12_practice_set_items", col.name)
		if err != nil {
			return fmt.Errorf("检查 k12_practice_set_items.%s: %w", col.name, err)
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_practice_set_items ADD COLUMN %s %s`, col.name, col.def)); err != nil {
			return fmt.Errorf("新增 k12_practice_set_items.%s: %w", col.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, k12PracticeCommandsV12DDL); err != nil {
		return fmt.Errorf("创建 K12 组卷/回传表: %w", err)
	}
	return nil
}

func migrateK12StructuredFeedbackV14(ctx context.Context, db *sql.DB) error {
	has, err := columnExists(ctx, db, "k12_work_feedback", "structured_feedback_json")
	if err != nil {
		return fmt.Errorf("检查 k12_work_feedback.structured_feedback_json: %w", err)
	}
	if has {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE k12_work_feedback
        ADD COLUMN structured_feedback_json TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("新增 k12_work_feedback.structured_feedback_json: %w", err)
	}
	return nil
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
