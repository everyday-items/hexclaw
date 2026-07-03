// Package autonomy 承载无人值守自动化的权限治理数据面：
//
//   - 权限决策审计日志（DecisionStore）：把引擎权限闸的每次放行/拦下/拒绝
//     持久化到本地 SQLite，供设置页「最近权限决策」与阻断处理页回溯。
//   - 任务级授权（GrantStore）：按任务（cron:<id>/webhook:<id>/workflow:<id>）
//     持久化最小范围的能力授权，权限闸在 Profile 矩阵之前求值它。
//   - 预检求值（Preflight）：创建流的静默预检——对照当前 Profile 与任务级
//     授权，确定性地回答「哪些能力自动放行、哪些会转审批」。
//
// 引擎侧只依赖 engine 包里定义的接口（PermissionDecisionRecorder /
// TaskGrantChecker），本包实现它们；engine 不反向依赖本包。
package autonomy

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/hexclaw/engine"
)

// Decision 是一条持久化的权限决策记录。
type Decision struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Source     string    `json:"source"`
	TaskRef    string    `json:"task_ref,omitempty"`
	Tool       string    `json:"tool"`
	Capability string    `json:"capability,omitempty"`
	Profile    string    `json:"profile"`
	Decision   string    `json:"decision"` // allow | pending | deny
	Via        string    `json:"via"`      // matrix | task_grant | solve_grant | policy
	Reason     string    `json:"reason,omitempty"`
}

// DecisionFilter 查询过滤条件；零值字段不过滤。
type DecisionFilter struct {
	Source   string
	TaskRef  string
	Decision string
	Limit    int
}

// decisionRetainRows 审计日志保留上限：超过后按时间淘汰最旧记录，
// 防止单用户桌面库无界膨胀。
const decisionRetainRows = 5000

// DecisionStore 权限决策审计存储（SQLite）。
type DecisionStore struct {
	db *sql.DB
	// writes 每 200 次插入触发一次淘汰（避免每写必扫）。
	// 原子计数：权限闸在并发工具调用上同步触发 Record（HX-2 race 取证）。
	writes atomic.Int64
}

// NewDecisionStore 创建决策审计存储。
func NewDecisionStore(db *sql.DB) *DecisionStore {
	return &DecisionStore{db: db}
}

// Init 建表（幂等）。
func (s *DecisionStore) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS autonomy_decisions (
		id TEXT PRIMARY KEY,
		at DATETIME NOT NULL,
		source TEXT NOT NULL,
		task_ref TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL,
		capability TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		decision TEXT NOT NULL,
		via TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return fmt.Errorf("初始化 autonomy_decisions 表失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_autonomy_decisions_at ON autonomy_decisions(at DESC)`); err != nil {
		return fmt.Errorf("初始化 autonomy_decisions 索引失败: %w", err)
	}
	// GO-8：writes 是进程内计数，重启归零——每次会话写不满 200 条的使用形态下
	// 表可跨重启无界膨胀。启动时兜底修剪一次，保证任意时刻 ≤ retain+200。
	s.prune(ctx)
	return nil
}

// Record 写入一条决策记录。
func (s *DecisionStore) Record(ctx context.Context, d Decision) error {
	if d.ID == "" {
		d.ID = "ad-" + idgen.ShortID()
	}
	if d.At.IsZero() {
		d.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO autonomy_decisions (id, at, source, task_ref, tool, capability, profile, decision, via, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.At, d.Source, d.TaskRef, d.Tool, d.Capability, d.Profile, d.Decision, d.Via, d.Reason)
	if err != nil {
		return fmt.Errorf("写入权限决策失败: %w", err)
	}
	if n := s.writes.Add(1); n%200 == 0 {
		s.prune(ctx)
	}
	return nil
}

// prune 淘汰最旧记录，保留 decisionRetainRows 条。
// `, id DESC` tie-break（GO-8）：同秒批量写入时保留集必须确定，且与 List 的
// `at DESC, id DESC` 排序一致——否则同一批记录哪些活下来取决于扫描顺序。
func (s *DecisionStore) prune(ctx context.Context) {
	_, err := s.db.ExecContext(ctx, `DELETE FROM autonomy_decisions WHERE id NOT IN (
		SELECT id FROM autonomy_decisions ORDER BY at DESC, id DESC LIMIT ?
	)`, decisionRetainRows)
	if err != nil {
		logger.Warn("[autonomy] 决策日志淘汰失败", "err", err.Error())
	}
}

// List 按过滤条件查询决策记录，按时间倒序。
func (s *DecisionStore) List(ctx context.Context, f DecisionFilter) ([]Decision, error) {
	query := `SELECT id, at, source, task_ref, tool, capability, profile, decision, via, reason
		FROM autonomy_decisions WHERE 1=1`
	args := []any{}
	if f.Source != "" {
		query += ` AND source = ?`
		args = append(args, f.Source)
	}
	if f.TaskRef != "" {
		query += ` AND task_ref = ?`
		args = append(args, f.TaskRef)
	}
	if f.Decision != "" {
		query += ` AND decision = ?`
		args = append(args, f.Decision)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.At, &d.Source, &d.TaskRef, &d.Tool,
			&d.Capability, &d.Profile, &d.Decision, &d.Via, &d.Reason); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PendingTaskRefs 返回最近仍处于「待审批」的任务集合：对每个 task_ref 取
// 其最新一条记录，最新记录 decision=pending 才算未解决（后续 allow 视为已解决）。
func (s *DecisionStore) PendingTaskRefs(ctx context.Context) (map[string]Decision, error) {
	// 每个 (task_ref, tool) 组合取最新一条；组合级 pending 才算未解决——
	// 只按 task_ref 聚合会被同任务后续别的工具 allow 掩盖真实阻断。
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.at, d.source, d.task_ref, d.tool, d.capability, d.profile, d.decision, d.via, d.reason
		FROM autonomy_decisions d
		JOIN (
			SELECT task_ref, tool, MAX(at) AS max_at
			FROM autonomy_decisions
			WHERE task_ref != ''
			GROUP BY task_ref, tool
		) latest ON d.task_ref = latest.task_ref AND d.tool = latest.tool AND d.at = latest.max_at
		WHERE d.decision = 'pending'
		ORDER BY d.at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Decision{}
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.At, &d.Source, &d.TaskRef, &d.Tool,
			&d.Capability, &d.Profile, &d.Decision, &d.Via, &d.Reason); err != nil {
			return nil, err
		}
		// 同任务多个 pending 工具：保留最新一条做代表，完整清单走 List 过滤。
		if _, seen := out[d.TaskRef]; !seen {
			out[d.TaskRef] = d
		}
	}
	return out, rows.Err()
}

// Recorder 把 engine 的权限决策事件写进 DecisionStore。
// 实现 engine.PermissionDecisionRecorder；存储失败只告警不阻断工具链路。
type Recorder struct {
	store *DecisionStore
}

// NewRecorder 创建决策记录器。
func NewRecorder(store *DecisionStore) *Recorder {
	return &Recorder{store: store}
}

// RecordPermissionDecision 实现 engine.PermissionDecisionRecorder。
func (r *Recorder) RecordPermissionDecision(ctx context.Context, d engine.PermissionDecision) {
	if r == nil || r.store == nil {
		return
	}
	// 工具链路的 ctx 可能随任务结束被取消；审计写入用独立短超时 ctx，
	// 保证「任务被拦下」这类关键记录不因任务 ctx 取消而丢失。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	err := r.store.Record(wctx, Decision{
		Source:     d.Source,
		TaskRef:    d.TaskRef,
		Tool:       d.Tool,
		Capability: d.Capability,
		Profile:    d.Profile,
		Decision:   d.Decision,
		Via:        d.Via,
		Reason:     d.Reason,
	})
	if err != nil {
		logger.Warn("[autonomy] 权限决策审计写入失败", "tool", d.Tool, "err", err.Error())
	}
}
