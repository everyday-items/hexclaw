package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CronIntegrityV29 repairs databases that were previously exposed to the v5
// cron_jobs table rebuild. That rebuild dropped secondary indexes and meta,
// and SQLite could cascade-delete run/state evidence when foreign keys were on.
var CronIntegrityV29 = Migration{
	Version:     29,
	Description: "v0.5.0 Cron 父子数据无损修复、冲突隔离与可审计唯一约束",
	Func:        RepairCronIntegrityV29,
}

const cronJobsCanonicalDDL = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'cron',
	schedule TEXT NOT NULL,
	spec_json TEXT NOT NULL DEFAULT '',
	source_prompt TEXT NOT NULL DEFAULT '',
	user_id TEXT NOT NULL,
	platform TEXT NOT NULL DEFAULT '',
	chat_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	last_run_at DATETIME,
	next_run_at DATETIME NOT NULL,
	run_count INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	meta TEXT NOT NULL DEFAULT '{}'
)`

const cronRunsCanonicalDDL = `CREATE TABLE %s (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'success',
	result TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	run_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	stdout TEXT NOT NULL DEFAULT '',
	stderr TEXT NOT NULL DEFAULT '',
	exit_code INTEGER NOT NULL DEFAULT 0,
	data_json TEXT NOT NULL DEFAULT '',
	FOREIGN KEY(job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
)`

const cronStateCanonicalDDL = `CREATE TABLE %s (
	job_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL DEFAULT '',
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(job_id,key),
	FOREIGN KEY(job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
)`

const cronMergeAuditDDL = `CREATE TABLE IF NOT EXISTS cron_job_merge_audit (
	audit_id TEXT PRIMARY KEY,
	repair_version INTEGER NOT NULL,
	event_kind TEXT NOT NULL,
	group_user_id TEXT NOT NULL DEFAULT '',
	group_name TEXT NOT NULL DEFAULT '',
	survivor_job_id TEXT NOT NULL DEFAULT '',
	loser_job_id TEXT NOT NULL DEFAULT '',
	child_key TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '{}',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

type cronSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type cronJobRow struct {
	ID           string
	Name         string
	Type         string
	Schedule     string
	SpecJSON     string
	SourcePrompt string
	UserID       string
	Platform     string
	ChatID       string
	Status       string
	LastRunAt    any
	NextRunAt    any
	RunCount     int64
	CreatedAt    any
	Meta         string
}

// migrateCronV2InPlace implements the historical v5 transition without ever
// dropping cron_jobs. SQLite DROP COLUMN preserves the parent row, its child
// rows and unrelated indexes; an unsupported/unsafe schema fails atomically.
func migrateCronV2InPlace(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := enableCronForeignKeys(ctx, conn); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureCronV2ColumnsInPlace(ctx, tx, false); err != nil {
		return err
	}
	if err := ensureCronStateTable(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, cronMergeAuditDDL); err != nil {
		return fmt.Errorf("创建 Cron merge audit: %w", err)
	}
	if err := repairCronDuplicateGroups(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_cron_jobs_user_name
		ON cron_jobs(user_id,name)`); err != nil {
		return fmt.Errorf("创建 Cron user/name 唯一索引: %w", err)
	}
	if err := validateCronDatabase(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureCronV2ColumnsInPlace(ctx context.Context, q cronSQL, createUnique bool) error {
	hasJobs, err := cronTableExists(ctx, q, "cron_jobs")
	if err != nil {
		return fmt.Errorf("检查 cron_jobs: %w", err)
	}
	if !hasJobs {
		if _, err := q.ExecContext(ctx, fmt.Sprintf(cronJobsCanonicalDDL, "cron_jobs")); err != nil {
			return fmt.Errorf("创建 cron_jobs: %w", err)
		}
	} else {
		for _, column := range []struct{ name, ddl string }{
			{"spec_json", `TEXT NOT NULL DEFAULT ''`},
			{"source_prompt", `TEXT NOT NULL DEFAULT ''`},
			{"meta", `TEXT NOT NULL DEFAULT '{}'`},
		} {
			has, checkErr := cronColumnExists(ctx, q, "cron_jobs", column.name)
			if checkErr != nil {
				return checkErr
			}
			if !has {
				if _, execErr := q.ExecContext(ctx, fmt.Sprintf(
					`ALTER TABLE cron_jobs ADD COLUMN %s %s`, column.name, column.ddl)); execErr != nil {
					return fmt.Errorf("新增 cron_jobs.%s: %w", column.name, execErr)
				}
			}
		}
		hasPrompt, checkErr := cronColumnExists(ctx, q, "cron_jobs", "prompt")
		if checkErr != nil {
			return checkErr
		}
		if hasPrompt {
			if _, execErr := q.ExecContext(ctx, `UPDATE cron_jobs SET source_prompt=prompt
				WHERE source_prompt=''`); execErr != nil {
				return fmt.Errorf("保全 cron_jobs.prompt 到 source_prompt: %w", execErr)
			}
			if _, execErr := q.ExecContext(ctx, `ALTER TABLE cron_jobs DROP COLUMN prompt`); execErr != nil {
				return fmt.Errorf("原地删除 cron_jobs.prompt（未改写父表）: %w", execErr)
			}
		}
	}

	hasRuns, err := cronTableExists(ctx, q, "cron_job_runs")
	if err != nil {
		return fmt.Errorf("检查 cron_job_runs: %w", err)
	}
	if !hasRuns {
		if _, err := q.ExecContext(ctx, fmt.Sprintf(cronRunsCanonicalDDL, "cron_job_runs")); err != nil {
			return fmt.Errorf("创建 cron_job_runs: %w", err)
		}
	} else {
		for _, column := range []struct{ name, ddl string }{
			{"result", `TEXT NOT NULL DEFAULT ''`},
			{"stdout", `TEXT NOT NULL DEFAULT ''`},
			{"stderr", `TEXT NOT NULL DEFAULT ''`},
			{"exit_code", `INTEGER NOT NULL DEFAULT 0`},
			{"data_json", `TEXT NOT NULL DEFAULT ''`},
		} {
			has, checkErr := cronColumnExists(ctx, q, "cron_job_runs", column.name)
			if checkErr != nil {
				return checkErr
			}
			if !has {
				if _, execErr := q.ExecContext(ctx, fmt.Sprintf(
					`ALTER TABLE cron_job_runs ADD COLUMN %s %s`, column.name, column.ddl)); execErr != nil {
					return fmt.Errorf("新增 cron_job_runs.%s: %w", column.name, execErr)
				}
			}
		}
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_user ON cron_jobs(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_status ON cron_jobs(status,next_run_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_job_runs_job ON cron_job_runs(job_id,run_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_job_runs_job_id ON cron_job_runs(job_id,id)`,
	}
	if createUnique {
		indexes = append(indexes, `CREATE UNIQUE INDEX IF NOT EXISTS idx_cron_jobs_user_name ON cron_jobs(user_id,name)`)
	}
	for _, ddl := range indexes {
		if _, err := q.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("恢复 Cron 索引: %w", err)
		}
	}
	return nil
}

func ensureCronStateTable(ctx context.Context, q cronSQL) error {
	hasState, err := cronTableExists(ctx, q, "cron_job_state")
	if err != nil {
		return err
	}
	if !hasState {
		if _, err := q.ExecContext(ctx, fmt.Sprintf(cronStateCanonicalDDL, "cron_job_state")); err != nil {
			return fmt.Errorf("创建 cron_job_state: %w", err)
		}
		return nil
	}
	for _, column := range []struct{ name, ddl string }{
		{"value", `TEXT NOT NULL DEFAULT ''`},
		{"updated_at", `DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP`},
	} {
		has, checkErr := cronColumnExists(ctx, q, "cron_job_state", column.name)
		if checkErr != nil {
			return checkErr
		}
		if !has {
			if _, execErr := q.ExecContext(ctx, fmt.Sprintf(
				`ALTER TABLE cron_job_state ADD COLUMN %s %s`, column.name, column.ddl)); execErr != nil {
				return fmt.Errorf("新增 cron_job_state.%s: %w", column.name, execErr)
			}
		}
	}
	return nil
}

// RepairCronIntegrityV29 performs the repair on one physical SQLite
// connection. Parent/child table rewrites run with FK enforcement temporarily
// disabled on only that connection, under one transaction, and cannot commit
// until both foreign_key_check and integrity_check are clean.
func RepairCronIntegrityV29(ctx context.Context, db *sql.DB) (retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	foreignKeysInitiallyEnabled, err := cronForeignKeysEnabled(ctx, conn)
	if err != nil {
		return err
	}
	ready, err := canonicalCronSchemaReady(ctx, conn)
	if err != nil {
		return err
	}
	if ready {
		return validateCronDatabase(ctx, conn)
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("临时关闭固定迁移连接的 foreign_keys: %w", err)
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if enableErr := setCronForeignKeys(restoreCtx, conn, foreignKeysInitiallyEnabled); enableErr != nil {
			// Never return a connection with FK enforcement disabled to the pool.
			// driver.ErrBadConn makes database/sql discard this physical handle.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if retErr == nil {
				retErr = enableErr
			} else {
				retErr = errors.Join(retErr, enableErr)
			}
		}
	}()
	var fk int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return err
	}
	if fk != 0 {
		return fmt.Errorf("固定迁移连接 foreign_keys=%d，无法安全重建", fk)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := repairCronIntegrityTx(ctx, tx); err != nil {
		return err
	}
	if err := validateCronDatabase(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 Cron 完整性修复: %w", err)
	}
	return nil
}

func canonicalCronSchemaReady(ctx context.Context, q cronSQL) (bool, error) {
	tables := map[string][]string{
		"cron_jobs": {
			"id", "name", "type", "schedule", "spec_json", "source_prompt", "user_id",
			"platform", "chat_id", "status", "last_run_at", "next_run_at", "run_count", "created_at", "meta",
		},
		"cron_job_runs": {
			"id", "job_id", "status", "result", "error", "duration_ms", "run_at",
			"stdout", "stderr", "exit_code", "data_json",
		},
		"cron_job_state":       {"job_id", "key", "value", "updated_at"},
		"cron_job_merge_audit": {"audit_id", "repair_version", "event_kind", "group_user_id", "group_name", "survivor_job_id", "loser_job_id", "child_key", "payload_json", "created_at"},
	}
	for table, expected := range tables {
		actual, err := cronColumnNames(ctx, q, table)
		if err != nil {
			return false, err
		}
		if !equalCronStrings(actual, expected) {
			return false, nil
		}
	}
	for _, index := range []struct {
		table, name string
		unique      bool
		columns     []string
	}{
		{"cron_jobs", "idx_cron_jobs_user", false, []string{"user_id"}},
		{"cron_jobs", "idx_cron_jobs_status", false, []string{"status", "next_run_at"}},
		{"cron_jobs", "idx_cron_jobs_user_name", true, []string{"user_id", "name"}},
		{"cron_job_runs", "idx_cron_job_runs_job", false, []string{"job_id", "run_at"}},
		{"cron_job_runs", "idx_cron_job_runs_job_id", false, []string{"job_id", "id"}},
	} {
		matches, err := cronIndexMatches(ctx, q, index.table, index.name, index.unique, index.columns)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, nil
		}
	}
	for _, table := range []string{"cron_job_runs", "cron_job_state"} {
		matches, err := cronForeignKeyMatches(ctx, q, table)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, nil
		}
	}
	auditFKs, err := cronForeignKeyCount(ctx, q, "cron_job_merge_audit")
	if err != nil {
		return false, err
	}
	return auditFKs == 0, nil
}

func cronColumnNames(ctx context.Context, q cronSQL, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func cronIndexMatches(ctx context.Context, q cronSQL, table, indexName string, wantUnique bool, wantColumns []string) (bool, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA index_list(`+table+`)`)
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		if name == indexName {
			found = (unique == 1) == wantUnique && partial == 0
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	indexRows, err := q.QueryContext(ctx, `PRAGMA index_info(`+indexName+`)`)
	if err != nil {
		return false, err
	}
	defer indexRows.Close()
	var columns []string
	for indexRows.Next() {
		var seqno, cid int
		var name string
		if err := indexRows.Scan(&seqno, &cid, &name); err != nil {
			return false, err
		}
		columns = append(columns, name)
	}
	return equalCronStrings(columns, wantColumns), indexRows.Err()
}

func cronForeignKeyMatches(ctx context.Context, q cronSQL, table string) (bool, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	count := 0
	matches := false
	for rows.Next() {
		var id, seq int
		var target, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return false, err
		}
		count++
		matches = target == "cron_jobs" && from == "job_id" && to == "id" && strings.EqualFold(onDelete, "CASCADE")
	}
	return count == 1 && matches, rows.Err()
}

func cronForeignKeyCount(ctx context.Context, q cronSQL, table string) (int, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}

func equalCronStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func repairCronIntegrityTx(ctx context.Context, tx *sql.Tx) error {
	if err := ensureCronV2ColumnsInPlace(ctx, tx, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, cronMergeAuditDDL); err != nil {
		return fmt.Errorf("创建 Cron merge audit: %w", err)
	}
	if err := ensureCronStateTable(ctx, tx); err != nil {
		return err
	}
	if err := repairCronDuplicateGroups(ctx, tx); err != nil {
		return err
	}
	return rebuildCanonicalCronTables(ctx, tx)
}

func repairCronDuplicateGroups(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,name,type,schedule,spec_json,source_prompt,user_id,
		platform,chat_id,status,last_run_at,next_run_at,run_count,created_at,meta FROM cron_jobs`)
	if err != nil {
		return fmt.Errorf("读取 Cron 修复候选: %w", err)
	}
	groups := make(map[string][]cronJobRow)
	for rows.Next() {
		job, scanErr := scanCronJobRow(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		key := job.UserID + "\x00" + job.Name
		groups[key] = append(groups[key], job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	groupKeys := make([]string, 0, len(groups))
	for key, jobs := range groups {
		if len(jobs) > 1 {
			groupKeys = append(groupKeys, key)
		}
	}
	sort.Strings(groupKeys)
	for _, key := range groupKeys {
		jobs := groups[key]
		sort.Slice(jobs, func(i, j int) bool { return cronJobLess(jobs[i], jobs[j]) })
		sourceKeys := make(map[string]struct{})
		for _, job := range jobs {
			if sourceKey := cronSourceKey(job.Meta); sourceKey != "" {
				sourceKeys[sourceKey] = struct{}{}
			}
		}
		if len(sourceKeys) <= 1 {
			loserIDs := make([]string, 0, len(jobs)-1)
			for _, loser := range jobs[1:] {
				loserIDs = append(loserIDs, loser.ID)
			}
			if err := MergeCronJobsTx(ctx, tx, jobs[0].ID, loserIDs, "v29_same_user_name"); err != nil {
				return err
			}
			continue
		}
		if err := quarantineCronSourceKeyConflict(ctx, tx, jobs); err != nil {
			return err
		}
	}
	return nil
}

func quarantineCronSourceKeyConflict(ctx context.Context, tx *sql.Tx, jobs []cronJobRow) error {
	if len(jobs) < 2 {
		return nil
	}
	usedNames, err := cronUserNames(ctx, tx, jobs[0].UserID)
	if err != nil {
		return err
	}
	for index, job := range jobs {
		if err := insertCronAudit(ctx, tx, "source_key_conflict", jobs[0].ID, job, "", "v29_distinct_source_keys"); err != nil {
			return err
		}
		newName := job.Name
		if index > 0 {
			base := job.Name + " · 隔离 · " + job.ID
			newName = base
			for suffix := 2; ; suffix++ {
				if _, exists := usedNames[newName]; !exists {
					break
				}
				newName = base + "-" + strconv.Itoa(suffix)
			}
			usedNames[newName] = struct{}{}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET status='paused',name=? WHERE id=?`, newName, job.ID); err != nil {
			return fmt.Errorf("隔离 Cron SourceKey 冲突 %s: %w", job.ID, err)
		}
	}
	return nil
}

func cronUserNames(ctx context.Context, tx *sql.Tx, userID string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM cron_jobs WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = struct{}{}
	}
	return names, rows.Err()
}

// MergeCronJobsTx preserves every run ID/payload and every non-conflicting
// state row before deleting a duplicate parent. Conflicting state keeps the
// deterministic survivor value and stores the complete loser row in the
// no-FK audit ledger. Callers must hold their lifecycle lock when applicable.
func MergeCronJobsTx(ctx context.Context, tx *sql.Tx, survivorID string, loserIDs []string, reason string) error {
	if len(loserIDs) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, cronMergeAuditDDL); err != nil {
		return fmt.Errorf("创建 Cron merge audit: %w", err)
	}
	survivor, err := readCronJobByID(ctx, tx, survivorID)
	if err != nil {
		return err
	}
	mergedRunCount := survivor.RunCount
	mergedLastRunAt := survivor.LastRunAt
	seen := make(map[string]struct{}, len(loserIDs))
	losers := make([]cronJobRow, 0, len(loserIDs))
	for _, loserID := range loserIDs {
		if loserID == survivorID {
			return fmt.Errorf("Cron merge loser equals survivor %q", survivorID)
		}
		if _, duplicate := seen[loserID]; duplicate {
			continue
		}
		seen[loserID] = struct{}{}
		loser, err := readCronJobByID(ctx, tx, loserID)
		if err != nil {
			return err
		}
		mergedRunCount, err = checkedCronRunCountAdd(mergedRunCount, loser.RunCount)
		if err != nil {
			return fmt.Errorf("Cron merge run_count overflow (%s→%s): %w", loserID, survivorID, err)
		}
		mergedLastRunAt, err = maxCronLastRunAt(mergedLastRunAt, loser.LastRunAt)
		if err != nil {
			return fmt.Errorf("Cron merge last_run_at (%s→%s): %w", loserID, survivorID, err)
		}
		losers = append(losers, loser)
	}

	for _, loser := range losers {
		loserID := loser.ID
		if err := insertCronAudit(ctx, tx, "merge_parent", survivorID, loser, "", reason); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cron_job_runs SET job_id=? WHERE job_id=?`, survivorID, loserID); err != nil {
			return fmt.Errorf("迁移 Cron run %s→%s: %w", loserID, survivorID, err)
		}
		if err := mergeCronState(ctx, tx, survivorID, loser, reason); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=?`, loserID); err != nil {
			return fmt.Errorf("删除已清空的 Cron duplicate %s: %w", loserID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET run_count=?,last_run_at=? WHERE id=?`,
		mergedRunCount, mergedLastRunAt, survivorID); err != nil {
		return fmt.Errorf("汇总 Cron parent 运行统计 %s: %w", survivorID, err)
	}
	return nil
}

func checkedCronRunCountAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("int64 positive overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, errors.New("int64 negative overflow")
	}
	return left + right, nil
}

func maxCronLastRunAt(current, candidate any) (any, error) {
	currentAbsent := cronLastRunAtAbsent(current)
	candidateAbsent := cronLastRunAtAbsent(candidate)
	if currentAbsent && candidateAbsent {
		return nil, nil
	}
	if currentAbsent {
		if _, ok := normalizedCronCreatedAt(candidate); !ok {
			return nil, errors.New("candidate timestamp is not parseable")
		}
		return candidate, nil
	}
	currentTime, currentOK := normalizedCronCreatedAt(current)
	if !currentOK {
		return nil, errors.New("survivor timestamp is not parseable")
	}
	if candidateAbsent {
		return current, nil
	}
	candidateTime, candidateOK := normalizedCronCreatedAt(candidate)
	if !candidateOK {
		return nil, errors.New("candidate timestamp is not parseable")
	}
	if candidateTime.After(currentTime) {
		return candidate, nil
	}
	return current, nil
}

func cronLastRunAtAbsent(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case time.Time:
		return typed.IsZero()
	case string:
		return strings.TrimSpace(typed) == ""
	case []byte:
		return strings.TrimSpace(string(typed)) == ""
	case int64:
		return typed <= 0
	case float64:
		return typed <= 0
	default:
		return false
	}
}

func mergeCronState(ctx context.Context, tx *sql.Tx, survivorID string, loser cronJobRow, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT key,value,updated_at FROM cron_job_state WHERE job_id=? ORDER BY key`, loser.ID)
	if err != nil {
		return err
	}
	type stateRow struct {
		Key, Value string
		UpdatedAt  any
	}
	var states []stateRow
	for rows.Next() {
		var state stateRow
		if err := rows.Scan(&state.Key, &state.Value, &state.UpdatedAt); err != nil {
			rows.Close()
			return err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, state := range states {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_state WHERE job_id=? AND key=?`,
			survivorID, state.Key).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE cron_job_state SET job_id=? WHERE job_id=? AND key=?`,
				survivorID, loser.ID, state.Key); err != nil {
				return fmt.Errorf("迁移 Cron state %s/%s: %w", loser.ID, state.Key, err)
			}
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"job_id": loser.ID, "key": state.Key, "value": state.Value,
			"updated_at": cronJSONValue(state.UpdatedAt), "reason": reason,
		})
		if err != nil {
			return err
		}
		if err := insertCronAuditPayload(ctx, tx, "state_conflict", survivorID, loser,
			state.Key, string(payload)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cron_job_state WHERE job_id=? AND key=?`, loser.ID, state.Key); err != nil {
			return fmt.Errorf("删除已审计 Cron state conflict %s/%s: %w", loser.ID, state.Key, err)
		}
	}
	return nil
}

func insertCronAudit(ctx context.Context, tx *sql.Tx, eventKind, survivorID string, loser cronJobRow, childKey, reason string) error {
	payload, err := json.Marshal(map[string]any{
		"id": loser.ID, "name": loser.Name, "type": loser.Type, "schedule": loser.Schedule,
		"spec_json": loser.SpecJSON, "source_prompt": loser.SourcePrompt,
		"user_id": loser.UserID, "platform": loser.Platform, "chat_id": loser.ChatID,
		"status": loser.Status, "last_run_at": cronJSONValue(loser.LastRunAt),
		"next_run_at": cronJSONValue(loser.NextRunAt), "run_count": loser.RunCount,
		"created_at": cronJSONValue(loser.CreatedAt), "meta": loser.Meta, "reason": reason,
	})
	if err != nil {
		return err
	}
	return insertCronAuditPayload(ctx, tx, eventKind, survivorID, loser, childKey, string(payload))
}

func insertCronAuditPayload(ctx context.Context, tx *sql.Tx, eventKind, survivorID string, loser cronJobRow, childKey, payload string) error {
	auditID := cronAuditID(eventKind, survivorID, loser.ID, childKey)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cron_job_merge_audit
		(audit_id,repair_version,event_kind,group_user_id,group_name,survivor_job_id,loser_job_id,child_key,payload_json)
		VALUES (?,29,?,?,?,?,?,?,?)`, auditID, eventKind, loser.UserID, loser.Name,
		survivorID, loser.ID, childKey, payload)
	if err != nil {
		return fmt.Errorf("写 Cron merge audit %s: %w", eventKind, err)
	}
	return nil
}

func cronAuditID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return "cron-v29-" + hex.EncodeToString(h.Sum(nil))
}

func rebuildCanonicalCronTables(ctx context.Context, tx *sql.Tx) error {
	oldSequence := int64(0)
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='cron_job_runs'`).Scan(&oldSequence); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("读取 cron_job_runs sequence: %w", err)
	}
	for _, table := range []string{"cron_job_runs_v29_new", "cron_job_state_v29_new", "cron_jobs_v29_new"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return err
		}
	}
	for _, ddl := range []string{
		fmt.Sprintf(cronJobsCanonicalDDL, "cron_jobs_v29_new"),
		fmt.Sprintf(cronRunsCanonicalDDL, "cron_job_runs_v29_new"),
		fmt.Sprintf(cronStateCanonicalDDL, "cron_job_state_v29_new"),
	} {
		// Child DDL points to canonical cron_jobs, which is installed before the
		// transaction is validated and committed.
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("创建 canonical Cron 临时表: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cron_jobs_v29_new
		(id,name,type,schedule,spec_json,source_prompt,user_id,platform,chat_id,status,
		 last_run_at,next_run_at,run_count,created_at,meta)
		SELECT id,name,COALESCE(type,'cron'),schedule,COALESCE(spec_json,''),COALESCE(source_prompt,''),
		       user_id,COALESCE(platform,''),COALESCE(chat_id,''),COALESCE(status,'paused'),last_run_at,
		       COALESCE(next_run_at,'9999-12-31T23:59:59Z'),COALESCE(run_count,0),
		       COALESCE(created_at,'9999-12-31T23:59:59Z'),COALESCE(meta,'{}')
		FROM cron_jobs`); err != nil {
		return fmt.Errorf("复制 cron_jobs canonical 数据: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cron_job_runs_v29_new
		(id,job_id,status,result,error,duration_ms,run_at,stdout,stderr,exit_code,data_json)
		SELECT id,job_id,COALESCE(status,'success'),COALESCE(result,''),COALESCE(error,''),
		       COALESCE(duration_ms,0),COALESCE(run_at,'9999-12-31T23:59:59Z'),COALESCE(stdout,''),
		       COALESCE(stderr,''),COALESCE(exit_code,0),COALESCE(data_json,'') FROM cron_job_runs`); err != nil {
		return fmt.Errorf("复制 cron_job_runs canonical 数据: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cron_job_state_v29_new(job_id,key,value,updated_at)
		SELECT job_id,key,COALESCE(value,''),COALESCE(updated_at,'9999-12-31T23:59:59Z') FROM cron_job_state`); err != nil {
		return fmt.Errorf("复制 cron_job_state canonical 数据: %w", err)
	}
	for _, ddl := range []string{
		`DROP TABLE cron_job_runs`, `DROP TABLE cron_job_state`, `DROP TABLE cron_jobs`,
		`ALTER TABLE cron_jobs_v29_new RENAME TO cron_jobs`,
		`ALTER TABLE cron_job_runs_v29_new RENAME TO cron_job_runs`,
		`ALTER TABLE cron_job_state_v29_new RENAME TO cron_job_state`,
		`CREATE INDEX idx_cron_jobs_user ON cron_jobs(user_id)`,
		`CREATE INDEX idx_cron_jobs_status ON cron_jobs(status,next_run_at)`,
		`CREATE UNIQUE INDEX idx_cron_jobs_user_name ON cron_jobs(user_id,name)`,
		`CREATE INDEX idx_cron_job_runs_job ON cron_job_runs(job_id,run_at)`,
		`CREATE INDEX idx_cron_job_runs_job_id ON cron_job_runs(job_id,id)`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("安装 canonical Cron schema (%s): %w", ddl, err)
		}
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM cron_job_runs`).Scan(&maxID); err != nil {
		return err
	}
	if oldSequence > maxID {
		maxID = oldSequence
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='cron_job_runs'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name,seq) VALUES ('cron_job_runs',?)`, maxID); err != nil {
		return fmt.Errorf("恢复 cron_job_runs sequence: %w", err)
	}
	return nil
}

func validateCronDatabase(ctx context.Context, q cronSQL) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return err
		}
		return fmt.Errorf("foreign_key_check failed: table=%s parent=%s fk=%d", table, parent, fkID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var integrity string
	if err := q.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("integrity_check failed: %s", integrity)
	}
	return nil
}

func enableCronForeignKeys(ctx context.Context, conn *sql.Conn) error {
	return setCronForeignKeys(ctx, conn, true)
}

func cronForeignKeysEnabled(ctx context.Context, conn *sql.Conn) (bool, error) {
	var enabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return false, err
	}
	return enabled == 1, nil
}

func setCronForeignKeys(ctx context.Context, conn *sql.Conn, enabled bool) error {
	value := "OFF"
	want := 0
	if enabled {
		value = "ON"
		want = 1
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=`+value); err != nil {
		return fmt.Errorf("设置 fixed connection foreign_keys=%s: %w", value, err)
	}
	var actual int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&actual); err != nil {
		return err
	}
	if actual != want {
		return fmt.Errorf("fixed connection foreign_keys=%d, want %d", actual, want)
	}
	return nil
}

func cronTableExists(ctx context.Context, q cronSQL, table string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count)
	return count == 1, err
}

func cronColumnExists(ctx context.Context, q cronSQL, table, column string) (bool, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func readCronJobByID(ctx context.Context, q cronSQL, id string) (cronJobRow, error) {
	row := q.QueryRowContext(ctx, `SELECT id,name,type,schedule,spec_json,source_prompt,user_id,
		platform,chat_id,status,last_run_at,next_run_at,run_count,created_at,meta FROM cron_jobs WHERE id=?`, id)
	job, err := scanCronJobRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return cronJobRow{}, fmt.Errorf("Cron merge loser %q does not exist", id)
	}
	return job, err
}

type cronRowScanner interface{ Scan(...any) error }

func scanCronJobRow(row cronRowScanner) (cronJobRow, error) {
	var job cronJobRow
	var jobType, specJSON, sourcePrompt, platform, chatID, status, meta sql.NullString
	var runCount sql.NullInt64
	if err := row.Scan(&job.ID, &job.Name, &jobType, &job.Schedule, &specJSON, &sourcePrompt,
		&job.UserID, &platform, &chatID, &status, &job.LastRunAt, &job.NextRunAt,
		&runCount, &job.CreatedAt, &meta); err != nil {
		return cronJobRow{}, err
	}
	job.Type = jobType.String
	job.SpecJSON = specJSON.String
	job.SourcePrompt = sourcePrompt.String
	job.Platform = platform.String
	job.ChatID = chatID.String
	job.Status = status.String
	job.RunCount = runCount.Int64
	job.Meta = meta.String
	return job, nil
}

func cronSourceKey(metaJSON string) string {
	var meta struct {
		SourceKey string `json:"source_key"`
	}
	if json.Unmarshal([]byte(metaJSON), &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.SourceKey)
}

func cronJobLess(left, right cronJobRow) bool {
	leftTime, leftKnown := normalizedCronCreatedAt(left.CreatedAt)
	rightTime, rightKnown := normalizedCronCreatedAt(right.CreatedAt)
	if leftKnown != rightKnown {
		return leftKnown
	}
	if leftKnown && !leftTime.Equal(rightTime) {
		return leftTime.Before(rightTime)
	}
	return left.ID < right.ID
}

func normalizedCronCreatedAt(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		if typed.IsZero() {
			return time.Time{}, false
		}
		return typed.UTC(), true
	case int64:
		return normalizedCronUnix(typed)
	case float64:
		return normalizedCronUnix(int64(typed))
	case []byte:
		return normalizedCronCreatedAtString(string(typed))
	case string:
		return normalizedCronCreatedAtString(typed)
	default:
		return time.Time{}, false
	}
}

func normalizedCronUnix(value int64) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}
	if value > 10_000_000_000 {
		return time.UnixMilli(value).UTC(), true
	}
	return time.Unix(value, 0).UTC(), true
}

func normalizedCronCreatedAtString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if unixValue, err := strconv.ParseInt(value, 10, 64); err == nil {
		return normalizedCronUnix(unixValue)
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func cronJSONValue(value any) any {
	switch typed := value.(type) {
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	case []byte:
		return string(typed)
	default:
		return value
	}
}
