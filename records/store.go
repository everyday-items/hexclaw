package records

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Store agent_records 的 SQLite 存储。
//
// 依赖 RecordSchemaRegistry 拿记录集规则（状态机/去重键/字段校验），
// 自身零业务条件——它只做「按 schema 声明存取通用记录」。
type Store struct {
	db       *sql.DB
	registry *RecordSchemaRegistry
}

// NewStore 创建存储。db 需已跑过 migrate（含 v8 agent_records 表）。
func NewStore(db *sql.DB, registry *RecordSchemaRegistry) *Store {
	return &Store{db: db, registry: registry}
}

// Put 幂等写入一条记录。
//
// 流程：查 schema → 校验字段 → 定 status（空则 InitialStatus，非空须 ∈ 状态机）→
// 派生 dedupe_key → INSERT ... ON CONFLICT(agent_name,collection,dedupe_key) DO NOTHING。
// 返回 created：true=新建，false=去重命中（同实例+记录集已有同 dedupe_key 的记录，幂等跳过）。
//
// 幂等语义防「同一条记录反复写入膨胀」（对标 kb_chunks UNIQUE 收口）。
func (s *Store) Put(ctx context.Context, r *AgentRecord) (created bool, err error) {
	if r.AgentName == "" {
		return false, fmt.Errorf("records: AgentName 不可空（隔离键）")
	}
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(r.Fields); err != nil {
			return false, fmt.Errorf("records: 记录集 %q 字段校验失败: %w", r.Collection, err)
		}
	}

	if r.Status == "" {
		r.Status = schema.InitialStatus
	} else if !schema.hasStatus(r.Status) {
		return false, fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", ErrInvalidStatus, r.Status, r.Collection)
	}

	r.DedupeKey = schema.DedupeKey(r)
	r.SchemaVersion = schema.Version
	if r.RecordID == "" {
		r.RecordID = idgen.NanoID()
	}
	now := nowUnix()
	r.CreatedAt, r.UpdatedAt, r.Version = now, now, 0
	if r.Fields == "" {
		r.Fields = "{}"
	}
	if r.Tags == "" {
		r.Tags = "[]"
	}

	res, err := s.db.ExecContext(ctx, `
INSERT INTO agent_records
    (record_id, agent_name, collection, schema_version, status, fields_json,
     dedupe_key, tags_json, due_at, source_session_id, version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_name, collection, dedupe_key) DO NOTHING`,
		r.RecordID, r.AgentName, r.Collection, r.SchemaVersion, r.Status, r.Fields,
		r.DedupeKey, r.Tags, r.DueAt, r.SourceSession, r.Version, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("records: 写入失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// 去重命中（ON CONFLICT DO NOTHING）：把 r.RecordID 回填为**已存在记录**的 ID，
	// 否则调用方拿到的是从未入库的新 NanoID（Get 不到，前端 mark-mastered/详情全失败）。
	var existingID string
	if qErr := s.db.QueryRowContext(ctx,
		`SELECT record_id FROM agent_records WHERE agent_name = ? AND collection = ? AND dedupe_key = ?`,
		r.AgentName, r.Collection, r.DedupeKey).Scan(&existingID); qErr == nil {
		r.RecordID = existingID
	}
	return false, nil
}

// FindDuplicate 按记录集 schema 的去重键查找同实例内的既有记录（无则 ErrNotFound）。
// 用于「答对时推进同题错题」等需要按业务去重键回查的场景（不新增记录，只定位既有）。
func (s *Store) FindDuplicate(ctx context.Context, r *AgentRecord) (*AgentRecord, error) {
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return nil, err
	}
	key := schema.DedupeKey(r)
	rows, err := s.queryRecords(ctx, selectCols+
		` WHERE agent_name = ? AND collection = ? AND dedupe_key = ? LIMIT 1`,
		r.AgentName, r.Collection, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

// Get 按 record_id 取记录。
func (s *Store) Get(ctx context.Context, recordID string) (*AgentRecord, error) {
	row := s.db.QueryRowContext(ctx, selectCols+` WHERE record_id = ?`, recordID)
	return scanRecord(row.Scan)
}

// ListByScope 按 实例 + 记录集 (+ 可选状态) 列出记录。
//
// status 空串 = 该记录集全部状态。**始终按 agent_name 过滤 = 多孩隔离硬边界**。
func (s *Store) ListByScope(ctx context.Context, agentName, collection, status string) ([]*AgentRecord, error) {
	q := selectCols + ` WHERE agent_name = ? AND collection = ?`
	args := []any{agentName, collection}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, record_id`
	return s.queryRecords(ctx, q, args...)
}

// ListDue 到期队列：某实例+记录集内 due_at ≤ before 的记录，按到期升序。
//
// 通用到期/间隔重排调度原语（场景包按自己的语义解读，如"到期该练"）。仅返回有 due_at 的记录。
func (s *Store) ListDue(ctx context.Context, agentName, collection string, before int64) ([]*AgentRecord, error) {
	q := selectCols + ` WHERE agent_name = ? AND collection = ? AND due_at IS NOT NULL AND due_at <= ?
	                     ORDER BY due_at ASC, record_id`
	return s.queryRecords(ctx, q, agentName, collection, before)
}

// UpdateStatus 乐观锁推进状态。
//
// expectedVersion 与库中不符 → ErrVersionConflict（防 IM + 桌面并发写丢更新）。
// newStatus 必须 ∈ 记录集状态机。可同时更新 due_at（nil = 清空到期，移出到期队列）。
func (s *Store) UpdateStatus(ctx context.Context, recordID, newStatus string, dueAt *int64, expectedVersion int) error {
	cur, err := s.Get(ctx, recordID)
	if err != nil {
		return err
	}
	schema, err := s.registry.Get(cur.Collection)
	if err != nil {
		return err
	}
	if !schema.hasStatus(newStatus) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", ErrInvalidStatus, newStatus, cur.Collection)
	}
	if !schema.canTransition(cur.Status, newStatus) {
		return fmt.Errorf("%w: 记录集 %q 不允许 %q→%q", ErrIllegalTransition, cur.Collection, cur.Status, newStatus)
	}

	res, err := s.db.ExecContext(ctx, `
UPDATE agent_records
   SET status = ?, due_at = ?, version = version + 1, updated_at = ?
 WHERE record_id = ? AND version = ?`,
		newStatus, dueAt, nowUnix(), recordID, expectedVersion)
	if err != nil {
		return fmt.Errorf("records: 更新失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrVersionConflict
	}
	return nil
}

// UpdateStatusFields 同 UpdateStatus，但**同时替换领域字段**（fields_json）——供状态流转时
// 需改写领域字段的通用场景（如间隔重排把复习轮次 review_stage 写回卡片）。乐观锁与状态机、
// 字段校验规则同 Put/UpdateStatus。fieldsJSON 走记录集 schema 的 ValidateFields（若声明）。
func (s *Store) UpdateStatusFields(ctx context.Context, recordID, newStatus string, dueAt *int64, fieldsJSON string, expectedVersion int) error {
	cur, err := s.Get(ctx, recordID)
	if err != nil {
		return err
	}
	schema, err := s.registry.Get(cur.Collection)
	if err != nil {
		return err
	}
	if !schema.hasStatus(newStatus) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", ErrInvalidStatus, newStatus, cur.Collection)
	}
	if !schema.canTransition(cur.Status, newStatus) {
		return fmt.Errorf("%w: 记录集 %q 不允许 %q→%q", ErrIllegalTransition, cur.Collection, cur.Status, newStatus)
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(fieldsJSON); err != nil {
			return fmt.Errorf("records: 记录集 %q 字段校验失败: %w", cur.Collection, err)
		}
	}

	res, err := s.db.ExecContext(ctx, `
UPDATE agent_records
   SET status = ?, due_at = ?, fields_json = ?, version = version + 1, updated_at = ?
 WHERE record_id = ? AND version = ?`,
		newStatus, dueAt, fieldsJSON, nowUnix(), recordID, expectedVersion)
	if err != nil {
		return fmt.Errorf("records: 更新失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrVersionConflict
	}
	return nil
}

// ExportAgent 导出某实例的**全部**记录（跨记录集），供备份（.hexbak）。
func (s *Store) ExportAgent(ctx context.Context, agentName string) ([]*AgentRecord, error) {
	return s.queryRecords(ctx, selectCols+` WHERE agent_name = ? ORDER BY collection, created_at, record_id`, agentName)
}

// importRecordSQL 原样 upsert 一条记录的 SQL（ImportRecord / ImportRecords 共用）。
const importRecordSQL = `
INSERT INTO agent_records
    (record_id, agent_name, collection, schema_version, status, fields_json,
     dedupe_key, tags_json, due_at, source_session_id, version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(record_id) DO UPDATE SET
    agent_name=excluded.agent_name, collection=excluded.collection, schema_version=excluded.schema_version,
    status=excluded.status, fields_json=excluded.fields_json, dedupe_key=excluded.dedupe_key,
    tags_json=excluded.tags_json, due_at=excluded.due_at, source_session_id=excluded.source_session_id,
    version=excluded.version, created_at=excluded.created_at, updated_at=excluded.updated_at`

// execer 抽象 *sql.DB 与 *sql.Tx，让导入 SQL 在直连或事务内共用。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func importRecordVia(ctx context.Context, e execer, r *AgentRecord) error {
	_, err := e.ExecContext(ctx, importRecordSQL,
		r.RecordID, r.AgentName, r.Collection, r.SchemaVersion, r.Status, r.Fields,
		r.DedupeKey, r.Tags, r.DueAt, r.SourceSession, r.Version, r.CreatedAt, r.UpdatedAt)
	return err
}

// ImportRecord 原样导入一条记录（恢复用，保留 record_id/status/时间戳等全部字段）。
//
// 与 Put 不同：不走 dedupe/schema 校验，逐字段还原；record_id 冲突则整条覆盖（幂等恢复）。
func (s *Store) ImportRecord(ctx context.Context, r *AgentRecord) error {
	if err := importRecordVia(ctx, s.db, r); err != nil {
		return fmt.Errorf("records: 导入失败: %w", err)
	}
	return nil
}

// ImportRecords 单事务原子导入一批记录（恢复用）：任一条失败整批回滚，绝不部分导入
// （T1.2：PRD §3.12.8「不部分导入」贯彻到写库中途故障，而非仅前置 checksum 挡文件损坏）。
func (s *Store) ImportRecords(ctx context.Context, recs []*AgentRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: 开启导入事务: %w", err)
	}
	defer tx.Rollback() // 已 Commit 后为 no-op；任一步失败在此回滚
	for _, r := range recs {
		if err := importRecordVia(ctx, tx, r); err != nil {
			return fmt.Errorf("records: 导入记录 %s: %w", r.RecordID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交导入事务: %w", err)
	}
	return nil
}

const selectCols = `SELECT record_id, agent_name, collection, schema_version, status, fields_json,
    dedupe_key, tags_json, due_at, source_session_id, version, created_at, updated_at
    FROM agent_records`

func (s *Store) queryRecords(ctx context.Context, q string, args ...any) ([]*AgentRecord, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("records: 查询失败: %w", err)
	}
	defer rows.Close()
	var out []*AgentRecord
	for rows.Next() {
		r, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanRecord 把一行扫进 AgentRecord。scan 抽象成函数适配 *sql.Row 与 *sql.Rows。
func scanRecord(scan func(dest ...any) error) (*AgentRecord, error) {
	var r AgentRecord
	err := scan(&r.RecordID, &r.AgentName, &r.Collection, &r.SchemaVersion, &r.Status, &r.Fields,
		&r.DedupeKey, &r.Tags, &r.DueAt, &r.SourceSession, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("records: 扫描失败: %w", err)
	}
	return &r, nil
}
