// Package library 实现 §11.8 交互层后端：Prompt 库（一库三 type）。
// 用户自管内容，服务端下发，运营增删不发版。
//
// 设计取舍：command 带参走 $ARGUMENTS 纯文本替换（Claude Code 轻做法），
// 严禁演化为命名多槽 + 版本引擎。
//
// 砍薄版（§5）：旧记忆薄版（standing/fact）已并入统一文件记忆（memory.FileMemory），本包不再含记忆。
package library

import (
	"context"
	"database/sql"
	"strings"
	"time"

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
