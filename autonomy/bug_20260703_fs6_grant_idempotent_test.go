package autonomy

// FS-6（BUG-20260703）：grant 落库后 resume/enable 失败时前端会重试建 grant，
// GrantStore.Create 无幂等 → 同一 (task_ref, source, entries) 堆叠多条重复授权。
// 契约：相同活跃授权重复 Create 返回既有那条，不新增。

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newGrantTestStore(t *testing.T) *GrantStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := NewGrantStore(db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func TestBug20260703_GrantCreateIdempotent(t *testing.T) {
	s := newGrantTestStore(t)
	ctx := context.Background()

	g1, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"files", "delivery"}})
	if err != nil {
		t.Fatalf("Create#1: %v", err)
	}
	// 重试：entries 顺序不同但集合相同 → 应视为同一授权。
	g2, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"delivery", "files"}})
	if err != nil {
		t.Fatalf("Create#2: %v", err)
	}

	if g2.ID != g1.ID {
		t.Errorf("[FS-6] 重复授权应返回既有 ID %q，实际新建 %q", g1.ID, g2.ID)
	}
	if active := s.ListActive("cron:j1"); len(active) != 1 {
		t.Errorf("[FS-6] 相同授权重复 Create 应只有 1 条活跃，实际 %d", len(active))
	}
}

// 不同 entries / 不同 task_ref 仍应各自新建（幂等不能误合并）。
func TestBug20260703_GrantCreateDistinctStillCreates(t *testing.T) {
	s := newGrantTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"files"}}); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"delivery"}}); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:j2", Source: "cron", Entries: []string{"files"}}); err != nil {
		t.Fatalf("Create C: %v", err)
	}
	if active := s.ListActive("cron:j1"); len(active) != 2 {
		t.Errorf("不同 entries 应各自新建，cron:j1 期望 2 条，实际 %d", len(active))
	}
	if active := s.ListActive("cron:j2"); len(active) != 1 {
		t.Errorf("cron:j2 期望 1 条，实际 %d", len(active))
	}
}

// 已撤销的授权不参与幂等匹配：撤销后重新 Create 应新建活跃授权。
func TestBug20260703_GrantCreateAfterRevokeRecreates(t *testing.T) {
	s := newGrantTestStore(t)
	ctx := context.Background()

	g1, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"files"}})
	if err != nil {
		t.Fatalf("Create#1: %v", err)
	}
	if err := s.Revoke(ctx, g1.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	g2, err := s.Create(ctx, Grant{TaskRef: "cron:j1", Source: "cron", Entries: []string{"files"}})
	if err != nil {
		t.Fatalf("Create#2: %v", err)
	}
	if g2.ID == g1.ID {
		t.Error("[FS-6] 撤销后重建不应复用已撤销授权 ID")
	}
	if active := s.ListActive("cron:j1"); len(active) != 1 {
		t.Errorf("撤销后重建应有 1 条活跃，实际 %d", len(active))
	}
}
