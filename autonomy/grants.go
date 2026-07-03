package autonomy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/hexclaw/engine"
)

// Grant 一条任务级授权：仅对指定任务（task_ref）放行 entries 描述的能力。
// entries 复用矩阵条目语义：类别名（如 publish）、精确工具名
// （如 github.issues.write_label）或 glob（如 publish_*）。
type Grant struct {
	ID        string     `json:"id"`
	TaskRef   string     `json:"task_ref"` // "cron:<id>" / "webhook:<id>" / "workflow:<id>"
	Source    string     `json:"source"`   // 授权时的触发来源；空 = 不限来源
	Entries   []string   `json:"entries"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// GrantStore 任务级授权存储（SQLite + 内存镜像）。
//
// 权限闸在每次工具调用上求值 GrantAllows，故活跃授权镜像在内存里
// （读多写少，RWMutex 保护）；一切变更先落库再刷新镜像。
type GrantStore struct {
	db *sql.DB

	mu     sync.RWMutex
	active map[string][]Grant // task_ref -> active grants
}

// NewGrantStore 创建任务级授权存储。
func NewGrantStore(db *sql.DB) *GrantStore {
	return &GrantStore{db: db, active: map[string][]Grant{}}
}

// Init 建表并加载活跃授权（幂等）。
func (s *GrantStore) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS autonomy_grants (
		id TEXT PRIMARY KEY,
		task_ref TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '',
		entries TEXT NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		revoked_at DATETIME
	)`)
	if err != nil {
		return fmt.Errorf("初始化 autonomy_grants 表失败: %w", err)
	}
	return s.reload(ctx)
}

// Create 新建授权并刷新镜像。
func (s *GrantStore) Create(ctx context.Context, g Grant) (Grant, error) {
	g.TaskRef = strings.TrimSpace(g.TaskRef)
	if g.TaskRef == "" {
		return Grant{}, fmt.Errorf("task_ref 必填")
	}
	entries := normalizeGrantEntries(g.Entries)
	if len(entries) == 0 {
		return Grant{}, fmt.Errorf("entries 至少一条（类别名/工具名/glob）")
	}
	g.Entries = entries
	g.Source = strings.ToLower(strings.TrimSpace(g.Source))

	// FS-6：Create 幂等——grant 落库后 resume/enable 失败时前端会重试建 grant，
	// 无幂等会堆叠重复授权。相同活跃 (task_ref, source, entries集合) 直接返回既有那条。
	if existing, ok := s.findActiveGrant(g.TaskRef, g.Source, g.Entries); ok {
		return existing, nil
	}

	if g.ID == "" {
		g.ID = "ag-" + idgen.ShortID()
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}

	raw, err := json.Marshal(g.Entries)
	if err != nil {
		return Grant{}, fmt.Errorf("序列化 entries 失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO autonomy_grants (id, task_ref, source, entries, note, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		g.ID, g.TaskRef, g.Source, string(raw), g.Note, g.CreatedAt); err != nil {
		return Grant{}, fmt.Errorf("写入任务级授权失败: %w", err)
	}
	if err := s.reload(ctx); err != nil {
		return Grant{}, err
	}
	logger.Info("[autonomy] 任务级授权已创建", "task_ref", g.TaskRef, "entries", strings.Join(g.Entries, ","))
	return g, nil
}

// findActiveGrant 在内存镜像里找一条 task_ref/source/entries 集合都相同的活跃授权。
// entries 已去重，集合相等 = 长度相同且逐项互含（顺序无关，避免前端换序绕过幂等）。
func (s *GrantStore) findActiveGrant(taskRef, source string, entries []string) (Grant, bool) {
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.active[taskRef] {
		if g.Source != source || len(g.Entries) != len(entries) {
			continue
		}
		same := true
		for _, e := range g.Entries {
			if !want[e] {
				same = false
				break
			}
		}
		if same {
			return g, true
		}
	}
	return Grant{}, false
}

// Revoke 撤销单条授权。
func (s *GrantStore) Revoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE autonomy_grants SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, time.Now(), id)
	if err != nil {
		return fmt.Errorf("撤销授权失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("授权 %q 不存在或已撤销", id)
	}
	return s.reload(ctx)
}

// RevokeByTask 撤销任务的全部授权（任务删除时回收）。
func (s *GrantStore) RevokeByTask(ctx context.Context, taskRef string) error {
	if strings.TrimSpace(taskRef) == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE autonomy_grants SET revoked_at = ? WHERE task_ref = ? AND revoked_at IS NULL`,
		time.Now(), taskRef); err != nil {
		return fmt.Errorf("回收任务授权失败: %w", err)
	}
	return s.reload(ctx)
}

// ListActive 列出活跃授权；taskRef 为空时返回全部。
func (s *GrantStore) ListActive(taskRef string) []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if taskRef != "" {
		return append([]Grant(nil), s.active[taskRef]...)
	}
	var out []Grant
	for _, gs := range s.active {
		out = append(out, gs...)
	}
	return out
}

// GrantAllows 实现 engine.TaskGrantChecker：指定任务的活跃授权是否放行该工具。
func (s *GrantStore) GrantAllows(source, taskRef, toolName string) bool {
	if s == nil || taskRef == "" || toolName == "" {
		return false
	}
	source = strings.ToLower(strings.TrimSpace(source))
	s.mu.RLock()
	grants := s.active[taskRef]
	s.mu.RUnlock()
	for _, g := range grants {
		if g.Source != "" && g.Source != source {
			continue
		}
		for _, entry := range g.Entries {
			if engine.SystemDispatchEntryAllowsTool(entry, toolName) {
				return true
			}
		}
	}
	return false
}

// reload 从库重建活跃授权镜像。
func (s *GrantStore) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_ref, source, entries, note, created_at FROM autonomy_grants WHERE revoked_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()

	next := map[string][]Grant{}
	for rows.Next() {
		var g Grant
		var raw string
		if err := rows.Scan(&g.ID, &g.TaskRef, &g.Source, &raw, &g.Note, &g.CreatedAt); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &g.Entries); err != nil {
			logger.Warn("[autonomy] 授权 entries 解析失败，跳过", "id", g.ID, "err", err.Error())
			continue
		}
		next[g.TaskRef] = append(next[g.TaskRef], g)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.active = next
	s.mu.Unlock()
	return nil
}

// normalizeGrantEntries 去空去重。**不小写化**：连接器工具名大小写敏感
// （如 createIssue），小写化会让运行时匹配永假、授权永久失效（FS-2）。类别名
// 是固定小写枚举，匹配侧 systemDispatchEntryAllows 已对类别做大小写不敏感查表。
func normalizeGrantEntries(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		// 任务级授权是「最小范围」工具：不接受 "*" 全放行——那是全功能
		// Profile 的职责，必须走高后果确认，而不是借道 grant。
		if e == "*" {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
