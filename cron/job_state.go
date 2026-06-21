package cron

// job_state.go 实现 §13.3(2) 增量发现的跨运行状态：per-job 持久化 KV，支撑 Starlark
// state_get/state_set 内建与 only_if_changed 投递门。单写者（running 守卫保证任务不与
// 自身并发）→ DB 之外无需加锁；带容量界防无界涨盘（同 Hermes 产物无界堆盘的坑）。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
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

// stateJobIDKey 通过 ctx 把当前 job ID 传给 state 内建（StarlarkEngine.Execute 只收
// spec，不收 job；用 ctx 携带是最干净的 per-run scope，且不污染共享的引擎或 spec）。
type stateJobIDKeyT struct{}

var stateJobIDKey stateJobIDKeyT

func withStateJobID(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, stateJobIDKey, jobID)
}

func stateJobIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(stateJobIDKey).(string)
	return v
}

// lastOutputHashKey 是 only_if_changed 投递门存放上次产物哈希的保留 key。
const lastOutputHashKey = "__last_output_hash__"

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// outputUnchanged reports whether this run's deliverable content hashes equal to
// the previous run's (only_if_changed gate). It records the new hash on every
// call, so the first run (or any change) returns false and delivers. No store →
// false (never suppress without a way to compare — conservative: deliver).
func (s *Scheduler) outputUnchanged(jobID string, result *RunResult) bool {
	if s.state == nil {
		return false
	}
	h := hashString(deliverableContent(result))
	prev, ok := s.state.Get(jobID, lastOutputHashKey)
	if ok && prev == h {
		return true
	}
	if err := s.state.Set(jobID, lastOutputHashKey, h); err != nil {
		// 记不住哈希 → 下次仍会投递（保守，不会误抑制）。
		return false
	}
	return false
}
