package knowledge

// BUG-20260613 (audit M3): findBySourceTitle loaded EVERY document via List()
// and linear-scanned for a (source, title) match on each upsert — O(N) per
// ingest, so a knowledge base that grows also slows every scheduled re-ingest.
// It now resolves through the (source, title) index via GetBySourceTitle.
// These tests pin the index-backed lookup's contract: exact match, no
// cross-source bleakage, and (nil, nil) on miss.

import (
	"context"
	"testing"
)

func newM3Store(t *testing.T) (*SQLiteStore, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return store, ctx
}

func TestBug20260613_GetBySourceTitleExactMatch(t *testing.T) {
	store, ctx := newM3Store(t)
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	created, err := mgr.AddDocument(ctx, "热搜榜", "今日热搜内容。", "baidu")
	if err != nil {
		t.Fatalf("AddDocument: %v", err)
	}

	got, err := store.GetBySourceTitle(ctx, "baidu", "热搜榜")
	if err != nil {
		t.Fatalf("GetBySourceTitle: %v", err)
	}
	if got == nil || got.ID != created.ID {
		t.Fatalf("must return the document by (source,title), got %+v", got)
	}
}

func TestBug20260613_GetBySourceTitleMissReturnsNilNil(t *testing.T) {
	store, ctx := newM3Store(t)
	got, err := store.GetBySourceTitle(ctx, "baidu", "不存在的标题")
	if err != nil {
		t.Errorf("a miss must not be an error, got %v", err)
	}
	if got != nil {
		t.Errorf("a miss must return nil document, got %+v", got)
	}
}

// Same title under a different source must NOT collide — the upsert-hit check
// is keyed on the pair, not the title alone.
func TestBug20260613_GetBySourceTitleScopedBySource(t *testing.T) {
	store, ctx := newM3Store(t)
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	a, _ := mgr.AddDocument(ctx, "日报", "来自 A 源。", "source-a")
	b, _ := mgr.AddDocument(ctx, "日报", "来自 B 源。", "source-b")
	if a.ID == b.ID {
		t.Fatalf("same title under different sources must be distinct docs")
	}

	got, err := store.GetBySourceTitle(ctx, "source-b", "日报")
	if err != nil {
		t.Fatalf("GetBySourceTitle: %v", err)
	}
	if got == nil || got.ID != b.ID {
		t.Errorf("must return source-b's doc, not source-a's, got %+v", got)
	}
}

// The upsert path must reuse the existing row (in-place update), not insert a
// duplicate — the behavior findBySourceTitle exists to enable, now via the
// indexed lookup.
func TestBug20260613_UpsertReusesRowViaIndexedLookup(t *testing.T) {
	store, ctx := newM3Store(t)
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	first, _ := mgr.AddDocument(ctx, "榜单", "第一版。", "baidu")
	second, err := mgr.AddDocument(ctx, "榜单", "第二版（更新）。", "baidu")
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-ingest of same (source,title) must update in place, got new id %s vs %s", second.ID, first.ID)
	}

	docs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, d := range docs {
		if d.Source == "baidu" && d.Title == "榜单" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("upsert must leave exactly one row for (baidu,榜单), found %d", n)
	}
}
