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
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Deliver         []string `json:"deliver,omitempty"`
	AvailableSkills []string `json:"-"` // 服务端注入
	LocalAPIBase    string   `json:"-"` // 服务端注入
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
	agent      agentSupport
	jobs       map[string]*Job // id -> job
	stopCh     chan struct{}
	stopped    bool
	running    sync.Map // map[string]bool — 正在执行的任务 ID
}

// NewScheduler 创建调度器。
//
// compiler/scriptExec 可为 nil — 主要给测试使用（测试可只构造 prebuilt Job 不走编译）。
// 业务代码（cmd/hexclaw/main.go）必须注入两者，否则 AddJobFromPrompt / executeJob 会拒收。
func NewScheduler(db *sql.DB, compiler JobSpecCompiler, scriptExec *ScriptExecutor) *Scheduler {
	return &Scheduler{
		db:         db,
		compiler:   compiler,
		scriptExec: scriptExec,
		jobs:       make(map[string]*Job),
	}
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

	// 计算下次执行时间
	next, err := nextRunTime(job.Schedule, job.Type, time.Now())
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

	emit := func(stage ProgressStage, msg string) {
		if onProgress != nil {
			onProgress(CompileProgress{Stage: stage, Message: msg})
		}
	}

	emit(StageAnalyzing, "解析需求并选择 LLM provider…")

	// Dual-mode classification: cognitive tasks are marked for agent execution
	// (no script compilation); mechanical I/O tasks go through script compile.
	if ClassifyTaskMode(req.Prompt) == RuntimeAgent {
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
			Spec:         &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 300},
			Deliver:      req.Deliver,
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
	          last_run_at, next_run_at, run_count, created_at
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
	)
	if err := row.Scan(
		&job.ID, &job.Name, &job.Type, &job.Schedule,
		&specJSON, &srcPrompt,
		&job.UserID, &job.Platform, &job.ChatID, &job.Status,
		&lastRun, &job.NextRunAt, &job.RunCount, &job.CreatedAt,
	); err != nil {
		return nil, err
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
		Deliver []string `json:"deliver,omitempty"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		return
	}
	if len(m.Deliver) > 0 {
		job.Deliver = m.Deliver
	}
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
	if s.scriptExec == nil {
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
func (s *Scheduler) GetJobHistory(ctx context.Context, jobID string) ([]JobHistory, error) {
	s.mu.RLock()
	_, ok := s.jobs[jobID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("任务 %q 不存在", jobID)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, status, COALESCE(result,''), error, duration_ms, run_at,
		        COALESCE(stdout,''), COALESCE(stderr,''), COALESCE(exit_code,0), COALESCE(data_json,'')
		 FROM cron_job_runs WHERE job_id = ? ORDER BY run_at DESC LIMIT 50`, jobID)
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cron_job_runs (job_id, status, result, error, duration_ms, run_at, stdout, stderr, exit_code, data_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, status, result, errMsg, durationMs, runAt, stdout, stderr, exitCode, dataJSON)
	return err
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
		go s.executeJob(job)
	}
}

// executeJob 执行单个任务（v2: 走 ScriptExecutor 沙箱，零 LLM）。
func (s *Scheduler) executeJob(job *Job) {
	if _, loaded := s.running.LoadOrStore(job.ID, true); loaded {
		return // 上一次执行尚未完成
	}
	defer s.running.Delete(job.ID)

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
	// 执行预算：spec.TimeoutSec + 30s 给 venv / 进程清理留余量
	budget := time.Duration(job.Spec.TimeoutSec)*time.Second + 30*time.Second
	if budget < 60*time.Second {
		budget = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var result *RunResult
	var runErr error
	if job.Spec.Runtime == RuntimeAgent {
		slog.Info("[cron] running agent job", "source", "cron", "name", job.Name, "id", job.ID)
		result = s.runAgentJob(runCtx, job)
	} else {
		if s.scriptExec == nil {
			slog.Error("[cron] skipping run — scriptExec not injected", "source", "cron", "id", job.ID)
			ec, cancel := earlyCtx()
			defer cancel()
			_ = s.persistHistory(ec, job.ID, "error", "", "脚本执行器未就绪", 0, now, "", "", 0, nil)
			return
		}
		logger.Info("[cron] 执行脚本任务", "name", job.Name, "id", job.ID)
		result, runErr = s.scriptExec.Run(runCtx, job.Spec)
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

		if job.Type == JobTypeOnce {
			j.Status = StatusDone
		} else {
			next, err := nextRunTime(job.Schedule, job.Type, now)
			if err == nil {
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
		next, _ := nextRunTime(job.Schedule, job.Type, now)
		if _, err := s.db.ExecContext(dbCtx, `UPDATE cron_jobs SET last_run_at = ?, next_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
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
		// Deliver agent-mode results: script jobs self-deliver via the compiled
		// script (POST /api/v1/notify), agent jobs rely on the scheduler routing
		// the final content through the job's deliver targets (review H2).
		if job.Spec.Runtime == RuntimeAgent {
			s.deliverAgentResult(job, result)
		}
	} else {
		// Self-heal bridge: consecutive script failures past the threshold →
		// recompile with the failure context (cooldown-window quota applies).
		s.maybeSelfHeal(dbCtx, job, result)
	}
}

// loadJobs 从数据库加载活跃任务
func (s *Scheduler) loadJobs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, schedule, spec_json, source_prompt, user_id, platform, chat_id, status,
		 last_run_at, next_run_at, run_count, created_at
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
		next := from.Truncate(time.Hour).Add(time.Hour)
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

// parseCron5 解析标准 5 字段 cron 表达式
//
// 简化实现：支持数字和 * 通配符，不支持范围和步进。
// 对于个人 Agent 的定时任务场景够用。
func parseCron5(expr string, from time.Time) (time.Time, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("cron 表达式需要 5 个字段，得到 %d 个", len(fields))
	}

	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("分钟字段无效: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, fmt.Errorf("小时字段无效: %w", err)
	}
	day, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期字段无效: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, fmt.Errorf("月份字段无效: %w", err)
	}
	dow, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return time.Time{}, fmt.Errorf("星期字段无效: %w", err)
	}

	// 从 from 的下一分钟开始搜索
	candidate := from.Truncate(time.Minute).Add(time.Minute)

	// 最多搜索 366 天（覆盖所有情况）
	maxIter := 366 * 24 * 60
	for i := 0; i < maxIter; i++ {
		minMatch := minute == -1 || candidate.Minute() == minute
		hourMatch := hour == -1 || candidate.Hour() == hour
		dayMatch := day == -1 || candidate.Day() == day
		monthMatch := month == -1 || int(candidate.Month()) == month
		dowMatch := dow == -1 || int(candidate.Weekday()) == dow
		if minMatch && hourMatch && dayMatch && monthMatch && dowMatch {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}

	return time.Time{}, fmt.Errorf("无法计算下次执行时间: %s", expr)
}

// parseCronField 解析单个 cron 字段
// 返回 -1 表示通配符 (*)
func parseCronField(field string, min, max int) (int, error) {
	if field == "*" {
		return -1, nil
	}
	v, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("无效的数字: %s", field)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("值 %d 超出范围 [%d, %d]", v, min, max)
	}
	return v, nil
}
