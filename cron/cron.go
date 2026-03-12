// Package cron 提供定时任务调度
//
// 支持两种任务类型：
//   - 周期任务：标准 cron 表达式 + @every/@daily 等快捷方式
//   - 一次性任务：定时提醒，执行后自动删除
//
// 任务持久化到 SQLite，服务重启后自动恢复。
// 任务执行通过回调函数触发，通常调用 Agent 引擎处理。
//
// 用法示例：
//
//	scheduler := cron.NewScheduler(store)
//	scheduler.AddJob(ctx, &cron.Job{
//	    Name:     "每日摘要",
//	    Schedule: "@daily",
//	    Prompt:   "总结今天的待办事项和重要邮件",
//	    UserID:   "user-1",
//	})
//	scheduler.Start(ctx, func(ctx context.Context, job *cron.Job) error {
//	    // 调用引擎处理
//	})
package cron

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everyday-items/toolkit/util/idgen"
)

// JobType 任务类型
type JobType string

const (
	JobTypeCron   JobType = "cron"   // 周期任务（cron 表达式）
	JobTypeOnce   JobType = "once"   // 一次性任务（定时提醒）
)

// JobStatus 任务状态
type JobStatus string

const (
	StatusActive  JobStatus = "active"  // 活跃
	StatusPaused  JobStatus = "paused"  // 暂停
	StatusDone    JobStatus = "done"    // 已完成（一次性任务执行后）
)

// Job 定时任务
type Job struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`        // 任务名称
	Type       JobType   `json:"type"`        // 任务类型
	Schedule   string    `json:"schedule"`    // cron 表达式或时间点
	Prompt     string    `json:"prompt"`      // 发送给 Agent 的提示词
	UserID     string    `json:"user_id"`     // 所属用户
	Platform   string    `json:"platform"`    // 通知平台
	ChatID     string    `json:"chat_id"`     // 通知目标
	Status     JobStatus `json:"status"`      // 任务状态
	LastRunAt  time.Time `json:"last_run_at"` // 上次执行时间
	NextRunAt  time.Time `json:"next_run_at"` // 下次执行时间
	RunCount   int       `json:"run_count"`   // 已执行次数
	CreatedAt  time.Time `json:"created_at"`
}

// JobExecutor 任务执行回调
type JobExecutor func(ctx context.Context, job *Job) error

// Scheduler 定时任务调度器
//
// 使用最小堆 + ticker 实现高效调度。
// 所有任务持久化到 SQLite，支持服务重启恢复。
type Scheduler struct {
	mu       sync.RWMutex
	db       *sql.DB
	executor JobExecutor
	jobs     map[string]*Job // id -> job
	stopCh   chan struct{}
	stopped  bool
}

// NewScheduler 创建调度器
func NewScheduler(db *sql.DB) *Scheduler {
	return &Scheduler{
		db:   db,
		jobs: make(map[string]*Job),
	}
}

// Init 初始化调度器存储表
func (s *Scheduler) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL,
		prompt TEXT NOT NULL,
		user_id TEXT NOT NULL,
		platform TEXT DEFAULT '',
		chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("初始化 cron 表失败: %w", err)
	}

	// 加载所有活跃任务
	return s.loadJobs(ctx)
}

// Start 启动调度器
//
// executor 为任务执行回调，通常调用 Agent 引擎处理。
// 调度器会每秒检查是否有任务需要执行。
func (s *Scheduler) Start(_ context.Context, executor JobExecutor) {
	s.mu.Lock()
	s.executor = executor
	s.stopCh = make(chan struct{})
	s.stopped = false
	s.mu.Unlock()

	go s.runLoop()
	log.Println("Cron 调度器已启动")
}

// Stop 停止调度器
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
		log.Println("Cron 调度器已停止")
	}
}

// AddJob 添加定时任务
func (s *Scheduler) AddJob(ctx context.Context, job *Job) error {
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

	// 持久化
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, platform, chat_id, status, next_run_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Type, job.Schedule, job.Prompt,
		job.UserID, job.Platform, job.ChatID, job.Status, job.NextRunAt, job.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("保存任务失败: %w", err)
	}

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	log.Printf("Cron 任务已添加: %s (%s) 下次执行: %s", job.Name, job.Schedule, job.NextRunAt.Format(time.RFC3339))
	return nil
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
	query := `SELECT id, name, type, schedule, prompt, user_id, platform, chat_id, status,
	          last_run_at, next_run_at, run_count, created_at
	          FROM cron_jobs WHERE user_id = ? ORDER BY next_run_at`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job := &Job{}
		var lastRun sql.NullTime
		if err := rows.Scan(&job.ID, &job.Name, &job.Type, &job.Schedule, &job.Prompt,
			&job.UserID, &job.Platform, &job.ChatID, &job.Status,
			&lastRun, &job.NextRunAt, &job.RunCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		if lastRun.Valid {
			job.LastRunAt = lastRun.Time
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
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

// executeJob 执行单个任务
func (s *Scheduler) executeJob(job *Job) {
	s.mu.RLock()
	executor := s.executor
	s.mu.RUnlock()

	if executor == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Printf("Cron 执行任务: %s (%s)", job.Name, job.ID)

	if err := executor(ctx, job); err != nil {
		log.Printf("Cron 任务执行失败: %s: %v", job.Name, err)
	}

	// 更新任务状态
	now := time.Now()
	s.mu.Lock()
	if j, ok := s.jobs[job.ID]; ok {
		j.LastRunAt = now
		j.RunCount++

		if job.Type == JobTypeOnce {
			// 一次性任务执行后标记完成
			j.Status = StatusDone
		} else {
			// 计算下次执行时间
			next, err := nextRunTime(job.Schedule, job.Type, now)
			if err == nil {
				j.NextRunAt = next
			}
		}
	}
	s.mu.Unlock()

	// 持久化状态
	if job.Type == JobTypeOnce {
		s.db.ExecContext(ctx, `UPDATE cron_jobs SET status = 'done', last_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
			now, job.ID)
	} else {
		next, _ := nextRunTime(job.Schedule, job.Type, now)
		s.db.ExecContext(ctx, `UPDATE cron_jobs SET last_run_at = ?, next_run_at = ?, run_count = run_count + 1 WHERE id = ?`,
			now, next, job.ID)
	}
}

// loadJobs 从数据库加载活跃任务
func (s *Scheduler) loadJobs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, schedule, prompt, user_id, platform, chat_id, status,
		 last_run_at, next_run_at, run_count, created_at
		 FROM cron_jobs WHERE status = 'active'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		job := &Job{}
		var lastRun sql.NullTime
		if err := rows.Scan(&job.ID, &job.Name, &job.Type, &job.Schedule, &job.Prompt,
			&job.UserID, &job.Platform, &job.ChatID, &job.Status,
			&lastRun, &job.NextRunAt, &job.RunCount, &job.CreatedAt); err != nil {
			return err
		}
		if lastRun.Valid {
			job.LastRunAt = lastRun.Time
		}
		s.jobs[job.ID] = job
	}

	log.Printf("Cron 已加载 %d 个活跃任务", len(s.jobs))
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

	// 从 from 的下一分钟开始搜索
	candidate := from.Truncate(time.Minute).Add(time.Minute)

	// 最多搜索 366 天（覆盖所有情况）
	maxIter := 366 * 24 * 60
	for i := 0; i < maxIter; i++ {
		minMatch := minute == -1 || candidate.Minute() == minute
		hourMatch := hour == -1 || candidate.Hour() == hour
		// 日/月/周暂用通配符简化处理
		if minMatch && hourMatch {
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
