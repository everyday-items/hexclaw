package memory

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	return err == nil
}

func TestMigrateLegacyMemories(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE memories (id TEXT PRIMARY KEY, kind TEXT, content TEXT, updated_at TIMESTAMP)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	mustInsert := func(id, kind, content string) {
		if _, err := db.ExecContext(ctx, `INSERT INTO memories (id, kind, content, updated_at) VALUES (?,?,?,datetime('now'))`, id, kind, content); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mustInsert("1", "standing", "务必用简体中文回复")
	mustInsert("2", "fact", "用户的项目用 Go 语言")
	mustInsert("3", "fact", "我的密码是 hunter2xyz") // 敏感 → 不迁入
	mustInsert("4", "fact", "   ")              // 空 → 跳过

	fm, err := New(Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	n, err := MigrateLegacyMemories(ctx, db, fm)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 2 {
		t.Fatalf("应迁入 2 条（敏感/空跳过），得 %d", n)
	}

	// standing → rule（落 _global，GetMemory 可见）。
	g := fm.GetMemory()
	if !strings.Contains(g, "简体中文") {
		t.Fatalf("standing 应迁为 rule 落 _global，GetMemory 未含: %q", g)
	}
	// 类型映射核对。
	var rules, facts int
	for _, e := range fm.ParseEntries() {
		switch e.Type {
		case "rule":
			rules++
		case "fact":
			facts++
		}
		if strings.Contains(e.Content, "密码") {
			t.Fatalf("敏感内容不应迁入: %q", e.Content)
		}
	}
	if rules != 1 || facts != 1 {
		t.Fatalf("应 1 rule + 1 fact，得 rules=%d facts=%d", rules, facts)
	}

	// 迁移后表已删（幂等标记）。
	if tableExists(t, db, "memories") {
		t.Fatal("迁移后 memories 表应被删除（幂等）")
	}

	// 幂等：再次迁移（表不存在）→ 0 条、无错。
	n2, err := MigrateLegacyMemories(ctx, db, fm)
	if err != nil || n2 != 0 {
		t.Fatalf("二次迁移应 0 条无错，得 n=%d err=%v", n2, err)
	}
}

// 表不存在（全新装/已迁移）→ 直接 0 条无错。
func TestMigrateLegacyMemories_NoTable(t *testing.T) {
	db := openTestDB(t)
	fm, _ := New(Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200})
	n, err := MigrateLegacyMemories(context.Background(), db, fm)
	if err != nil || n != 0 {
		t.Fatalf("无表应 0 条无错，得 n=%d err=%v", n, err)
	}
}
