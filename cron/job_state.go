package cron

// job_state.go 实现 §13.3(2) 增量发现的跨运行状态：per-job 持久化 KV，支撑 Starlark
// state_get/state_set 内建与 only_if_changed 投递门。单写者（running 守卫保证任务不与
// 自身并发）→ DB 之外无需加锁；带容量界防无界涨盘（同 Hermes 产物无界堆盘的坑）。

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/hexagon-codes/toolkit/util/hash"
)

// StateStore 持久化 per-job 跨运行键值状态。
type StateStore interface {
	Get(jobID, key string) (string, bool)
	Set(jobID, key, val string) error
}

// 容量界：单 job key 数 + value 字节 + TTL，防一个脚本每次写新 key / 写巨值撑爆库。
const (
	maxStateKeysPerJob = 100
	maxStateValueBytes = 64 * 1024
	stateTTL           = 30 * 24 * time.Hour
)

type sqlStateStore struct{ db *sql.DB }

func newSQLStateStore(db *sql.DB) *sqlStateStore { return &sqlStateStore{db: db} }

func (s *sqlStateStore) Get(jobID, key string) (string, bool) {
	if s.db == nil || jobID == "" || key == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var val string
	var updated time.Time
	if err := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM cron_job_state WHERE job_id = ? AND key = ?`,
		jobID, key).Scan(&val, &updated); err != nil {
		return "", false
	}
	if time.Since(updated) > stateTTL {
		// TTL 清理失败不改变"视为过期"语义，但不能静默吞错（DB errcheck 红线）：记录以便排查脏数据残留
		if _, err := s.db.ExecContext(ctx, `DELETE FROM cron_job_state WHERE job_id = ? AND key = ?`, jobID, key); err != nil {
			slog.Warn("[cron] state TTL cleanup failed", "source", "cron", "job_id", jobID, "key", key, "error", err)
		}
		return "", false
	}
	return val, true
}

func (s *sqlStateStore) Set(jobID, key, val string) error {
	if s.db == nil {
		return fmt.Errorf("state store not available")
	}
	if jobID == "" || key == "" {
		return fmt.Errorf("state key requires a job context")
	}
	if len(val) > maxStateValueBytes {
		return fmt.Errorf("state value too large: %d > %d bytes", len(val), maxStateValueBytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 新 key 才计入上限：更新既有 key 永远允许（有界工作集不会被卡住）；新增超界则拒。
	var existing int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM cron_job_state WHERE job_id = ? AND key = ?`, jobID, key).Scan(&existing)
	if existing == 0 {
		var n int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM cron_job_state WHERE job_id = ?`, jobID).Scan(&n)
		if n >= maxStateKeysPerJob {
			return fmt.Errorf("too many state keys for job (%d ≥ %d); reuse keys", n, maxStateKeysPerJob)
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cron_job_state (job_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(job_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		jobID, key, val, time.Now())
	return err
}

// stateJobIDKey 通过 ctx 把当前 job lifecycle scope 传给 state 内建
// （StarlarkEngine.Execute 只收 spec，不收 job；用 ctx 携带不会污染共享引擎/spec）。
type stateJobIDKeyT struct{}

var stateJobIDKey stateJobIDKeyT

type stateJobScope struct {
	jobID      string
	generation string
}

// withStateJobID preserves the legacy/standalone generation-less scope used by
// direct engine callers and old tests. Admitted scheduler jobs use withStateJob.
func withStateJobID(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, stateJobIDKey, stateJobScope{jobID: jobID})
}

func withStateJob(ctx context.Context, job *Job) context.Context {
	if job == nil {
		return context.WithValue(ctx, stateJobIDKey, stateJobScope{})
	}
	return context.WithValue(ctx, stateJobIDKey, stateJobScope{
		jobID: job.ID, generation: job.Generation,
	})
}

func stateJobScopeFrom(ctx context.Context) stateJobScope {
	scope, _ := ctx.Value(stateJobIDKey).(stateJobScope)
	return scope
}

func stateJobIDFrom(ctx context.Context) string {
	return stateJobScopeFrom(ctx).jobID
}

func stateKeyFromContext(ctx context.Context, key string) string {
	scope := stateJobScopeFrom(ctx)
	return stateKeyForGeneration(key, &Job{Generation: scope.generation})
}

// lastOutputHashKey 是 only_if_changed 投递门存放上次产物哈希的保留 key。
const lastOutputHashKey = "__last_output_hash__"

// stateKeyForGeneration keeps state that belongs to a concrete job definition
// isolated across stable-key replacements. Upsert deliberately reuses the
// public job ID, so jobID alone is not a lifecycle identity.
func stateKeyForGeneration(base string, job *Job) string {
	if job == nil || job.Generation == "" {
		return base
	}
	return base + ":" + job.Generation
}

func hashString(s string) string {
	// 复用 toolkit/util/hash.SHA256：内部即 sha256.Sum256 + hex 编码，输出与原实现逐字节一致。
	return hash.SHA256(s)
}

// outputUnchanged reports whether this run's deliverable content hashes equal to
// the previous run's (only_if_changed gate). It records the new hash on every
// call, so the first run (or any change) returns false and delivers. No store →
// false (never suppress without a way to compare — conservative: deliver).
func (s *Scheduler) outputUnchanged(jobID string, result *RunResult) bool {
	return s.outputUnchangedWithKey(jobID, lastOutputHashKey, result)
}

// outputUnchangedForGeneration isolates delivery-dedup state across stable-key
// replacements. Reusing the public job ID must not let an old run seed or
// suppress the replacement's first output.
func (s *Scheduler) outputUnchangedForGeneration(job *Job, result *RunResult) bool {
	if job == nil {
		return false
	}
	return s.outputUnchangedWithKey(job.ID, stateKeyForGeneration(lastOutputHashKey, job), result)
}

func (s *Scheduler) outputUnchangedWithKey(jobID, key string, result *RunResult) bool {
	if s.state == nil {
		return false
	}
	h := hashString(deliverableContent(result))
	prev, ok := s.state.Get(jobID, key)
	if ok && prev == h {
		return true
	}
	if err := s.state.Set(jobID, key, h); err != nil {
		// 记不住哈希 → 下次仍会投递（保守，不会误抑制）。
		return false
	}
	return false
}
