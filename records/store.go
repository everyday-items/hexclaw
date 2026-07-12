package records

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// isForeignKeyErr 判断是否 agent_name 外键约束失败（隔离键指向的 agent 未注册）。
func isForeignKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

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
			return false, fmt.Errorf("%w: 记录集 %q: %v", ErrInvalidFields, r.Collection, err)
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
		if isForeignKeyErr(err) {
			// BUG-20260712-#2：agent_name 外键指向的 agent 未注册 → 语义错误 + 清晰提示，
			// 不再把原始 "FOREIGN KEY constraint failed" 甩成 500。
			return false, fmt.Errorf("%w: agent %q 未注册（请先创建该辅导助手再记录）", ErrScopeNotFound, r.AgentName)
		}
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
		r.AgentName, r.Collection, r.DedupeKey).Scan(&existingID); qErr != nil {
		return false, fmt.Errorf("records: 去重命中后回查记录失败: %w", qErr)
	}
	r.RecordID = existingID
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

// Delete 按 record_id 删除一条记录，**始终按 agent_name 圈定归属**（多孩隔离硬边界）——
// 仅删属于该实例的记录，避免仅凭 record_id 跨实例删除。record 不存在 / 不属于该 agent
// 均按 ErrNotFound 返回（幂等语义：调用方据此回 404）。agentName 空 = ErrNotFound。
//
// 通用数据纠错原语（场景包按自己语义解读，如 K12「删除记错/重复的错题」）。
func (s *Store) Delete(ctx context.Context, agentName, recordID string) error {
	if agentName == "" || recordID == "" {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_records WHERE record_id = ? AND agent_name = ?`, recordID, agentName)
	if err != nil {
		return fmt.Errorf("records: 删除失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
	return s.updateStatusScoped(ctx, "", recordID, newStatus, dueAt, expectedVersion)
}

// UpdateStatusScoped 与 UpdateStatus 相同，但把 agentName 同时放进 CAS WHERE，
// 避免仅凭 record_id 跨实例推进记录状态。scope 不匹配按不存在返回。
func (s *Store) UpdateStatusScoped(ctx context.Context, agentName, recordID, newStatus string, dueAt *int64, expectedVersion int) error {
	if agentName == "" {
		return ErrNotFound
	}
	return s.updateStatusScoped(ctx, agentName, recordID, newStatus, dueAt, expectedVersion)
}

func (s *Store) updateStatusScoped(ctx context.Context, agentName, recordID, newStatus string, dueAt *int64, expectedVersion int) error {
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
	if agentName != "" && cur.AgentName != agentName {
		return ErrNotFound
	}

	query := `
UPDATE agent_records
   SET status = ?, due_at = ?, version = version + 1, updated_at = ?
 WHERE record_id = ? AND version = ?`
	args := []any{newStatus, dueAt, nowUnix(), recordID, expectedVersion}
	if agentName != "" {
		query += ` AND agent_name = ?`
		args = append(args, agentName)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("records: 更新失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if agentName != "" {
			var exists int
			if qErr := s.db.QueryRowContext(ctx, `SELECT 1 FROM agent_records WHERE record_id = ? AND agent_name = ?`, recordID, agentName).Scan(&exists); qErr == sql.ErrNoRows {
				return ErrNotFound
			}
		}
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
			return fmt.Errorf("%w: 记录集 %q: %v", ErrInvalidFields, cur.Collection, err)
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
    version=excluded.version, created_at=excluded.created_at, updated_at=excluded.updated_at
WHERE agent_records.agent_name = excluded.agent_name`

// execer 抽象 *sql.DB 与 *sql.Tx，让导入 SQL 在直连或事务内共用。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func importRecordVia(ctx context.Context, e execer, r *AgentRecord) error {
	res, err := e.ExecContext(ctx, importRecordSQL,
		r.RecordID, r.AgentName, r.Collection, r.SchemaVersion, r.Status, r.Fields,
		r.DedupeKey, r.Tags, r.DueAt, r.SourceSession, r.Version, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if n == 0 {
		return fmt.Errorf("records: record_id %q 已属于其他实例", r.RecordID)
	}
	return nil
}

func (s *Store) validateImportRecord(r *AgentRecord, expectedAgent string) error {
	if r == nil {
		return fmt.Errorf("%w: 导入记录不可为 nil", ErrInvalidRecord)
	}
	if r.RecordID == "" || r.AgentName == "" || r.Collection == "" {
		return fmt.Errorf("%w: 导入记录缺少 record_id / agent_name / collection", ErrInvalidRecord)
	}
	if expectedAgent != "" && r.AgentName != expectedAgent {
		return fmt.Errorf("%w: 导入记录 %q 不属于实例 %q", ErrInvalidRecord, r.RecordID, expectedAgent)
	}
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return err
	}
	if r.SchemaVersion <= 0 || r.SchemaVersion > schema.Version {
		return fmt.Errorf("%w: 记录 %q schema v%d 超出 %q 支持的 v%d", ErrInvalidRecord, r.RecordID, r.SchemaVersion, r.Collection, schema.Version)
	}
	if !schema.hasStatus(r.Status) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", ErrInvalidStatus, r.Status, r.Collection)
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(r.Fields); err != nil {
			return fmt.Errorf("%w: 记录集 %q: %v", ErrInvalidFields, r.Collection, err)
		}
	}
	return nil
}

// ImportRecord 原样导入一条记录（恢复用，保留 record_id/status/时间戳等全部字段）。
// 导入前校验注册 schema/status/fields；record_id 只允许覆盖同 agent 记录。
func (s *Store) ImportRecord(ctx context.Context, r *AgentRecord) error {
	if err := s.validateImportRecord(r, ""); err != nil {
		return err
	}
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
	for _, r := range recs {
		if err := s.validateImportRecord(r, ""); err != nil {
			return err
		}
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

// ImportAgentRecordsTx atomically merges one archive batch into an existing
// agent through a caller-owned transaction. Records absent from the archive are
// preserved; equal record_id values are idempotently replaced by the imported
// value. The full batch is validated against agentName before the first write.
func (s *Store) ImportAgentRecordsTx(ctx context.Context, tx *sql.Tx, agentName string, recs []*AgentRecord) error {
	if tx == nil {
		return fmt.Errorf("records: nil merge transaction")
	}
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	for _, r := range recs {
		if err := importRecordVia(ctx, tx, r); err != nil {
			return fmt.Errorf("records: 合并记录 %q: %w", r.RecordID, err)
		}
	}
	return nil
}

// ImportAgentRecords merges a complete agent-scoped archive batch in one
// transaction. It implements the restore contract: add missing records, replace
// duplicate record IDs with the imported value, and retain unrelated current
// records.
func (s *Store) ImportAgentRecords(ctx context.Context, agentName string, recs []*AgentRecord) error {
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: 开启合并事务: %w", err)
	}
	defer tx.Rollback()
	if err := s.ImportAgentRecordsTx(ctx, tx, agentName, recs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交合并事务: %w", err)
	}
	return nil
}

// ValidateAgentRecords validates a complete agent-scoped restore batch without
// mutating storage. Atomic cross-subsystem restore adapters use this as their
// preflight before acquiring other subsystem locks and opening a shared tx.
func (s *Store) ValidateAgentRecords(agentName string, recs []*AgentRecord) error {
	if agentName == "" {
		return fmt.Errorf("%w: agentName 不可空", ErrInvalidRecord)
	}
	for _, r := range recs {
		if err := s.validateImportRecord(r, agentName); err != nil {
			return err
		}
	}
	return nil
}

func replaceAgentRecordsVia(ctx context.Context, e execer, agentName string, recs []*AgentRecord) error {
	if _, err := e.ExecContext(ctx, `DELETE FROM agent_records WHERE agent_name = ?`, agentName); err != nil {
		return fmt.Errorf("records: 清理实例旧记录: %w", err)
	}
	for _, r := range recs {
		if err := importRecordVia(ctx, e, r); err != nil {
			return fmt.Errorf("records: 替换记录 %q: %w", r.RecordID, err)
		}
	}
	return nil
}

// ReplaceAgentRecordsTx replaces one agent's records through a caller-owned
// transaction. This lets a composition adapter commit records together with
// another SQLite-backed aggregate (for example agents.metadata).
func (s *Store) ReplaceAgentRecordsTx(ctx context.Context, tx *sql.Tx, agentName string, recs []*AgentRecord) error {
	if tx == nil {
		return fmt.Errorf("records: nil replace transaction")
	}
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	return replaceAgentRecordsVia(ctx, tx, agentName, recs)
}

// ReplaceAgentRecords 在单事务内用归档记录替换指定实例的全部记录。
// 整批先校验 agent/schema/status/fields，任一不合法时不会删除现有数据。
func (s *Store) ReplaceAgentRecords(ctx context.Context, agentName string, recs []*AgentRecord) error {
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: 开启替换事务: %w", err)
	}
	defer tx.Rollback()
	if err := replaceAgentRecordsVia(ctx, tx, agentName, recs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交替换事务: %w", err)
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
