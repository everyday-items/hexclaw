// Package k12storage 是 K12 类型化 canonical store（架构设计-v0.5.0.md §6.9 / ADR-K12-006）。
//
// 它把五个 K12 记录集落到 k12_* 类型化表（迁移 V9），并保持与通用 records.Store
// **逐方法等价**的行为契约：RecordSchema 状态机转移校验、乐观并发锁（version CAS）、
// 幂等 dedupe（UNIQUE(agent_name, dedupe_key)）、归属隔离（agent_name 圈定）、到期队列
// （due_at）。对上仍然吞吐 records.AgentRecord 信封（Fields=JSON）——六缝 RecordSchema
// 声明与场景契约面不变，数据层替换、行为等价（ADR-K12-013 一次切换）。
//
// Transactional Outbox（§6.9/§6.15）：错题域写与 OutboxEvent 在同一 SQLite 事务提交；
// 投递由单进程 Dispatcher 顺序消费（at-least-once + (consumer,event_id) 去重 + dead-letter）。
package k12storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
)

// nowUnix 当前 unix 秒（测试可注入）。
var nowUnix = func() int64 { return time.Now().Unix() }

// Store K12 类型化存储（k12_* 表）。
//
// 与 records.Store 同构的方法面；依赖 RecordSchemaRegistry 拿记录集规则
// （状态机/去重键/字段校验），领域列映射由 rowMapper 承担。
type Store struct {
	db       *sql.DB
	registry *records.RecordSchemaRegistry
	mappers  map[string]rowMapper
	// notifyOutbox 领域写成功提交 Outbox 事件后的通知钩子（Dispatcher 绑定；可空）。
	notifyOutbox func()
}

// NewStore 创建类型化存储。db 需已跑过 migrate（含 V9 k12_* 表）。
func NewStore(db *sql.DB, registry *records.RecordSchemaRegistry) *Store {
	m := map[string]rowMapper{}
	for _, mp := range allMappers() {
		m[mp.collection()] = mp
	}
	return &Store{db: db, registry: registry, mappers: m}
}

// DB 暴露底层句柄（组合层跨聚合共享事务用，与 records.Store 的 Tx 缝对齐）。
func (s *Store) DB() *sql.DB { return s.db }

// SetOutboxNotifier 绑定 Outbox 追加通知（Dispatcher 醒来立即消费；可空 = 仅轮询）。
func (s *Store) SetOutboxNotifier(fn func()) { s.notifyOutbox = fn }

func (s *Store) mapperFor(collection string) (rowMapper, error) {
	if mp, ok := s.mappers[collection]; ok {
		return mp, nil
	}
	return nil, fmt.Errorf("%w: %q 未映射类型化表", records.ErrUnknownCollection, collection)
}

// —— RecordSchema 规则复刻（records 包同语义；Statuses/Transitions 为导出字段）——

func schemaHasStatus(schema *records.RecordSchema, status string) bool {
	for _, v := range schema.Statuses {
		if v == status {
			return true
		}
	}
	return false
}

func schemaCanTransition(schema *records.RecordSchema, from, to string) bool {
	if len(schema.Transitions) == 0 || from == to {
		return true
	}
	for _, allowed := range schema.Transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// baseCols 通用基建列（全部 k12_* 聚合根表同构）。
const baseCols = `record_id, agent_name, schema_version, status, dedupe_key, tags_json, due_at,
    source_session_id, version, created_at, updated_at`

// ensureAgentRegistered 归属隔离硬边界：agent 未注册 → ErrScopeNotFound
// （不依赖连接是否开启 foreign_keys pragma，语义与 records.Store 的 BUG-20260712-#2 修复一致）。
func ensureAgentRegistered(ctx context.Context, q dbQueryer, agentName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM agents WHERE name = ?`, agentName).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: agent %q 未注册（请先创建该辅导助手再记录）", records.ErrScopeNotFound, agentName)
	}
	if err != nil {
		return fmt.Errorf("k12storage: 校验归属实例: %w", err)
	}
	return nil
}

func isForeignKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// Put 幂等写入一条记录（语义 = records.Store.Put，数据层为类型化表 + Outbox 同事务）。
//
// 返回 created：true=新建，false=去重命中（回填既有 record_id）。
func (s *Store) Put(ctx context.Context, r *records.AgentRecord) (created bool, err error) {
	if r.AgentName == "" {
		return false, fmt.Errorf("records: AgentName 不可空（隔离键）")
	}
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return false, err
	}
	mp, err := s.mapperFor(r.Collection)
	if err != nil {
		return false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(r.Fields); err != nil {
			return false, fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, r.Collection, err)
		}
	}
	if r.Status == "" {
		r.Status = schema.InitialStatus
	} else if !schemaHasStatus(schema, r.Status) {
		return false, fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", records.ErrInvalidStatus, r.Status, r.Collection)
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

	domainVals, err := mp.encode(r.Fields)
	if err != nil {
		return false, err
	}

	// 归属校验放事务外（autocommit 读）：事务首语句必须是写——SQLite 延迟事务
	// 先读后写会在锁升级处死锁式 SQLITE_BUSY（busy handler 不等待），并发写全挂。
	if err := ensureAgentRegistered(ctx, s.db, r.AgentName); err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("k12storage: 开启写事务: %w", err)
	}
	defer tx.Rollback()

	cols := mp.domainCols()
	q := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
        ON CONFLICT(agent_name, dedupe_key) DO NOTHING`,
		mp.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)))
	args := append([]any{r.RecordID, r.AgentName, r.SchemaVersion, r.Status, r.DedupeKey, r.Tags,
		r.DueAt, r.SourceSession, r.Version, r.CreatedAt, r.UpdatedAt}, domainVals...)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		if isForeignKeyErr(err) {
			return false, fmt.Errorf("%w: agent %q 未注册（请先创建该辅导助手再记录）", records.ErrScopeNotFound, r.AgentName)
		}
		return false, fmt.Errorf("records: 写入失败: %w", err)
	}
	n, _ := res.RowsAffected()
	created = n > 0
	if created {
		if err := mp.syncChildren(ctx, tx, r.RecordID, r.Fields); err != nil {
			return false, err
		}
	} else {
		// 去重命中：回填既有 record_id（与 records.Store 的 BUG 修复语义一致）。
		var existingID string
		if qErr := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT record_id FROM %s WHERE agent_name = ? AND dedupe_key = ?`, mp.table()),
			r.AgentName, r.DedupeKey).Scan(&existingID); qErr != nil {
			return false, fmt.Errorf("records: 去重命中后回查记录失败: %w", qErr)
		}
		r.RecordID = existingID
	}

	// Transactional Outbox：错题域写与事件同事务（§6.9）。
	emitted, err := appendDomainEvents(ctx, tx, r, created)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("k12storage: 提交写事务: %w", err)
	}
	if emitted && s.notifyOutbox != nil {
		s.notifyOutbox()
	}
	return created, nil
}

// FindDuplicate 按去重键查找既有记录（无则 ErrNotFound）。
func (s *Store) FindDuplicate(ctx context.Context, r *records.AgentRecord) (*records.AgentRecord, error) {
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return nil, err
	}
	mp, err := s.mapperFor(r.Collection)
	if err != nil {
		return nil, err
	}
	key := schema.DedupeKey(r)
	rows, err := s.queryRecords(ctx, mp,
		`WHERE agent_name = ? AND dedupe_key = ? LIMIT 1`, r.AgentName, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, records.ErrNotFound
	}
	return rows[0], nil
}

// Delete 按 record_id 删除（agent_name 圈定归属；不存在/不属于该实例 = ErrNotFound）。
func (s *Store) Delete(ctx context.Context, agentName, recordID string) error {
	if agentName == "" || recordID == "" {
		return records.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: 开启删除事务: %w", err)
	}
	defer tx.Rollback()
	for _, mp := range allMappers() {
		// 子表先清（不依赖连接层 foreign_keys pragma 的 CASCADE）。
		if err := deleteChildren(ctx, tx, mp, recordID, agentName); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE record_id = ? AND agent_name = ?`, mp.table()),
			recordID, agentName)
		if err != nil {
			return fmt.Errorf("records: 删除失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return tx.Commit()
		}
	}
	return records.ErrNotFound
}

// deleteChildren 删除聚合根的子表行（按归属圈定）。
func deleteChildren(ctx context.Context, ex dbExecer, mp rowMapper, recordID, agentName string) error {
	scope := ""
	args := []any{recordID}
	if agentName != "" {
		scope = ` AND agent_name = ?`
		args = append(args, agentName)
	}
	switch mp.(type) {
	case practiceSetMapper:
		_, err := ex.ExecContext(ctx, `DELETE FROM k12_practice_set_items WHERE set_record_id IN
            (SELECT record_id FROM k12_practice_sets WHERE record_id = ?`+scope+`)`, args...)
		return err
	case creativeWorkMapper:
		if _, err := ex.ExecContext(ctx, `DELETE FROM k12_creative_work_versions WHERE work_record_id IN
            (SELECT record_id FROM k12_creative_works WHERE record_id = ?`+scope+`)`, args...); err != nil {
			return err
		}
		_, err := ex.ExecContext(ctx, `DELETE FROM k12_work_feedback WHERE work_record_id IN
            (SELECT record_id FROM k12_creative_works WHERE record_id = ?`+scope+`)`, args...)
		return err
	}
	return nil
}

// Get 按 record_id 取记录（跨五表探测）。
func (s *Store) Get(ctx context.Context, recordID string) (*records.AgentRecord, error) {
	return s.getVia(ctx, s.db, recordID)
}

func (s *Store) getVia(ctx context.Context, q dbQueryer, recordID string) (*records.AgentRecord, error) {
	for _, mp := range allMappers() {
		rows, err := s.queryRecordsVia(ctx, q, mp, `WHERE record_id = ?`, recordID)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return rows[0], nil
		}
	}
	return nil, records.ErrNotFound
}

// ListByScope 按 实例 + 记录集 (+ 可选状态) 列出记录（归属隔离硬边界）。
func (s *Store) ListByScope(ctx context.Context, agentName, collection, status string) ([]*records.AgentRecord, error) {
	if _, err := s.registry.Get(collection); err != nil {
		return nil, err
	}
	mp, err := s.mapperFor(collection)
	if err != nil {
		return nil, err
	}
	where := `WHERE agent_name = ?`
	args := []any{agentName}
	if status != "" {
		where += ` AND status = ?`
		args = append(args, status)
	}
	where += ` ORDER BY created_at DESC, record_id`
	return s.queryRecords(ctx, mp, where, args...)
}

// ListDue 到期队列：due_at ≤ before 的记录按到期升序（间隔重排调度原语）。
func (s *Store) ListDue(ctx context.Context, agentName, collection string, before int64) ([]*records.AgentRecord, error) {
	mp, err := s.mapperFor(collection)
	if err != nil {
		return nil, err
	}
	return s.queryRecords(ctx, mp,
		`WHERE agent_name = ? AND due_at IS NOT NULL AND due_at <= ? ORDER BY due_at ASC, record_id`,
		agentName, before)
}

// UpdateStatus 乐观锁推进状态（version CAS；转移须在记录集状态机偏序内）。
func (s *Store) UpdateStatus(ctx context.Context, recordID, newStatus string, dueAt *int64, expectedVersion int) error {
	return s.updateStatusScoped(ctx, "", recordID, newStatus, dueAt, expectedVersion)
}

// UpdateStatusScoped 同 UpdateStatus，且 agent_name 进 CAS WHERE（防跨实例推进）。
func (s *Store) UpdateStatusScoped(ctx context.Context, agentName, recordID, newStatus string, dueAt *int64, expectedVersion int) error {
	if agentName == "" {
		return records.ErrNotFound
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
	mp, err := s.mapperFor(cur.Collection)
	if err != nil {
		return err
	}
	if !schemaHasStatus(schema, newStatus) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", records.ErrInvalidStatus, newStatus, cur.Collection)
	}
	if !schemaCanTransition(schema, cur.Status, newStatus) {
		return fmt.Errorf("%w: 记录集 %q 不允许 %q→%q", records.ErrIllegalTransition, cur.Collection, cur.Status, newStatus)
	}
	if agentName != "" && cur.AgentName != agentName {
		return records.ErrNotFound
	}

	dedupeSet, dedupeArgs := dedupeReleaseAssign(schema, cur, newStatus)
	query := fmt.Sprintf(`UPDATE %s SET status = ?, due_at = ?%s, version = version + 1, updated_at = ?
        WHERE record_id = ? AND version = ?`, mp.table(), dedupeSet)
	args := append([]any{newStatus, dueAt}, dedupeArgs...)
	args = append(args, nowUnix(), recordID, expectedVersion)
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
			if qErr := s.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT 1 FROM %s WHERE record_id = ? AND agent_name = ?`, mp.table()),
				recordID, agentName).Scan(&exists); qErr == sql.ErrNoRows {
				return records.ErrNotFound
			}
		}
		return records.ErrVersionConflict
	}
	return nil
}

// UpdateStatusFields 同 UpdateStatus 且同时替换领域字段（CAS + 状态机 + 字段校验 + 子表整体重写）。
func (s *Store) UpdateStatusFields(ctx context.Context, recordID, newStatus string, dueAt *int64, fieldsJSON string, expectedVersion int) error {
	cur, err := s.Get(ctx, recordID)
	if err != nil {
		return err
	}
	schema, err := s.registry.Get(cur.Collection)
	if err != nil {
		return err
	}
	mp, err := s.mapperFor(cur.Collection)
	if err != nil {
		return err
	}
	if !schemaHasStatus(schema, newStatus) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", records.ErrInvalidStatus, newStatus, cur.Collection)
	}
	if !schemaCanTransition(schema, cur.Status, newStatus) {
		return fmt.Errorf("%w: 记录集 %q 不允许 %q→%q", records.ErrIllegalTransition, cur.Collection, cur.Status, newStatus)
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(fieldsJSON); err != nil {
			return fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, cur.Collection, err)
		}
	}
	domainVals, err := mp.encode(fieldsJSON)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: 开启更新事务: %w", err)
	}
	defer tx.Rollback()

	cols := mp.domainCols()
	assigns := make([]string, 0, len(cols))
	for _, c := range cols {
		assigns = append(assigns, c+" = ?")
	}
	dedupeSet, dedupeArgs := dedupeReleaseAssign(schema, cur, newStatus)
	q := fmt.Sprintf(`UPDATE %s SET status = ?, due_at = ?%s, %s, version = version + 1, updated_at = ?
        WHERE record_id = ? AND version = ?`, mp.table(), dedupeSet, strings.Join(assigns, ", "))
	args := append([]any{newStatus, dueAt}, dedupeArgs...)
	args = append(args, domainVals...)
	args = append(args, nowUnix(), recordID, expectedVersion)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("records: 更新失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return records.ErrVersionConflict
	}
	if err := mp.syncChildren(ctx, tx, recordID, fieldsJSON); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: 提交更新事务: %w", err)
	}
	return nil
}

// dedupeReleaseAssign 按 schema.ReleaseDedupeOnStatuses 生成"释放去重键"的 SET 片段
// （BUG-20260718 归档件永久截胡同键重建）：进入声明的退出态时，把 dedupe_key 改写为
// 含 record_id 的墓碑值（确定性、互不碰撞、幂等——已是墓碑不再叠加），
// 让同键新记录可重新创建；其余状态返回空片段（幂等去重语义不变）。
func dedupeReleaseAssign(schema *records.RecordSchema, cur *records.AgentRecord, newStatus string) (string, []any) {
	if schema == nil || !schema.ReleasesDedupeOn(newStatus) {
		return "", nil
	}
	if strings.Contains(cur.DedupeKey, dedupeTombstoneSep) {
		return "", nil
	}
	return ", dedupe_key = ?", []any{cur.DedupeKey + dedupeTombstoneSep + cur.RecordID}
}

// dedupeTombstoneSep 墓碑键分隔符：dedupe_key 均为 sha1 hex（场景包派生），不含 '#'，
// 与活跃键零碰撞。
const dedupeTombstoneSep = "#released#"

// ExportAgent 导出某实例全部 K12 记录（跨记录集），供备份（.hexbak）。
// 排序与 agent_records 时代一致：collection（字节序）、created_at、record_id。
func (s *Store) ExportAgent(ctx context.Context, agentName string) ([]*records.AgentRecord, error) {
	mappers := allMappers()
	sort.Slice(mappers, func(i, j int) bool { return mappers[i].collection() < mappers[j].collection() })
	var out []*records.AgentRecord
	for _, mp := range mappers {
		rows, err := s.queryRecords(ctx, mp, `WHERE agent_name = ? ORDER BY created_at, record_id`, agentName)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// —— 导入 / 恢复（.hexbak 契约：原样保留 record_id/status/version/时间戳）——

func (s *Store) validateImportRecord(r *records.AgentRecord, expectedAgent string) error {
	if r == nil {
		return fmt.Errorf("%w: 导入记录不可为 nil", records.ErrInvalidRecord)
	}
	if r.RecordID == "" || r.AgentName == "" || r.Collection == "" {
		return fmt.Errorf("%w: 导入记录缺少 record_id / agent_name / collection", records.ErrInvalidRecord)
	}
	if expectedAgent != "" && r.AgentName != expectedAgent {
		return fmt.Errorf("%w: 导入记录 %q 不属于实例 %q", records.ErrInvalidRecord, r.RecordID, expectedAgent)
	}
	schema, err := s.registry.Get(r.Collection)
	if err != nil {
		return err
	}
	if _, err := s.mapperFor(r.Collection); err != nil {
		return err
	}
	if r.SchemaVersion <= 0 || r.SchemaVersion > schema.Version {
		return fmt.Errorf("%w: 记录 %q schema v%d 超出 %q 支持的 v%d", records.ErrInvalidRecord, r.RecordID, r.SchemaVersion, r.Collection, schema.Version)
	}
	if !schemaHasStatus(schema, r.Status) {
		return fmt.Errorf("%w: %q 不在记录集 %q 的状态机内", records.ErrInvalidStatus, r.Status, r.Collection)
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(r.Fields); err != nil {
			return fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, r.Collection, err)
		}
	}
	return nil
}

// importRecordVia 原样 upsert 一条记录（含子表整体重写）。record_id 只允许覆盖同 agent 记录。
func (s *Store) importRecordVia(ctx context.Context, ex dbHandle, r *records.AgentRecord) error {
	mp, err := s.mapperFor(r.Collection)
	if err != nil {
		return err
	}
	domainVals, err := mp.encode(r.Fields)
	if err != nil {
		return err
	}
	cols := mp.domainCols()
	assigns := make([]string, 0, len(cols)+10)
	for _, c := range append([]string{"agent_name", "schema_version", "status", "dedupe_key",
		"tags_json", "due_at", "source_session_id", "version", "created_at", "updated_at"}, cols...) {
		assigns = append(assigns, fmt.Sprintf("%s=excluded.%s", c, c))
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
        ON CONFLICT(record_id) DO UPDATE SET %s
        WHERE %s.agent_name = excluded.agent_name`,
		mp.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)),
		strings.Join(assigns, ", "), mp.table())
	args := append([]any{r.RecordID, r.AgentName, r.SchemaVersion, r.Status, r.DedupeKey, r.Tags,
		r.DueAt, r.SourceSession, r.Version, r.CreatedAt, r.UpdatedAt}, domainVals...)
	res, err := ex.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if n == 0 {
		return fmt.Errorf("records: record_id %q 已属于其他实例", r.RecordID)
	}
	return mp.syncChildren(ctx, ex, r.RecordID, r.Fields)
}

// ImportRecord 原样导入一条记录（恢复用）。
func (s *Store) ImportRecord(ctx context.Context, r *records.AgentRecord) error {
	if err := s.validateImportRecord(r, ""); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: 开启导入事务: %w", err)
	}
	defer tx.Rollback()
	if err := s.importRecordVia(ctx, tx, r); err != nil {
		return fmt.Errorf("records: 导入失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交导入事务: %w", err)
	}
	return nil
}

// ImportRecords 单事务原子导入一批（任一条失败整批回滚，不部分导入）。
func (s *Store) ImportRecords(ctx context.Context, recs []*records.AgentRecord) error {
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
	defer tx.Rollback()
	for _, r := range recs {
		if err := s.importRecordVia(ctx, tx, r); err != nil {
			return fmt.Errorf("records: 导入记录 %s: %w", r.RecordID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交导入事务: %w", err)
	}
	return nil
}

// ValidateAgentRecords 恢复批次的无副作用预检（跨子系统原子恢复的前置）。
func (s *Store) ValidateAgentRecords(agentName string, recs []*records.AgentRecord) error {
	if agentName == "" {
		return fmt.Errorf("%w: agentName 不可空", records.ErrInvalidRecord)
	}
	for _, r := range recs {
		if err := s.validateImportRecord(r, agentName); err != nil {
			return err
		}
	}
	return nil
}

// ImportAgentRecordsTx 在调用方事务内原子合并一个实例的归档批次
// （缺失新增、同 record_id 幂等替换、无关现存记录保留）。
func (s *Store) ImportAgentRecordsTx(ctx context.Context, tx *sql.Tx, agentName string, recs []*records.AgentRecord) error {
	if tx == nil {
		return fmt.Errorf("records: nil merge transaction")
	}
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	for _, r := range recs {
		if err := s.importRecordVia(ctx, tx, r); err != nil {
			return fmt.Errorf("records: 合并记录 %q: %w", r.RecordID, err)
		}
	}
	return nil
}

// ImportAgentRecords 单事务合并一个实例的完整归档批次（恢复契约）。
func (s *Store) ImportAgentRecords(ctx context.Context, agentName string, recs []*records.AgentRecord) error {
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

func (s *Store) replaceAgentRecordsVia(ctx context.Context, ex dbHandle, agentName string, recs []*records.AgentRecord) error {
	for _, mp := range allMappers() {
		switch mp.(type) {
		case practiceSetMapper:
			if _, err := ex.ExecContext(ctx, `DELETE FROM k12_practice_set_items WHERE set_record_id IN
                (SELECT record_id FROM k12_practice_sets WHERE agent_name = ?)`, agentName); err != nil {
				return fmt.Errorf("records: 清理实例旧记录: %w", err)
			}
		case creativeWorkMapper:
			if _, err := ex.ExecContext(ctx, `DELETE FROM k12_creative_work_versions WHERE work_record_id IN
                (SELECT record_id FROM k12_creative_works WHERE agent_name = ?)`, agentName); err != nil {
				return fmt.Errorf("records: 清理实例旧记录: %w", err)
			}
			if _, err := ex.ExecContext(ctx, `DELETE FROM k12_work_feedback WHERE work_record_id IN
                (SELECT record_id FROM k12_creative_works WHERE agent_name = ?)`, agentName); err != nil {
				return fmt.Errorf("records: 清理实例旧记录: %w", err)
			}
		}
		if _, err := ex.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE agent_name = ?`, mp.table()), agentName); err != nil {
			return fmt.Errorf("records: 清理实例旧记录: %w", err)
		}
	}
	for _, r := range recs {
		if err := s.importRecordVia(ctx, ex, r); err != nil {
			return fmt.Errorf("records: 替换记录 %q: %w", r.RecordID, err)
		}
	}
	return nil
}

// ReplaceAgentRecordsTx 在调用方事务内用归档记录替换实例全部记录（组合层跨聚合共同提交）。
func (s *Store) ReplaceAgentRecordsTx(ctx context.Context, tx *sql.Tx, agentName string, recs []*records.AgentRecord) error {
	if tx == nil {
		return fmt.Errorf("records: nil replace transaction")
	}
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	return s.replaceAgentRecordsVia(ctx, tx, agentName, recs)
}

// ReplaceAgentRecords 单事务替换实例全部记录（整批先校验，不合法不动现有数据）。
func (s *Store) ReplaceAgentRecords(ctx context.Context, agentName string, recs []*records.AgentRecord) error {
	if err := s.ValidateAgentRecords(agentName, recs); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: 开启替换事务: %w", err)
	}
	defer tx.Rollback()
	if err := s.replaceAgentRecordsVia(ctx, tx, agentName, recs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("records: 提交替换事务: %w", err)
	}
	return nil
}

// —— 查询装配 ——

func (s *Store) queryRecords(ctx context.Context, mp rowMapper, whereOrder string, args ...any) ([]*records.AgentRecord, error) {
	return s.queryRecordsVia(ctx, s.db, mp, whereOrder, args...)
}

func (s *Store) queryRecordsVia(ctx context.Context, q dbQueryer, mp rowMapper, whereOrder string, args ...any) ([]*records.AgentRecord, error) {
	query := fmt.Sprintf(`SELECT %s, %s FROM %s %s`,
		baseCols, strings.Join(mp.domainCols(), ", "), mp.table(), whereOrder)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("records: 查询失败: %w", err)
	}
	defer rows.Close()
	var out []*records.AgentRecord
	for rows.Next() {
		var r records.AgentRecord
		domainDest, finish := mp.newScan()
		dest := append([]any{&r.RecordID, &r.AgentName, &r.SchemaVersion, &r.Status, &r.DedupeKey,
			&r.Tags, &r.DueAt, &r.SourceSession, &r.Version, &r.CreatedAt, &r.UpdatedAt}, domainDest...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("records: 扫描失败: %w", err)
		}
		r.Collection = mp.collection()
		fieldsJSON, err := finish()
		if err != nil {
			return nil, err
		}
		r.Fields = fieldsJSON
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 子表挂载放在主查询之后（避免同连接嵌套活动 rows）。
	for _, r := range out {
		merged, err := mp.attachChildren(ctx, q, r.RecordID, r.Fields)
		if err != nil {
			return nil, err
		}
		r.Fields = merged
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}
