package library

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1) // :memory: 每连接独立库，限单连接共享
	for _, ddl := range []string{
		`CREATE TABLE prompts (id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT 'prompt', title TEXT NOT NULL,
			body_md TEXT NOT NULL DEFAULT '', args_json TEXT NOT NULL DEFAULT '', tool_scope TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE memories (id TEXT PRIMARY KEY, kind TEXT NOT NULL DEFAULT 'fact', content TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

func TestPromptStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s := NewPromptStore(setupDB(t))

	id, err := s.Upsert(ctx, &Prompt{Title: "日报", BodyMD: "总结今天", Enabled: true})
	if err != nil || id == "" {
		t.Fatalf("upsert: id=%q err=%v", id, err)
	}
	// disabled 条目不进 ListEnabled，但进 List。
	if _, err := s.Upsert(ctx, &Prompt{Title: "草稿", Enabled: false}); err != nil {
		t.Fatalf("upsert2: %v", err)
	}

	all, _ := s.List(ctx)
	if len(all) != 2 {
		t.Errorf("List should return all 2, got %d", len(all))
	}
	enabled, _ := s.ListEnabled(ctx)
	if len(enabled) != 1 {
		t.Errorf("ListEnabled should return 1, got %d", len(enabled))
	}

	got, ok, _ := s.Get(ctx, id)
	if !ok || got.Title != "日报" {
		t.Errorf("Get: ok=%v title=%q", ok, got.Title)
	}

	// update existing (same id)
	got.Title = "日报v2"
	if _, err := s.Upsert(ctx, &got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g2, _, _ := s.Get(ctx, id); g2.Title != "日报v2" {
		t.Errorf("update did not persist, title=%q", g2.Title)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := s.Get(ctx, id); ok {
		t.Error("deleted prompt should be gone")
	}
}

func TestRender_Arguments(t *testing.T) {
	p := Prompt{Type: "command", BodyMD: "翻译以下内容：$ARGUMENTS，保持原意"}
	got := Render(p, "Hello world")
	if got != "翻译以下内容：Hello world，保持原意" {
		t.Errorf("Render = %q", got)
	}
	// 无占位 → 原样
	if Render(Prompt{BodyMD: "no placeholder"}, "x") != "no placeholder" {
		t.Error("no-placeholder body should be returned unchanged")
	}
}
