// Package cron 提供定时任务调度
//
// 支持两种任务类型：
//   - 周期任务：标准 cron 表达式 + @every/@daily 等快捷方式
//   - 一次性任务：定时提醒，执行后自动删除
//
// 任务持久化到 SQLite，服务重启后自动恢复。
//
// v2 架构（参考 .claude/cron-script-compilation-design.md）：
// 创建任务时调用 LLM 一次性编译为可执行 Python 脚本（JobSpec），
// 运行时由 ScriptExecutor 在沙箱中执行，全程零 LLM 调用。
//
// 用法示例：
//
//	scheduler := cron.NewScheduler(db, compiler, scriptExec)
//	scheduler.Init(ctx)
//	scheduler.Start(ctx)
//	scheduler.AddJobFromPrompt(ctx, cron.AddJobRequest{
//	    Name:     "每日摘要",
//	    Schedule: "@daily",
//	    Prompt:   "总结今天的待办事项和重要邮件",
//	    UserID:   "user-1",
//	})
package cron

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/security"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// JobType 任务类型
type JobType string

const (
	JobTypeCron JobType = "cron" // 周期任务（cron 表达式）
	JobTypeOnce JobType = "once" // 一次性任务（定时提醒）
)

// JobStatus 任务状态
type JobStatus string

const (
	StatusActive JobStatus = "active" // 活跃
	StatusPaused JobStatus = "paused" // 暂停
	StatusDone   JobStatus = "done"   // 已完成（一次性任务执行后）
)

// Job 定时任务
//
// v2: 不再持有 Prompt 字段。SourcePrompt 是用户原始需求（仅供 UI 展示），
// Spec 是编译后的可执行规约（运行时由 ScriptExecutor 直接跑）。
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`        // 任务名称
	Type      JobType   `json:"type"`        // 任务类型
	Schedule  string    `json:"schedule"`    // cron 表达式或时间点
	UserID    string    `json:"user_id"`     // 所属用户
	Platform  string    `json:"platform"`    // 通知平台
	ChatID    string    `json:"chat_id"`     // 通知目标
	Status    JobStatus `json:"status"`      // 任务状态
	LastRunAt time.Time `json:"last_run_at"` // 上次执行时间
	NextRunAt time.Time `json:"next_run_at"` // 下次执行时间
	RunCount  int       `json:"run_count"`   // 已执行次数
	CreatedAt time.Time `json:"created_at"`

	// Spec 是 LLM 一次性编译的可执行规约（read-only 编译产物）。
	// 浅拷贝 *Job 时共享同一 *JobSpec 是安全的，因为执行流程不会写它。
	Spec         *JobSpec `json:"spec,omitempty"`
	SourcePrompt string   `json:"source_prompt"`
	// D4.2 多 deliver 桥接 — 通过 meta JSON 列持久化
	Deliver []string `json:"deliver,omitempty"`

	// 可靠性 & 安全扩展，全部走 meta JSON 列（与 Deliver 同款，免 schema migration）。
	TZ             string   `json:"tz,omitempty"`              // IANA 时区，"" → 服务器本地
	ContextFrom    []string `json:"context_from,omitempty"`    // 上游 jobID（线性任务链，读最近一条产物）
	FailureDeliver []string `json:"failure_deliver,omitempty"` // 失败投递目标（空则回落现有节流告警）
	RequireConfirm bool     `json:"require_confirm,omitempty"` // 危险动作人确认门（送达/发布/扣费）

	// H1 条件升级门（Hermes wakeAgent）：Starlark 门脚本每 tick 廉价跑，仅在调用
	// wake_agent() 时才升级到 Agent（花 token）。两字段走 meta JSON，免迁移。
	EscalateAgent  bool   `json:"escalate_agent,omitempty"`  // 允许 wake_agent 触发 Agent 升级
	EscalatePrompt string `json:"escalate_prompt,omitempty"` // 升级时跑的 Agent prompt（与门脚本同意图、独立）

	// §13.3(2) 增量发现：开启后，产物与上次相同则跳过投递（执行仍跑以取数比对）。
	OnlyIfChanged bool `json:"only_if_changed,omitempty"`

	// Continuous（持续型任务 + 跨 tick 检查点）：开启后（仅 agent 模式），每个 tick 不是从头重做整个
	// 目标，而是读上次进度档案(checkpoint，存 StateStore，重启不丢) → 只推进**一个增量** → 写回档案；
	// 目标完成 / 连续无新进展 / 达推进次数上限即自动收工。把长目标摊到多次廉价唤醒上累积推进——桌面版
	// 的"慢循环"（有界、带检查点、可自动终止），而非过夜死刷。
	Continuous bool `json:"continuous,omitempty"`

	// H4 不静默失效：运行时观测字段（仅内存，按 API 下发；重启后由下次运行重填）。
	// 执行失败 / 调度算不出 → LastError；投递失败 → LastDeliveryError。二者解耦，成功各自清零。
	LastError         string `json:"last_error,omitempty"`
	LastDeliveryError string `json:"last_delivery_error,omitempty"`
}

// JobSpec 编译后的可执行规约。一次编译，多次执行，全程零 LLM 调用。
type JobSpec struct {
	Runtime    string         `json:"runtime"`   // v1 固定 "python3"
	Script     string         `json:"script"`    // 完整 Python 源码
	Deps       []string       `json:"deps"`      // pip requirements，空则不创建 venv
	Inputs     map[string]any `json:"inputs"`    // 注入到沙箱 env 的 JSON（HEXCLAW_INPUTS）
	TimeoutSec int            `json:"timeout_s"` // 单次执行硬超时，0 → 默认 300s
	Compiled   CompileMeta    `json:"compiled"`
}

// CompileMeta 编译元信息，主要用于 venv 缓存键与可观测性。
type CompileMeta struct {
	Model     string    `json:"model"`
	At        time.Time `json:"at"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	Hash      string    `json:"hash"` // sha256(script + deps)，venv 缓存键
}

// AddJobRequest 业务路径添加任务的入参（POST /cron/jobs body）。
// AddJobFromPrompt 会用 Compiler 编译 Prompt → Spec 后入库。
type AddJobRequest struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	UserID   string `json:"user_id"`
	Platform string `json:"platform,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	// D4.2 多 deliver 桥接：chat / push / feishu / discord / wechat 任意组合
	// 留空 → 默认 ["chat"]（仅在 chat 流回写）
	Deliver []string `json:"deliver,omitempty"`
	// TimeoutSec optionally overrides the per-run execution budget. 0 → the
	// mode default (agent: defaultAgentTimeoutSec). Lets a multi-step agent
	// task (browse + reason + ingest) get more than the old fixed 300s.
	TimeoutSec int `json:"timeout_s,omitempty"`
	// Continuous 开启「持续型任务 + 跨 tick 检查点」：长目标分多次廉价唤醒累积推进（见 continuous.go）。
	// 持续任务每 tick 需推理"下一个增量"，故必为 agent 模式——置 true 时强制 agent 模式。
	Continuous      bool     `json:"continuous,omitempty"`
	AvailableSkills []string `json:"-"` // 服务端注入
	LocalAPIBase    string   `json:"-"` // 服务端注入
}

// defaultAgentTimeoutSec is the per-run budget for agent-mode jobs when the
// request does not override it. Raised from the original fixed 300s because a
// browse→reason→ingest round routinely needs more (BUG-20260613).
const defaultAgentTimeoutSec = 600

// agentTimeoutSec resolves the agent-mode per-run budget: an explicit positive
// override, else the default. Clamped to a sane ceiling to avoid a runaway job.
func agentTimeoutSec(override int) int {
	if override <= 0 {
		return defaultAgentTimeoutSec
	}
	if override > 1800 {
		return 1800
	}
	return override
}

// Scheduler 定时任务调度器
//
// v2：每次 tick 直接由 ScriptExecutor 跑 JobSpec.Script，全程零 LLM 调用。
// 创建任务时由 Compiler 一次性编译；后续运行只跑 Python 沙箱。
type Scheduler struct {
	mu         sync.RWMutex
	db         *sql.DB
	compiler   JobSpecCompiler
	scriptExec *ScriptExecutor
	engines    map[string]ScriptEngine // runtime -> engine (starlark default, python3 optional)
	agent      agentSupport
	jobs       map[string]*Job // id -> job
	stopCh     chan struct{}
	stopped    bool
	running    sync.Map // map[string]bool — 正在执行的任务 ID

	// H1 升级成本护栏：jobID → 上次 wake_agent 升级时刻。坏门每 tick 唤醒时，
	// 把 Agent 升级限制在最小间隔内（同 agent-mode 任务的 1h 下限），防刷 token。
	lastEscalated sync.Map // map[string]time.Time

	// §13.3(2) 增量发现：per-job 跨运行状态存储（state_get/set 与 only_if_changed 共用）。
	state StateStore

	// 全局并发上限 + 确定性错峰。
	// sem 满则派发 goroutine 阻塞等待空槽（防整点惊群打满上游/资源）。
	// nil → 不限（向后兼容，仅测试可能出现）。staggerMax>0 时按 jobID 哈希错开派发。
	sem        chan struct{}
	staggerMax time.Duration
}

// defaultMaxConcurrentRuns 是同时在执行的 cron 任务数上限。
// 整点 N 个任务到期时，最多 N 个并发，其余排队，避免惊群打满上游 API / CPU / 内存。
const defaultMaxConcurrentRuns = 8

// NewScheduler 创建调度器。
//
// compiler/scriptExec 可为 nil — 主要给测试使用（测试可只构造 prebuilt Job 不走编译）。
// 业务代码（cmd/hexclaw/main.go）必须注入两者，否则 AddJobFromPrompt / executeJob 会拒收。
func NewScheduler(db *sql.DB, compiler JobSpecCompiler, scriptExec *ScriptExecutor) *Scheduler {
	engines := map[string]ScriptEngine{
		RuntimeStarlark: NewStarlarkEngine(), // pure-Go default, always available
	}
	if scriptExec != nil {
		engines[RuntimePython3] = scriptExec // optional: only where python3 exists
	}
	s := &Scheduler{
		db:         db,
		compiler:   compiler,
		scriptExec: scriptExec,
		engines:    engines,
		jobs:       make(map[string]*Job),
		sem:        make(chan struct{}, defaultMaxConcurrentRuns),
	}
	// §13.3(2)：per-job 跨运行状态存储（需 DB）。注入 Starlark 引擎供 state_get/set，
	// scheduler 自身亦持有引用供 only_if_changed 投递门。
	if db != nil {
		store := newSQLStateStore(db)
		s.state = store
		if eng, ok := s.engines[RuntimeStarlark].(*StarlarkEngine); ok {
			eng.SetStateStore(store)
		}
	}
	return s
}

// SetMaxConcurrentRuns overrides the global concurrency cap. Must be
// called before Start. n<=0 disables the cap (unbounded — discouraged). Replacing
// the channel here is safe because dispatch goroutines are only spawned by Start.
func (s *Scheduler) SetMaxConcurrentRuns(n int) {
	if n <= 0 {
		s.sem = nil
		return
	}
	s.sem = make(chan struct{}, n)
}

// SetStaggerMax sets the maximum deterministic dispatch jitter. Each due
// job is delayed by a stable fnv(jobID)%max so a top-of-hour burst is spread out
// instead of firing simultaneously. 0 (default) disables staggering. Determinism
// (no random source) keeps runs reproducible and testable.
func (s *Scheduler) SetStaggerMax(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.staggerMax = d
}

// staggerFor returns a deterministic dispatch delay in [0, max) derived from the
// job ID via FNV-1a — same jobID always yields the same delay, no random source.
func staggerFor(jobID string, max time.Duration) time.Duration {
	if max <= 0 || jobID == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(jobID))
	return time.Duration(h.Sum64() % uint64(max))
}

// jobLocation resolves a job's IANA timezone. Empty or invalid
// falls back to server-local so a bad tz never silently shifts schedules to UTC.
func jobLocation(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	logger.Warn("[cron] 无效时区，回落服务器本地", "tz", tz)
	return time.Local
}

// SetKBIngest wires the in-process knowledge-base writer into the Starlark
// engine's kb_ingest builtin, so scripts ingest without an HTTP POST to the
// app's own API (which loopback SSRF blocking now forbids). No-op if the
// Starlark engine is absent — though NewScheduler always installs it.
func (s *Scheduler) SetKBIngest(fn KBIngestFunc) {
	if eng, ok := s.engines[RuntimeStarlark].(*StarlarkEngine); ok {
		eng.SetKBIngest(fn)
	}
}

// engineFor resolves the script engine for a runtime, or nil for an unknown one.
// Returning nil (rather than silently falling back to Starlark) lets executeJob
// surface an explicit "no engine for runtime X" error instead of running, say, a
// python script on the Starlark engine and reporting a misleading syntax error.
func (s *Scheduler) engineFor(runtime string) ScriptEngine {
	return s.engines[runtime]
}

// looksLikeStarlark reports whether a script calls Starlark host builtins that the
// python3 runtime does not define (emit / kb_ingest / http_get / ...). Used as a
// backstop to re-route scripts mis-tagged runtime "python3" to the Starlark engine.
func looksLikeStarlark(script string) bool {
	for _, b := range []string{"emit(", "kb_ingest(", "http_get(", "http_post(", "json_decode(", "json_encode("} {
		if strings.Contains(script, b) {
			return true
		}
	}
	return false
}

// Init 初始化调度器存储表
//
// v2 schema：cron_jobs 不再含 prompt 列。旧库由 Sprint 1.2 的 migration 处理。
func (s *Scheduler) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL,
		spec_json TEXT NOT NULL DEFAULT '',
		source_prompt TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL,
		platform TEXT DEFAULT '',
		chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		meta TEXT NOT NULL DEFAULT '{}'
	)`)
	if err != nil {
		return fmt.Errorf("初始化 cron 表失败: %w", err)
	}

	// 执行历史表
	_, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_job_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'success',
		result TEXT DEFAULT '',
		error TEXT DEFAULT '',
		duration_ms INTEGER DEFAULT 0,
		run_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
	)`)
	if err != nil {
		return fmt.Errorf("初始化 cron 历史表失败: %w", err)
	}

	// 兼容升级：为旧表添加 result 列（已存在时返回 duplicate column，是预期）
	addColumnExpectDuplicate(ctx, s.db, "cron_job_runs", `ALTER TABLE cron_job_runs ADD COLUMN result TEXT DEFAULT ''`)

	// §13.3(2) 增量发现：per-job 跨运行状态表（state_get/set + only_if_changed 哈希）。
	if _, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_job_state (
		job_id TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL DEFAULT '',
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (job_id, key),
		FOREIGN KEY (job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
	)`); err != nil {
		return fmt.Errorf("初始化 cron 状态表失败: %w", err)
	}

	// v2 migration：旧库可能缺 spec_json / source_prompt 列。
	// ALTER TABLE ADD COLUMN 在列已存在时报 "duplicate column name"，预期忽略；其他 error 必须 log。
	addColumnExpectDuplicate(ctx, s.db, "cron_jobs", `ALTER TABLE cron_jobs ADD COLUMN spec_json TEXT NOT NULL DEFAULT ''`)
	addColumnExpectDuplicate(ctx, s.db, "cron_jobs", `ALTER TABLE cron_jobs ADD COLUMN source_prompt TEXT NOT NULL DEFAULT ''`)
	// D4.2 多 deliver 桥接：meta JSON 列承载 deliver 数组
	addColumnExpectDuplicate(ctx, s.db, "cron_jobs", `ALTER TABLE cron_jobs ADD COLUMN meta TEXT NOT NULL DEFAULT '{}'`)

	// 旧库的 prompt 列带 NOT NULL 约束，v2 INSERT 不写它就触发约束失败。
	// SQLite ALTER DROP COLUMN 在带索引/约束的列上可能静默失败，因此用表重建
	// 兜底：检测到 prompt 列存在 → 重建 cron_jobs（v1 旧任务已被 detect 清理）。
	if err := s.rebuildCronJobsIfLegacy(ctx); err != nil {
		return fmt.Errorf("v2 migration 重建 cron_jobs 失败: %w", err)
	}

	// v2 history migration：为脚本执行新增 stdout/stderr/exit_code/data_json 列
	addColumnExpectDuplicate(ctx, s.db, "cron_job_runs", `ALTER TABLE cron_job_runs ADD COLUMN stdout TEXT NOT NULL DEFAULT ''`)
	addColumnExpectDuplicate(ctx, s.db, "cron_job_runs", `ALTER TABLE cron_job_runs ADD COLUMN stderr TEXT NOT NULL DEFAULT ''`)
	addColumnExpectDuplicate(ctx, s.db, "cron_job_runs", `ALTER TABLE cron_job_runs ADD COLUMN exit_code INTEGER NOT NULL DEFAULT 0`)
	addColumnExpectDuplicate(ctx, s.db, "cron_job_runs", `ALTER TABLE cron_job_runs ADD COLUMN data_json TEXT NOT NULL DEFAULT ''`)

	// (job_id, id) index backs the retention prune's "newest N ids per job"
	// subquery — id is insertion-ordered, so this avoids a DATETIME sort
	// (review H2: history was pruned per-write but lacked the supporting index).
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_cron_job_runs_job_id ON cron_job_runs(job_id, id)`); err != nil {
		return fmt.Errorf("创建 cron_job_runs 索引失败: %w", err)
	}

	// 清理 v1 遗留任务（只有 prompt 无 spec_json）—— 不自动编译，提示用户重建
	if err := s.detectAndCleanupLegacyJobs(ctx); err != nil {
		return fmt.Errorf("清理 v1 遗留任务失败: %w", err)
	}

	// 加载所有活跃任务
	return s.loadJobs(ctx)
}

// rebuildCronJobsIfLegacy 若 cron_jobs 表含 v1 遗留的 prompt 列，做表重建：
//   - CREATE TABLE cron_jobs_v2 with the canonical v2 schema
//   - 拷贝 spec_json 非空的 v2 任务（v1 任务在 detectAndCleanupLegacyJobs 阶段已被 DELETE）
//   - DROP TABLE cron_jobs; ALTER RENAME cron_jobs_v2 → cron_jobs
//   - 重建索引
//
// 全程在 transaction 内，失败 ROLLBACK。
func (s *Scheduler) rebuildCronJobsIfLegacy(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(cron_jobs)`)
	if err != nil {
		return err
	}
	hasPrompt := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "prompt" {
			hasPrompt = true
		}
	}
	rows.Close()
	if !hasPrompt {
		return nil
	}

	logger.Warn("[cron] 检测到 v1 遗留 prompt 列，执行 cron_jobs 表重建迁移")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }

	if _, err := tx.ExecContext(ctx, `CREATE TABLE cron_jobs_v2 (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL,
		spec_json TEXT NOT NULL DEFAULT '',
		source_prompt TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL,
		platform TEXT DEFAULT '',
		chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		meta TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		rollback()
		return fmt.Errorf("CREATE cron_jobs_v2: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO cron_jobs_v2
		(id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status, last_run_at, next_run_at, run_count, created_at, meta)
		SELECT id, name, type, schedule, spec_json, source_prompt,
		       user_id, COALESCE(platform,''), COALESCE(chat_id,''), status, last_run_at, next_run_at,
		       COALESCE(run_count,0), created_at, '{}'
		FROM cron_jobs
		WHERE spec_json IS NOT NULL AND spec_json != ''`); err != nil {
		rollback()
		return fmt.Errorf("拷贝 v2 任务: %w", err)
	}

	for _, ddl := range []string{
		`DROP TABLE cron_jobs`,
		`ALTER TABLE cron_jobs_v2 RENAME TO cron_jobs`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			rollback()
			return fmt.Errorf("%s: %w", ddl, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	logger.Warn("[cron] cron_jobs 表重建完成 — v1 schema 已清退")
	return nil
}

// detectAndCleanupLegacyJobs 启动时清理 v1 遗留任务。
//
// 判定：spec_json 为空 → 该任务未经 v2 编译，运行时不知道怎么执行。
// 不自动编译（避免启动期 LLM 调用 + 旧 prompt 可能已过时）。
// 直接 DELETE 并通过日志告知用户在 UI 重建。
func (s *Scheduler) detectAndCleanupLegacyJobs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name FROM cron_jobs WHERE spec_json IS NULL OR spec_json = ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type legacy struct{ id, name string }
	var legs []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.name); err != nil {
			return err
		}
		legs = append(legs, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(legs) == 0 {
		return nil
	}

	for _, l := range legs {
		logger.Warn("[cron] 清理 v1 遗留任务 — 请在 UI 重新创建",
			"id", l.id, "name", l.name)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cron_jobs WHERE spec_json IS NULL OR spec_json = ''`); err != nil {
		return fmt.Errorf("DELETE legacy jobs: %w", err)
	}
	logger.Warn("[cron] v1 遗留任务已清理", "count", len(legs))
	return nil
}

// Start 启动调度器
//
// v2: 不再需要 executor callback。每次 tick 由内部 ScriptExecutor 直接执行 JobSpec。
// 调度器每秒检查是否有任务需要执行。
func (s *Scheduler) Start(_ context.Context) {
	s.mu.Lock()
	s.stopCh = make(chan struct{})
	s.stopped = false
	s.mu.Unlock()

	go s.runLoop()
	logger.Info("Cron 调度器已启动")
}

// Stop 停止调度器
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
		logger.Info("Cron 调度器已停止")
	}
}

// AddJob 添加一个 prebuilt Job（带 Spec）到调度器。
//
// 用途：
//   - 测试 / 内部 loadJobs / 从已编译 Spec 直接构造（如导入）
//   - 业务路径（POST /cron/jobs）请用 AddJobFromPrompt，它会先编译再调本方法
//
// 必须满足 Job.Spec != nil；nil Spec 在 v2 运行时无法被 executeJob 处理。
func (s *Scheduler) AddJob(ctx context.Context, job *Job) error {
	if job.Spec == nil {
		return fmt.Errorf("Job.Spec 缺失 — 业务路径请用 AddJobFromPrompt")
	}
	if job.ID == "" {
		job.ID = "cron-" + idgen.ShortID()
	}
	if job.Type == "" {
		job.Type = JobTypeCron
	}
	if job.Status == "" {
		job.Status = StatusActive
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}

	// 计算下次执行时间（在任务时区里求 @daily/@hourly/cron 的下一刻）
	next, err := nextRunTime(job.Schedule, job.Type, time.Now().In(jobLocation(job.TZ)))
	if err != nil {
		return fmt.Errorf("无效的调度表达式 %q: %w", job.Schedule, err)
	}
	job.NextRunAt = next

	// 序列化 Spec
	b, err := json.Marshal(job.Spec)
	if err != nil {
		return fmt.Errorf("序列化 Spec 失败: %w", err)
	}
	specJSON := string(b)

	// D4.2 deliver 持久化到 meta JSON 列（avoid schema migration）
	metaJSON := serializeJobMeta(job)

	// 持久化
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status, next_run_at, created_at, meta)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Type, job.Schedule, specJSON, job.SourcePrompt,
		job.UserID, job.Platform, job.ChatID, job.Status, job.NextRunAt, job.CreatedAt, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("保存任务失败: %w", err)
	}

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	logger.Info("Cron 任务已添加", "name", job.Name, "schedule", job.Schedule, "下次执行", job.NextRunAt.Format(time.RFC3339))
	return nil
}

// AddJobFromPrompt 业务入口（同步）：用 Compiler 把 prompt 编译成 JobSpec 再 AddJob。
//
// 编译失败 / 校验失败 → 拒绝入库，返回详细错误（含 LLM 原文，便于用户调 prompt 重试）。
// 等价于 AddJobFromPromptWithProgress(ctx, req, nil)。
func (s *Scheduler) AddJobFromPrompt(ctx context.Context, req AddJobRequest) (*Job, error) {
	return s.AddJobFromPromptWithProgress(ctx, req, nil)
}

// AddJobFromPromptWithProgress 同 AddJobFromPrompt 但每个阶段通过 onProgress 推送事件。
//
// 阶段顺序：
//
//	analyzing  → Compiler.CompileWithProgress（内部 emit calling_llm / validating）
//	persisting → INSERT cron_jobs + 加入内存 map
//
// onProgress 为 nil 时静默执行。callback 同步触发 — handler 层应 flush HTTP response。
func (s *Scheduler) AddJobFromPromptWithProgress(
	ctx context.Context,
	req AddJobRequest,
	onProgress ProgressFunc,
) (*Job, error) {
	if s.compiler == nil {
		return nil, fmt.Errorf("compiler 未注入 — 调度器初始化错")
	}
	if strings.TrimSpace(req.Schedule) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("schedule 与 prompt 必填")
	}
	// 注入扫描：任务创建期严格扫（指令覆盖 / 凭证外泄 / 零宽混淆）。纵深防御一层，
	// 主防御仍是 stripTools + 权限闸；这里只快速拦明显恶意的任务 prompt。
	if err := security.ScanUserPrompt(req.Prompt); err != nil {
		return nil, fmt.Errorf("任务 prompt 未通过安全扫描: %w", err)
	}

	emit := func(stage ProgressStage, msg string) {
		if onProgress != nil {
			onProgress(CompileProgress{Stage: stage, Message: msg})
		}
	}

	emit(StageAnalyzing, "解析需求并选择 LLM provider…")

	// Dual-mode classification: cognitive tasks are marked for agent execution
	// (no script compilation); mechanical I/O tasks go through script compile.
	// 持续型任务每 tick 都要推理"下一个增量"，必为 agent 模式 → req.Continuous 强制走 agent 分支。
	if req.Continuous || ClassifyTaskMode(req.Prompt) == RuntimeAgent {
		if err := validateAgentModeSchedule(req.Schedule); err != nil {
			return nil, err
		}
		emit(StagePersisting, "保存任务（AI 推理模式）…")
		job := &Job{
			Name:         req.Name,
			Type:         JobTypeCron,
			Schedule:     req.Schedule,
			UserID:       req.UserID,
			Platform:     req.Platform,
			ChatID:       req.ChatID,
			Status:       StatusActive,
			SourcePrompt: req.Prompt,
			Spec:         &JobSpec{Runtime: RuntimeAgent, TimeoutSec: agentTimeoutSec(req.TimeoutSec)},
			Deliver:      req.Deliver,
			Continuous:   req.Continuous,
		}
		if err := s.AddJob(ctx, job); err != nil {
			return nil, err
		}
		return job, nil
	}

	hints := CompileHints{
		AvailableSkills: req.AvailableSkills,
		LocalAPIBase:    req.LocalAPIBase,
		UserID:          req.UserID,
	}

	var spec *JobSpec
	var err error
	if pc, ok := s.compiler.(ProgressCompiler); ok {
		spec, err = pc.CompileWithProgress(ctx, req.Prompt, hints, onProgress)
	} else {
		// Compiler 不支持进度反馈 — emit 一个 calling_llm 占位，保持前端 UX 一致
		emit(StageCallingLLM, "调用 LLM 生成脚本…")
		spec, err = s.compiler.Compile(ctx, req.Prompt, hints)
	}
	if err != nil {
		return nil, fmt.Errorf("编译失败: %w", err)
	}

	emit(StagePersisting, "保存任务…")
	job := &Job{
		Name:         req.Name,
		Type:         JobTypeCron,
		Schedule:     req.Schedule,
		UserID:       req.UserID,
		Platform:     req.Platform,
		ChatID:       req.ChatID,
		Status:       StatusActive,
		SourcePrompt: req.Prompt,
		Spec:         spec,
		Deliver:      req.Deliver, // D4.2 多 deliver 桥接 — 持久化到 meta JSON 列
	}
	if err := s.AddJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// AddJobFromScript admits a pre-authored, deterministic script as a first-class
// script-mode job, bypassing LLM compilation entirely. This is the reliable path
// for mechanical fetch->parse->store jobs that a weak compile model keeps failing
// to emit (BUG-20260615): the caller supplies a vetted stdlib script, it runs
// through the same validateSpec chain (syntax + AST allowlist + output contract)
// the compiler output goes through, and then executes with zero LLM at tick time.
func (s *Scheduler) AddJobFromScript(ctx context.Context, req AddJobRequest, runtime, script string) (*Job, error) {
	if strings.TrimSpace(req.Schedule) == "" {
		return nil, fmt.Errorf("schedule is required")
	}
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("script must not be empty")
	}
	if runtime == "" {
		runtime = RuntimeStarlark // pure-Go default: no runtime dependency
	}
	engine, ok := s.engines[runtime]
	if !ok {
		return nil, fmt.Errorf("unknown or unavailable runtime %q", runtime)
	}
	if !engine.Available() {
		return nil, fmt.Errorf("%s runtime is not available on this host", engine.Name())
	}
	if err := engine.Validate(script); err != nil {
		return nil, fmt.Errorf("script validation failed: %w", err)
	}
	spec := &JobSpec{
		Runtime:    engine.Name(),
		Script:     script,
		TimeoutSec: req.TimeoutSec, // 0 -> executor's effective default
	}
	job := &Job{
		Name:         req.Name,
		Type:         JobTypeCron,
		Schedule:     req.Schedule,
		UserID:       req.UserID,
		Platform:     req.Platform,
		ChatID:       req.ChatID,
		Status:       StatusActive,
		SourcePrompt: req.Prompt,
		Spec:         spec,
		Deliver:      req.Deliver,
	}
	if err := s.AddJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// RemoveJob 删除任务
func (s *Scheduler) RemoveJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id = ?`, jobID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.jobs, jobID)
	s.mu.Unlock()
	// Drop heal-quota / notification bookkeeping so the maps cannot grow
	// unboundedly across job churn (review L5).
	s.pruneAgentState(jobID)
	return nil
}

// PauseJob 暂停任务
func (s *Scheduler) PauseJob(ctx context.Context, jobID string) error {
	return s.updateJobStatus(ctx, jobID, StatusPaused)
}

// ResumeJob 恢复任务
func (s *Scheduler) ResumeJob(ctx context.Context, jobID string) error {
	return s.updateJobStatus(ctx, jobID, StatusActive)
}

// ListJobs 列出所有任务
func (s *Scheduler) ListJobs(ctx context.Context, userID string) ([]*Job, error) {
	query := `SELECT id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status,
	          last_run_at, next_run_at, run_count, created_at, meta
	          FROM cron_jobs WHERE user_id = ? ORDER BY next_run_at`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// scanJobRow 把一条 cron_jobs 记录扫描成 *Job，集中处理 Spec JSON 反序列化与 NULL 时间。
//
// 期望列顺序：id, name, type, schedule, spec_json, source_prompt, user_id,
//
//	platform, chat_id, status, last_run_at, next_run_at, run_count, created_at
func scanJobRow(row interface {
	Scan(dest ...any) error
}) (*Job, error) {
	job := &Job{}
	var (
		specJSON  sql.NullString
		srcPrompt sql.NullString
		lastRun   sql.NullTime
		meta      sql.NullString
	)
	if err := row.Scan(
		&job.ID, &job.Name, &job.Type, &job.Schedule,
		&specJSON, &srcPrompt,
		&job.UserID, &job.Platform, &job.ChatID, &job.Status,
		&lastRun, &job.NextRunAt, &job.RunCount, &job.CreatedAt, &meta,
	); err != nil {
		return nil, err
	}
	if meta.Valid {
		parseJobMeta(job, meta.String) // restore Deliver and other extended metadata
	}
	if lastRun.Valid {
		job.LastRunAt = lastRun.Time
	}
	if srcPrompt.Valid {
		job.SourcePrompt = srcPrompt.String
	}
	if specJSON.Valid && specJSON.String != "" {
		var spec JobSpec
		if err := json.Unmarshal([]byte(specJSON.String), &spec); err != nil {
			return nil, fmt.Errorf("反序列化 Job.Spec 失败 (id=%s): %w", job.ID, err)
		}
		job.Spec = &spec
	}
	return job, nil
}

// serializeJobMeta 把 Job 的扩展元数据（D4.2 deliver 等）序列化到 meta JSON 列。
// 失败时返回 "{}" 不阻断入库（meta 是元数据，缺失不致命）。
func serializeJobMeta(job *Job) string {
	m := map[string]any{}
	if len(job.Deliver) > 0 {
		m["deliver"] = job.Deliver
	}
	// 可靠性 & 安全字段同走 meta JSON 列，免 schema migration。
	if job.TZ != "" {
		m["tz"] = job.TZ
	}
	if len(job.ContextFrom) > 0 {
		m["context_from"] = job.ContextFrom
	}
	if len(job.FailureDeliver) > 0 {
		m["failure_deliver"] = job.FailureDeliver
	}
	if job.RequireConfirm {
		m["require_confirm"] = true
	}
	if job.EscalateAgent {
		m["escalate_agent"] = true
	}
	if job.EscalatePrompt != "" {
		m["escalate_prompt"] = job.EscalatePrompt
	}
	if job.OnlyIfChanged {
		m["only_if_changed"] = true
	}
	if job.Continuous {
		m["continuous"] = true
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseJobMeta 从 meta JSON 反序列化扩展元数据回填到 job。
// 任何错误都静默吞掉（meta 缺失不致命，回填空 deliver 即"默认 chat"）。
func parseJobMeta(job *Job, metaJSON string) {
	if job == nil || strings.TrimSpace(metaJSON) == "" {
		return
	}
	var m struct {
		Deliver        []string `json:"deliver,omitempty"`
		TZ             string   `json:"tz,omitempty"`
		ContextFrom    []string `json:"context_from,omitempty"`
		FailureDeliver []string `json:"failure_deliver,omitempty"`
		RequireConfirm bool     `json:"require_confirm,omitempty"`
		EscalateAgent  bool     `json:"escalate_agent,omitempty"`
		EscalatePrompt string   `json:"escalate_prompt,omitempty"`
		OnlyIfChanged  bool     `json:"only_if_changed,omitempty"`
		Continuous     bool     `json:"continuous,omitempty"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		return
	}
	if len(m.Deliver) > 0 {
		job.Deliver = m.Deliver
	}
	job.TZ = m.TZ
	job.ContextFrom = m.ContextFrom
	job.FailureDeliver = m.FailureDeliver
	job.RequireConfirm = m.RequireConfirm
	job.EscalateAgent = m.EscalateAgent
	job.EscalatePrompt = m.EscalatePrompt
	job.OnlyIfChanged = m.OnlyIfChanged
	job.Continuous = m.Continuous
}

// latestRunOutput returns the most recent run's product for jobID (for
// context_from injection): stdout, or data_json when stdout is empty. Empty
// string on any miss (no DB / no rows / error) so a broken upstream never blocks
// the consumer.
func (s *Scheduler) latestRunOutput(jobID string) string {
	if s.db == nil || jobID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, dataJSON sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stdout, data_json FROM cron_job_runs WHERE job_id = ? ORDER BY id DESC LIMIT 1`,
		jobID).Scan(&stdout, &dataJSON)
	if err != nil {
		return ""
	}
	if stdout.Valid && strings.TrimSpace(stdout.String) != "" {
		return stdout.String
	}
	if dataJSON.Valid {
		return dataJSON.String
	}
	return ""
}

// withUpstreamContext returns a per-run copy of job with the upstream product
// injected — Spec.Inputs["upstream_context"] for scripts (surfaced as
// HEXCLAW_INPUTS) and a SourcePrompt prefix for agent jobs. It deep-copies Spec
// and its Inputs map so the shared read-only *JobSpec is never mutated; the
// original s.jobs[id] is untouched.
func withUpstreamContext(job *Job, upstream string) *Job {
	cp := *job
	if job.Spec != nil {
		specCopy := *job.Spec
		inputs := make(map[string]any, len(job.Spec.Inputs)+1)
		for k, v := range job.Spec.Inputs {
			inputs[k] = v
		}
		inputs["upstream_context"] = upstream
		specCopy.Inputs = inputs
		cp.Spec = &specCopy
	}
	if cp.SourcePrompt != "" {
		cp.SourcePrompt = "【上游任务最近产物】\n" + upstream + "\n\n" + cp.SourcePrompt
	}
	return &cp
}

// EffectiveDeliver 返回 job 实际应通知的渠道列表。空时回退 ["chat"]。
// scheduler.executeJob 执行完成后用此函数路由结果到对应 IM/通知适配器。
func EffectiveDeliver(job *Job) []string {
	if job == nil || len(job.Deliver) == 0 {
		return []string{"chat"}
	}
	return job.Deliver
}

// GetJob 获取单个任务
func (s *Scheduler) GetJob(_ context.Context, jobID string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[jobID]
	return job, ok
}

// TriggerJob 手动触发任务执行（fire-and-forget，不等结果）。
//
// v2：不依赖 executor callback，直接 dispatch 到 executeJob，由 ScriptExecutor 跑沙箱。
func (s *Scheduler) TriggerJob(_ context.Context, jobID string) error {
	s.mu.RLock()
	job, ok := s.jobs[jobID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("任务 %q 不存在", jobID)
	}
	// Agent-mode jobs run via the injected AgentRunner, not the script executor,
	// so only script-mode jobs need scriptExec — don't block a webhook-triggered
	// agent job on it (an agent-only deployment may legitimately have no exec).
	if s.scriptExec == nil && (job.Spec == nil || job.Spec.Runtime != RuntimeAgent) {
		return fmt.Errorf("脚本执行器未就绪")
	}

	// 复制一份避免并发修改（Spec 是 read-only pointer，共享安全）
	j := *job
	go s.executeJob(&j)
	return nil
}

// JobHistory 执行历史记录
//
// v2: 新增 Stdout/Stderr/ExitCode/Data 字段记录脚本沙箱执行结果。
// 旧 Result 字段保留兼容（v1 LLM 模式或 v2 渠道通知摘要）。
type JobHistory struct {
	ID         int64     `json:"id"`
	JobID      string    `json:"job_id"`
	Status     string    `json:"status"` // success / failed / timeout
	Result     string    `json:"result,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	RunAt      time.Time `json:"run_at"`

	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Data     any    `json:"data,omitempty"`
}

// GetJobHistory 获取任务执行历史（最近 50 条，从新到旧）
func (s *Scheduler) GetJobHistory(ctx context.Context, jobID string, limit ...int) ([]JobHistory, error) {
	s.mu.RLock()
	_, ok := s.jobs[jobID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("任务 %q 不存在", jobID)
	}

	// limit 可选：缺省 50，传入 >0 时生效并夹到 [1,200]（bug 2026-06-22：前端 ?limit 此前被忽略）。
	n := 50
	if len(limit) > 0 && limit[0] > 0 {
		n = min(limit[0], 200)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, status, COALESCE(result,''), error, duration_ms, run_at,
		        COALESCE(stdout,''), COALESCE(stderr,''), COALESCE(exit_code,0), COALESCE(data_json,'')
		 FROM cron_job_runs WHERE job_id = ? ORDER BY run_at DESC LIMIT ?`, jobID, n)
	if err != nil {
		return nil, fmt.Errorf("查询执行历史失败: %w", err)
	}
	defer rows.Close()

	var history []JobHistory
	for rows.Next() {
		var h JobHistory
		var dataJSON string
		if err := rows.Scan(&h.ID, &h.JobID, &h.Status, &h.Result, &h.Error, &h.DurationMs, &h.RunAt,
			&h.Stdout, &h.Stderr, &h.ExitCode, &dataJSON); err != nil {
			return nil, fmt.Errorf("读取历史记录失败: %w", err)
		}
		if dataJSON != "" {
			var parsed any
			if err := json.Unmarshal([]byte(dataJSON), &parsed); err == nil {
				h.Data = parsed
			}
		}
		history = append(history, h)
	}
	return history, rows.Err()
}

// persistHistory 写入一条执行历史（v2 脚本执行 + v1 LLM 模式共用）。
//
// runResult 可为 nil（兼容旧调用方），此时只写 Status/Result/Error/DurationMs。
func (s *Scheduler) persistHistory(ctx context.Context, jobID, status, result, errMsg string, durationMs int64, runAt time.Time, stdout, stderr string, exitCode int, data any) error {
	var dataJSON string
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("序列化 data 失败: %w", err)
		}
		dataJSON = string(b)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO cron_job_runs (job_id, status, result, error, duration_ms, run_at, stdout, stderr, exit_code, data_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, status, result, errMsg, durationMs, runAt, stdout, stderr, exitCode, dataJSON); err != nil {
		return err
	}
	// Retention: a long-lived job ticks forever, so without a cap cron_job_runs
	// grows unbounded. Keep only the most recent maxHistoryRowsPerJob rows per
	// job (review H2). Pruning on every write keeps the table bounded with no
	// background sweeper; a prune failure is non-fatal — the row is already
	// persisted and the next write retries the trim.
	s.pruneHistory(ctx, jobID)
	return nil
}

// maxHistoryRowsPerJob caps retained execution-history rows per job. The cron
// UI shows the latest 50 (see GetJobHistory); 200 keeps comfortable headroom
// for the self-heal window scan while bounding growth.
const maxHistoryRowsPerJob = 200

// pruneHistory deletes all but the most recent maxHistoryRowsPerJob runs of a
// job. id is insertion-ordered (AUTOINCREMENT), so "newest" is "highest id".
func (s *Scheduler) pruneHistory(ctx context.Context, jobID string) {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cron_job_runs
		 WHERE job_id = ?
		   AND id NOT IN (
		       SELECT id FROM cron_job_runs WHERE job_id = ? ORDER BY id DESC LIMIT ?
		   )`,
		jobID, jobID, maxHistoryRowsPerJob); err != nil {
		slog.Warn("[cron] history prune failed (non-fatal)", "source", "cron", "id", jobID, "err", err.Error())
	}
}

// --- 内部方法 ---

// runLoop 调度主循环
func (s *Scheduler) runLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			s.checkAndExecute(now)
		}
	}
}

// checkAndExecute 检查并执行到期任务
func (s *Scheduler) checkAndExecute(now time.Time) {
	s.mu.RLock()
	var dueJobs []*Job
	for _, job := range s.jobs {
		if job.Status == StatusActive && !job.NextRunAt.After(now) {
			// 复制一份避免并发修改
			j := *job
			dueJobs = append(dueJobs, &j)
		}
	}
	s.mu.RUnlock()

	for _, job := range dueJobs {
		// A slow job stays "due" every tick; skip spawning before we even
		// take a goroutine/sem slot, so a long run doesn't churn goroutines or
		// starve the semaphore. executeJob's running.LoadOrStore stays the
		// authoritative guard (the Load→spawn race is benign — at worst one extra
		// goroutine hits that guard and returns).
		if _, busy := s.running.Load(job.ID); busy {
			continue
		}
		// H2 misfire 宽限：停服/休眠后重启，错过本次到期超过半周期（clamp[120s,2h]）→
		// 快进到下个未来点、跳过本次，**不补一串积压**；未超宽限则正常补跑一次。
		// 一次性任务不参与（错过的提醒仍应迟到触发）。
		if job.Type != JobTypeOnce && now.Sub(job.NextRunAt) > graceFor(job.Schedule, job.Type, job.TZ) {
			s.fastForward(job, now)
			continue
		}
		delay := staggerFor(job.ID, s.staggerMax)
		go func(j *Job) {
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-s.stopCh:
					timer.Stop()
					return
				}
			}
			// global concurrency cap: block for a free slot (or bail on stop).
			if s.sem != nil {
				select {
				case s.sem <- struct{}{}:
					defer func() { <-s.sem }()
				case <-s.stopCh:
					return
				}
			}
			s.executeJob(j)
		}(job)
	}
}

// graceFor 返回任务的 misfire 宽限窗 = 半周期，clamp 到 [120s, 2h]（对标 Hermes
// _compute_grace_seconds）。周期由"下个 occurrence − 基准"推出，免解析 cron 表达式；
// 算不出周期（坏 schedule）时返上限宽限，保守避免误快进。
func graceFor(schedule string, jobType JobType, tz string) time.Duration {
	loc := jobLocation(tz)
	// 周期 = 连续两个 occurrence 的间隔（而非"距下次的剩余时间"，后者在整点附近会
	// 退化为很小的值）。t1 = 下个 occurrence，t2 = 再下个，period = t2 - t1。
	t1, err := nextRunTime(schedule, jobType, time.Now().In(loc))
	if err != nil {
		return 2 * time.Hour
	}
	t2, err := nextRunTime(schedule, jobType, t1)
	if err != nil {
		return 2 * time.Hour
	}
	grace := t2.Sub(t1) / 2
	if grace < 2*time.Minute {
		return 2 * time.Minute
	}
	if grace > 2*time.Hour {
		return 2 * time.Hour
	}
	return grace
}

// fastForward 把超宽限的 misfire 任务推进到下个未来执行点（跳过积压、不补一串），
// loud log + 同步更新内存与 DB 的 next_run_at；保持任务 active。
func (s *Scheduler) fastForward(job *Job, now time.Time) {
	next, err := nextRunTime(job.Schedule, job.Type, now.In(jobLocation(job.TZ)))
	if err != nil {
		slog.Error("[cron] misfire 快进失败：next 算不出，保持原 next 不补跑",
			"source", "cron", "id", job.ID, "schedule", job.Schedule, "err", err.Error())
		return
	}
	slog.Warn("[cron] misfire 超宽限：快进跳过本次，不补积压",
		"source", "cron", "id", job.ID, "name", job.Name,
		"missed_by", now.Sub(job.NextRunAt).String(), "new_next", next.Format(time.RFC3339))
	s.mu.Lock()
	if j, ok := s.jobs[job.ID]; ok {
		j.NextRunAt = next
	}
	s.mu.Unlock()
	if s.db != nil {
		dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.db.ExecContext(dbCtx, `UPDATE cron_jobs SET next_run_at = ? WHERE id = ?`, next, job.ID); err != nil {
			slog.Error("[cron] misfire 快进 DB 更新失败", "source", "cron", "id", job.ID, "err", err.Error())
		}
	}
}

// claimJob 跨副本原子领取本次到期刻度：把 last_run_at 推进到 job.NextRunAt。
//
// 仅第一个副本的 UPDATE 命中（RowsAffected==1）；其余副本看到 last_run_at 已 >=
// next_run_at、RowsAffected==0，从而"一个到期刻度只跑一个副本"。与 MySQL 同语义。
// 无 DB（纯内存调度）或 DB 中无此行（测试）时 fail-open 放行，保持既有行为。
func (s *Scheduler) claimJob(job *Job) bool {
	if s.db == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := s.db.ExecContext(ctx,
		`UPDATE cron_jobs SET last_run_at = ? WHERE id = ? AND (last_run_at IS NULL OR last_run_at < ?)`,
		job.NextRunAt, job.ID, job.NextRunAt)
	if err != nil {
		logger.Error("[cron] 原子领取失败，跳过本次执行以防双跑", "id", job.ID, "err", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true
	}
	// RowsAffected==0：另一副本已领取，或 DB 无此行（纯内存/测试）→ 区分后 fail-open。
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM cron_jobs WHERE id = ?`, job.ID).Scan(&exists); err != nil || exists == 0 {
		return true
	}
	return false // 行存在但未领到 → 另一副本已领取本刻度
}

// executeJob 执行单个任务（v2: 走 ScriptExecutor 沙箱，零 LLM）。
func (s *Scheduler) executeJob(job *Job) {
	if _, loaded := s.running.LoadOrStore(job.ID, true); loaded {
		return // 上一次执行尚未完成（进程内去重）
	}
	defer s.running.Delete(job.ID)

	// 跨副本去重：进程内 sync.Map 只防单实例重入，多副本部署时同一到期刻度会被多个
	// 实例同时触发。claimJob 用 DB 原子领取保证一个到期刻度只跑一个副本。
	if !s.claimJob(job) {
		return // 另一副本已领取本到期刻度
	}

	now := time.Now()
	// Note: the persistence ctx must be created AFTER the run finishes — a job
	// can run for minutes, so a 10s ctx created up front would already be
	// expired by history-write time, silently dropping the row.
	earlyCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}

	// 防御：spec 为 nil（理论上 v2 不会发生，AddJob 已强制要求 Spec）
	if job.Spec == nil {
		logger.Error("[cron] 跳过执行 — Spec 为 nil", "id", job.ID)
		ec, cancel := earlyCtx()
		defer cancel()
		_ = s.persistHistory(ec, job.ID, "error", "", "Spec 为 nil — 请重新创建任务", 0, now, "", "", 0, nil)
		return
	}
	// context_from 线性任务链：执行前把上游最近一次产物注入本任务（仅读最近一条，
	// 非 DAG）。注入到 per-run 副本，绝不改共享 *JobSpec。
	if len(job.ContextFrom) > 0 {
		if up := s.latestRunOutput(job.ContextFrom[0]); up != "" {
			job = withUpstreamContext(job, up)
		}
	}

	// Outer execution budget — shared timeout resolution with the executor's
	// inner deadline so the two layers stay self-consistent (review M4).
	runCtx, cancel := context.WithTimeout(context.Background(), runBudget(job.Spec.TimeoutSec))
	defer cancel()
	// §13.3(2)：把 job ID 带进 ctx，供 Starlark state_get/state_set 内建按 job 隔离状态。
	runCtx = withStateJobID(runCtx, job.ID)

	var result *RunResult
	var runErr error
	if job.Spec.Runtime == RuntimeAgent {
		if job.Continuous {
			// 持续型任务：每 tick 读检查点 → 只推进一个增量 → 写回档案，目标完成/停滞/达上限自动收工。
			slog.Info("[cron] running continuous agent job", "source", "cron", "name", job.Name, "id", job.ID)
			result = s.runContinuousAgentJob(runCtx, job)
		} else {
			slog.Info("[cron] running agent job", "source", "cron", "name", job.Name, "id", job.ID)
			result = s.runAgentJob(runCtx, job)
		}
	} else {
		runtime := job.Spec.Runtime
		// Backstop: a script tagged python3 but using Starlark host builtins (emit /
		// kb_ingest / ...) was mis-routed — e.g. a weak compiler model declared python3
		// while emitting Starlark. Run it on the engine it was actually written for
		// instead of failing with NameError at python runtime; also self-heals already
		// persisted jobs without a re-compile.
		if runtime == RuntimePython3 && looksLikeStarlark(job.Spec.Script) {
			slog.Warn("[cron] script tagged python3 but uses Starlark builtins — routing to Starlark engine", "source", "cron", "id", job.ID)
			runtime = RuntimeStarlark
		}
		engine := s.engineFor(runtime)
		if engine == nil {
			slog.Error("[cron] skipping run — no engine for runtime", "source", "cron", "id", job.ID, "runtime", runtime)
			ec, cancel := earlyCtx()
			defer cancel()
			_ = s.persistHistory(ec, job.ID, "error", "", "no script engine for runtime "+runtime, 0, now, "", "", 0, nil)
			return
		}
		if !engine.Available() {
			// Explicit error instead of a silent exec failure (e.g. python3 not
			// installed). The Starlark engine is always available, so steering new
			// mechanical jobs to it avoids this entirely.
			slog.Error("[cron] skipping run — runtime unavailable on this host", "source", "cron", "id", job.ID, "runtime", engine.Name())
			ec, cancel := earlyCtx()
			defer cancel()
			_ = s.persistHistory(ec, job.ID, "error", "", engine.Name()+" runtime is not available on this host", 0, now, "", "", 0, nil)
			return
		}
		logger.Info("[cron] 执行脚本任务", "name", job.Name, "id", job.ID, "engine", engine.Name())
		result, runErr = engine.Execute(runCtx, job.Spec)

		// H1 条件升级门：门脚本调 wake_agent() 且任务开启 EscalateAgent → 注入门捕获
		// 的上下文并跑一轮 Agent（仅此刻花 token）；未 wake / EscalateAgent=false →
		// 只取廉价 Starlark 结果，0 token。成本护栏 canEscalate 防坏门每 tick 刷 token。
		if runErr == nil && result != nil && result.Wake && job.EscalateAgent {
			if strings.TrimSpace(job.EscalatePrompt) == "" {
				slog.Warn("[cron] wake_agent 触发但 EscalatePrompt 为空，跳过升级", "source", "cron", "id", job.ID)
			} else if !s.canEscalate(job.ID) {
				slog.Warn("[cron] wake_agent 触发但距上次升级 < 最小间隔，跳过（防坏门刷 token）", "source", "cron", "id", job.ID)
			} else {
				prompt := job.EscalatePrompt
				if result.WakeContext != "" {
					prompt = "【监控门捕获的上下文】\n" + result.WakeContext + "\n\n" + prompt
				}
				slog.Info("[cron] wake_agent 触发 Agent 升级", "source", "cron", "id", job.ID, "name", job.Name)
				if esc := s.runAgentJobWithPrompt(runCtx, job, prompt); esc != nil {
					result = esc
				}
				s.markEscalated(job.ID)
			}
		}
	}
	if result == nil {
		// Run 返 nil 一般是 venv 准备 / 工作目录创建失败，必须把 runErr 原文带出来便于排查
		errMsg := "executor 返回 nil"
		if runErr != nil {
			errMsg = runErr.Error()
		}
		result = &RunResult{Status: "error", Error: errMsg}
	}
	if runErr != nil && result.Error == "" {
		result.Status = "error"
		result.Error = runErr.Error()
	}
	logger.Info("[cron] 脚本任务结束",
		"name", job.Name, "id", job.ID,
		"status", result.Status, "exit", result.ExitCode, "duration_ms", result.DurationMs)

	// Update job state (the persistence ctx is created after the run finishes,
	// see the note at the top of this function).
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	now = time.Now()
	s.mu.Lock()
	if j, ok := s.jobs[job.ID]; ok {
		j.LastRunAt = now
		j.RunCount++

		// H4：执行失败写 LastError，成功清零（投递失败单独记 LastDeliveryError）。
		if result.Status == "success" {
			j.LastError = ""
		} else {
			j.LastError = result.Error
		}

		if job.Type == JobTypeOnce {
			j.Status = StatusDone
		} else {
			next, err := nextRunTime(job.Schedule, job.Type, now.In(jobLocation(job.TZ)))
			if err != nil {
				// H4 不静默失效：next_run_at 算不出 → loud + 保持 active（绝不静默
				// 转 done/paused 让任务消失）；保留原 NextRunAt，记 LastError 供 UI 可见。
				slog.Error("[cron] next_run_at 算不出，保持 active 不静默停",
					"source", "cron", "id", job.ID, "schedule", job.Schedule, "err", err.Error())
				j.LastError = err.Error()
			} else {
				j.NextRunAt = next
			}
		}
	}
	s.mu.Unlock()

	if job.Type == JobTypeOnce {
		if _, err := s.db.ExecContext(dbCtx, `UPDATE cron_jobs SET status = 'done', last_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
			now, job.ID); err != nil {
			logger.Error("Cron: 更新任务状态失败", "error", err)
		}
	} else {
		next, nerr := nextRunTime(job.Schedule, job.Type, now.In(jobLocation(job.TZ)))
		if nerr != nil {
			// H4 不静默失效：算不出 next 时只推进 last_run/run_count，**不写零时间**
			// 到 next_run_at（写零会让任务每 tick 重跑）；任务保持 active、原 next 不变。
			if _, err := s.db.ExecContext(dbCtx, `UPDATE cron_jobs SET last_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
				now, job.ID); err != nil {
				logger.Error("Cron: 更新任务状态失败", "error", err)
			}
		} else if _, err := s.db.ExecContext(dbCtx, `UPDATE cron_jobs SET last_run_at = ?, next_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
			now, next, job.ID); err != nil {
			logger.Error("Cron: 更新任务状态失败", "error", err)
		}
	}

	// 写入执行历史 — v2 完整字段
	if err := s.persistHistory(dbCtx, job.ID,
		result.Status,
		"", // Result 在 v2 留空（保留兼容字段）
		result.Error,
		result.DurationMs,
		now,
		result.Stdout, result.Stderr, result.ExitCode, result.Data,
	); err != nil {
		logger.Error("Cron: 写入执行历史失败", "error", err)
	}

	if result.Status == "success" {
		// §13.3(2) only_if_changed：产物与上次相同 → 跳过投递（执行已跑、取过数）。
		// 否则按 deliver 目标投递（脚本与 agent 结果同走）。
		if job.OnlyIfChanged && s.outputUnchanged(job.ID, result) {
			slog.Info("[cron] only_if_changed：产物未变，跳过投递", "source", "cron", "id", job.ID, "name", job.Name)
		} else {
			s.deliverResult(job, result)
		}
	} else {
		// failure_deliver: route a failure summary to the job's dedicated
		// failure channels; falls back to the existing throttled alert when unset.
		// Additive to the existing alert/self-heal paths.
		s.maybeFailureDeliver(job, result)
		if job.Spec.Runtime == RuntimeAgent {
			// Agent jobs have no script to recompile — alert on persistent failure
			// instead of healing.
			s.maybeAlertAgentFailure(dbCtx, job, result)
		} else {
			// Self-heal bridge: consecutive script failures past the threshold →
			// recompile with the failure context (cooldown-window quota applies).
			s.maybeSelfHeal(dbCtx, job, result)
		}
	}
}

// maybeFailureDeliver routes a failure summary to the job's FailureDeliver
// channels. Mirrors deliverResult's chat/push-via-notify vs IM-via-
// deliverer routing. Fire-and-forget, best-effort; no-op when unset.
func (s *Scheduler) maybeFailureDeliver(job *Job, result *RunResult) {
	if job == nil || len(job.FailureDeliver) == 0 {
		return
	}
	summary := failureSummary(job, result)
	s.agent.mu.Lock()
	deliverer := s.agent.deliverer
	s.agent.mu.Unlock()

	notified := false
	for _, target := range job.FailureDeliver {
		switch target {
		case "", "chat", "notify", "push":
			if !notified {
				s.notify(job, NotifyLevelWarning, job.Name+" 执行失败", summary)
				notified = true
			}
		default:
			if deliverer == nil {
				slog.Warn("[cron] failure_deliver: no deliverer wired for IM target",
					"source", "cron", "id", job.ID, "target", target)
				continue
			}
			if err := deliverer(job, target, summary); err != nil {
				slog.Warn("[cron] failure_deliver failed",
					"source", "cron", "id", job.ID, "target", target, "err", err.Error())
			}
		}
	}
}

// failureSummary builds a short human-readable failure line for failure_deliver.
func failureSummary(job *Job, result *RunResult) string {
	msg := "(no error detail)"
	if result != nil && strings.TrimSpace(result.Error) != "" {
		msg = clipForHeal(result.Error, 300)
	}
	return fmt.Sprintf("⚠️ 定时任务「%s」执行失败：%s", job.Name, msg)
}

// loadJobs 从数据库加载活跃任务
func (s *Scheduler) loadJobs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status,
		 last_run_at, next_run_at, run_count, created_at, meta
		 FROM cron_jobs WHERE status = 'active'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		job, err := scanJobRow(rows)
		if err != nil {
			return err
		}
		s.jobs[job.ID] = job
	}

	logger.Info("Cron 已加载", "len", len(s.jobs))
	return rows.Err()
}

// updateJobStatus 更新任务状态
func (s *Scheduler) updateJobStatus(ctx context.Context, jobID string, status JobStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cron_jobs SET status = ? WHERE id = ?`, status, jobID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if job, ok := s.jobs[jobID]; ok {
		job.Status = status
	}
	s.mu.Unlock()
	return nil
}

// --- Cron 表达式解析 ---

// nextRunTime 计算下次执行时间
//
// 支持的格式：
//   - @every 5m / @every 1h / @every 30s — 固定间隔
//   - @daily — 每天 00:00
//   - @hourly — 每小时整点
//   - @weekly — 每周一 00:00
//   - 标准 5 字段: "分 时 日 月 周" (如 "0 9 * * *" 每天 9:00)
//   - ISO 时间: "2026-03-15T10:00:00" — 一次性任务
func nextRunTime(schedule string, jobType JobType, from time.Time) (time.Time, error) {
	schedule = strings.TrimSpace(schedule)

	// 快捷方式（使用本地时区计算，而非 UTC）
	switch schedule {
	case "@daily":
		// 明天本地时间 00:00（使用 time.Date 避免 Truncate 的 UTC 问题）
		next := time.Date(from.Year(), from.Month(), from.Day()+1, 0, 0, 0, 0, from.Location())
		return next, nil
	case "@hourly":
		// Local top-of-hour (time.Date avoids Truncate's UTC alignment, which
		// fires at HH:30 in half-hour-offset zones).
		next := time.Date(from.Year(), from.Month(), from.Day(), from.Hour()+1, 0, 0, 0, from.Location())
		return next, nil
	case "@weekly":
		// 下周一本地时间 00:00
		daysUntilMonday := (8 - int(from.Weekday())) % 7
		if daysUntilMonday == 0 {
			daysUntilMonday = 7
		}
		today := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
		next := today.AddDate(0, 0, daysUntilMonday)
		return next, nil
	}

	// @every 间隔
	if strings.HasPrefix(schedule, "@every ") {
		durStr := strings.TrimPrefix(schedule, "@every ")
		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return time.Time{}, fmt.Errorf("无效的间隔: %s", durStr)
		}
		// A non-positive interval would leave NextRunAt forever in the past,
		// firing the job on every tick.
		if dur <= 0 {
			return time.Time{}, fmt.Errorf("间隔必须为正: %s", durStr)
		}
		return from.Add(dur), nil
	}

	// ISO 时间（一次性任务）
	if jobType == JobTypeOnce {
		t, err := time.Parse(time.RFC3339, schedule)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04:05", schedule)
		}
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04", schedule)
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("无效的时间格式: %s", schedule)
		}
		return t, nil
	}

	// 标准 5 字段 cron 表达式: "分 时 日 月 周"
	return parseCron5(schedule, from)
}

// parseCron5 解析标准 5 字段 cron 表达式："分 时 日 月 周"
//
// 支持完整 crontab 字段语法：通配 *、单值、列表 a,b,c、范围 a-b、
// 步进 */n 与 a-b/n、命名月份（JAN..DEC）与命名星期（SUN..SAT）。
func parseCron5(expr string, from time.Time) (time.Time, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("cron 表达式需要 5 个字段，得到 %d 个", len(fields))
	}

	minSet, _, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("分钟字段无效: %w", err)
	}
	hourSet, _, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("小时字段无效: %w", err)
	}
	daySet, dayStar, err := parseCronField(fields[2], 1, 31, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期字段无效: %w", err)
	}
	monthSet, _, err := parseCronField(fields[3], 1, 12, cronMonthNames)
	if err != nil {
		return time.Time{}, fmt.Errorf("月份字段无效: %w", err)
	}
	dowSet, dowStar, err := parseCronField(fields[4], 0, 7, cronDayNames)
	if err != nil {
		return time.Time{}, fmt.Errorf("星期字段无效: %w", err)
	}
	// POSIX: 周日既可写 0 也可写 7，统一归到 0（time.Weekday 中 Sunday=0）
	if dowSet[7] {
		dowSet[0] = true
		delete(dowSet, 7)
	}

	// 从 from 的下一分钟开始搜索
	candidate := from.Truncate(time.Minute).Add(time.Minute)

	// 最多搜索 366 天（覆盖所有情况）
	maxIter := 366 * 24 * 60
	for i := 0; i < maxIter; i++ {
		minMatch := minSet[candidate.Minute()]
		hourMatch := hourSet[candidate.Hour()]
		dayMatch := daySet[candidate.Day()]
		monthMatch := monthSet[int(candidate.Month())]
		dowMatch := dowSet[int(candidate.Weekday())]
		// POSIX: 当 day-of-month 与 day-of-week 都受限（非 *）时取 OR；否则取 AND。
		dayDowMatch := dayMatch && dowMatch
		if !dayStar && !dowStar {
			dayDowMatch = dayMatch || dowMatch
		}
		if minMatch && hourMatch && dayDowMatch && monthMatch {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}

	return time.Time{}, fmt.Errorf("无法计算下次执行时间: %s", expr)
}

// cronMonthNames / cronDayNames 支持命名字段（大小写不敏感）。
var cronMonthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var cronDayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseCronField 解析单个 cron 字段，返回该字段在 [min,max] 内匹配的值集合。
// 支持标准 crontab 语法：通配 *、单值、列表 a,b,c、范围 a-b、
// 步进 */n 与 a-b/n 与 a/n、命名（月/周，大小写不敏感）。
// 第二个返回值 isStar 表示该字段是否为纯通配（用于 POSIX day/dow 的 OR 语义）。
func parseCronField(field string, min, max int, names map[string]int) (map[int]bool, bool, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, false, fmt.Errorf("空字段")
	}
	isStar := field == "*" || strings.HasPrefix(field, "*/")
	result := make(map[int]bool)
	for _, part := range strings.Split(field, ",") {
		if err := parseCronPart(part, min, max, names, result); err != nil {
			return nil, false, err
		}
	}
	if len(result) == 0 {
		return nil, false, fmt.Errorf("无效的字段: %s", field)
	}
	return result, isStar, nil
}

// parseCronPart 解析逗号列表中的单个分量（含步进/范围/命名），把命中的值写入 result。
func parseCronPart(part string, min, max int, names map[string]int, result map[int]bool) error {
	part = strings.TrimSpace(part)
	if part == "" {
		return fmt.Errorf("空字段分量")
	}

	// 步进：<range>/<step>
	step := 1
	rangePart := part
	if slash := strings.IndexByte(part, '/'); slash >= 0 {
		rangePart = part[:slash]
		s, err := strconv.Atoi(part[slash+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("无效的步进: %s", part)
		}
		step = s
	}

	// 区间端点
	var lo, hi int
	switch {
	case rangePart == "*":
		lo, hi = min, max
	case strings.IndexByte(rangePart, '-') > 0:
		dash := strings.IndexByte(rangePart, '-')
		var err error
		if lo, err = parseCronValue(rangePart[:dash], names); err != nil {
			return err
		}
		if hi, err = parseCronValue(rangePart[dash+1:], names); err != nil {
			return err
		}
	default:
		v, err := parseCronValue(rangePart, names)
		if err != nil {
			return err
		}
		if step > 1 {
			// 单值带步进（如 5/10）：从该值起到 max
			lo, hi = v, max
		} else {
			lo, hi = v, v
		}
	}

	if lo < min || hi > max || lo > hi {
		return fmt.Errorf("值 %d-%d 超出范围 [%d, %d]", lo, hi, min, max)
	}
	for v := lo; v <= hi; v += step {
		result[v] = true
	}
	return nil
}

// parseCronValue 解析单个数值或命名（月/周，大小写不敏感）。
func parseCronValue(s string, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("无效的数字: %s", s)
	}
	return v, nil
}
