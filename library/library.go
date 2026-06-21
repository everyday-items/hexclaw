// Package library 实现 §11.8 交互层后端：Prompt 库（一库三 type）与记忆薄版
// （standing/fact）。两者都是用户自管内容，服务端下发，运营增删不发版。
//
// 设计取舍：
//   - command 带参走 $ARGUMENTS 纯文本替换（Claude Code 轻做法），严禁演化为命名多槽 + 版本引擎。
//   - 记忆注入 = standing 全量 + fact 命中子串，拼成一个上下文块，由 engine 每轮组 prompt 时前置。
package library

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// ── Prompt 库 ────────────────────────────────────────────────

// Prompt 是一条 Prompt 库条目（type=prompt 普通片段 / type=command 带参命令）。
type Prompt struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // prompt | command
	Title     string    `json:"title"`
	BodyMD    string    `json:"body_md"`
	ArgsJSON  string    `json:"args_json"`  // command 的轻量参数声明（$ARGUMENTS / 单参）
	ToolScope string    `json:"tool_scope"` // 召唤时限定的工具范围（逗号分隔）
	Model     string    `json:"model"`      // 召唤时建议模型
	Category  string    `json:"category"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PromptStore 持久化 Prompt 库。
type PromptStore struct{ db *sql.DB }

// NewPromptStore 构造 Prompt 库存储。
func NewPromptStore(db *sql.DB) *PromptStore { return &PromptStore{db: db} }

// List 返回全部条目（含禁用），供管理 UI 用。
func (s *PromptStore) List(ctx context.Context) ([]Prompt, error) {
	return s.query(ctx, `SELECT id, type, title, body_md, args_json, tool_scope, model, category, enabled, updated_at
		FROM prompts ORDER BY updated_at DESC`)
}

// ListEnabled 返回启用条目，供 GET /prompts 服务端下发。
func (s *PromptStore) ListEnabled(ctx context.Context) ([]Prompt, error) {
	return s.query(ctx, `SELECT id, type, title, body_md, args_json, tool_scope, model, category, enabled, updated_at
		FROM prompts WHERE enabled = 1 ORDER BY updated_at DESC`)
}

func (s *PromptStore) query(ctx context.Context, q string, args ...any) ([]Prompt, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Prompt
	for rows.Next() {
		var p Prompt
		var enabled int
		if err := rows.Scan(&p.ID, &p.Type, &p.Title, &p.BodyMD, &p.ArgsJSON,
			&p.ToolScope, &p.Model, &p.Category, &enabled, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get 按 id 取单条。
func (s *PromptStore) Get(ctx context.Context, id string) (Prompt, bool, error) {
	list, err := s.query(ctx, `SELECT id, type, title, body_md, args_json, tool_scope, model, category, enabled, updated_at
		FROM prompts WHERE id = ?`, id)
	if err != nil {
		return Prompt{}, false, err
	}
	if len(list) == 0 {
		return Prompt{}, false, nil
	}
	return list[0], true, nil
}

// Upsert 创建或更新一条 Prompt（ID 空 → 生成）。type 缺省 "prompt"。返回写入后的 ID。
func (s *PromptStore) Upsert(ctx context.Context, p *Prompt) (string, error) {
	if p.ID == "" {
		p.ID = "pr-" + idgen.ShortID()
	}
	if strings.TrimSpace(p.Type) == "" {
		p.Type = "prompt"
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO prompts (id, type, title, body_md, args_json, tool_scope, model, category, enabled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET type=excluded.type, title=excluded.title, body_md=excluded.body_md,
		   args_json=excluded.args_json, tool_scope=excluded.tool_scope, model=excluded.model,
		   category=excluded.category, enabled=excluded.enabled, updated_at=excluded.updated_at`,
		p.ID, p.Type, p.Title, p.BodyMD, p.ArgsJSON, p.ToolScope, p.Model, p.Category, enabled, time.Now())
	return p.ID, err
}

// Delete 删除一条 Prompt。
func (s *PromptStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM prompts WHERE id = ?`, id)
	return err
}

// Render 把 command 型 Prompt 的 $ARGUMENTS 占位替换为用户填的参数（纯文本替换，
// 非模板引擎 —— Claude Code 轻做法）。非 command 或无占位时原样返回 body。
func Render(p Prompt, arguments string) string {
	return strings.ReplaceAll(p.BodyMD, "$ARGUMENTS", arguments)
}

// ── 记忆薄版 ─────────────────────────────────────────────────

// Memory 是一条记忆（standing 每轮全量注入 / fact 命中注入）。
type Memory struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // standing | fact
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryStore 持久化记忆薄版。
type MemoryStore struct{ db *sql.DB }

// NewMemoryStore 构造记忆存储。
func NewMemoryStore(db *sql.DB) *MemoryStore { return &MemoryStore{db: db} }

// List 返回全部记忆（供管理 UI）。
func (s *MemoryStore) List(ctx context.Context) ([]Memory, error) {
	return s.query(ctx, `SELECT id, kind, content, updated_at FROM memories ORDER BY updated_at DESC`)
}

func (s *MemoryStore) query(ctx context.Context, q string, args ...any) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Kind, &m.Content, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Upsert 创建或更新一条记忆（ID 空 → 生成）。kind 缺省 "fact"。
func (s *MemoryStore) Upsert(ctx context.Context, m *Memory) (string, error) {
	if m.ID == "" {
		m.ID = "mem-" + idgen.ShortID()
	}
	if strings.TrimSpace(m.Kind) == "" {
		m.Kind = "fact"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, kind, content, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, content=excluded.content, updated_at=excluded.updated_at`,
		m.ID, m.Kind, m.Content, time.Now())
	return m.ID, err
}

// Delete 删除一条记忆。
func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	return err
}

// maxFactHits 限制单轮注入的 fact 数，防记忆膨胀挤爆 prompt。
const maxFactHits = 8

// queryTokens 把 query 按空白拆成检索词，丢弃 <2 字符的噪音词，统一小写。
func queryTokens(query string) []string {
	var out []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		t = strings.TrimSpace(t)
		if utf8.RuneCountInString(t) >= 2 {
			out = append(out, t)
		}
	}
	return out
}

// factMatches 判定一条 fact 是否被 query 命中：任一检索词（子串、大小写不敏感）出现在
// fact 正文即命中。无检索词时不命中（避免空 query 拉全部 fact）。
func factMatches(content string, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	lc := strings.ToLower(content)
	for _, t := range tokens {
		if strings.Contains(lc, t) {
			return true
		}
	}
	return false
}

// Inject 构建本轮记忆注入块：standing 全量 + 与 query 子串命中的 fact（上限 maxFactHits）。
// 无记忆时返回 ""。出错返回 ""（注入是增强，绝不阻断主流程）。
func (s *MemoryStore) Inject(ctx context.Context, query string) string {
	all, err := s.List(ctx)
	if err != nil {
		return ""
	}
	tokens := queryTokens(query)
	var standing, facts []string
	for _, m := range all {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if m.Kind == "standing" {
			standing = append(standing, c)
			continue
		}
		// fact：query 的某个词（≥2 字符）命中 fact 正文才注入（子串、大小写不敏感）。
		// query 为空 / 无有效词时不注入 fact。
		if len(facts) < maxFactHits && factMatches(c, tokens) {
			facts = append(facts, c)
		}
	}
	if len(standing) == 0 && len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【用户记忆】\n")
	for _, s := range standing {
		b.WriteString("- " + s + "\n")
	}
	for _, f := range facts {
		b.WriteString("- " + f + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
