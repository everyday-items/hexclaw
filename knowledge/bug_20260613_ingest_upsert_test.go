package knowledge

// BUG-20260613: AddDocument always inserted a new doc with a fresh ID, so a
// scheduled "summarize and add to KB" job accumulated a duplicate document
// every run (the live model even hallucinated a "uniqueness conflict" that the
// code never enforced). Ingesting the same (source, title) now updates the
// existing document in place and bumps UpdatedAt instead of duplicating.

import (
	"context"
	"database/sql"
	"testing"
)

func newUpsertTestManager(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	return NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter())), ctx
}

func TestBug20260613_IngestSameSourceTitleUpdatesInPlace(t *testing.T) {
	mgr, ctx := newUpsertTestManager(t)

	first, err := mgr.AddDocument(ctx, "科技要点总结", "第一版内容，足够长以便切分成块。", "agent")
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	second, err := mgr.AddDocument(ctx, "科技要点总结", "第二版内容，已更新并替换原文档。", "agent")
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("same (source,title) must reuse the doc, got new id %q vs %q", second.ID, first.ID)
	}

	docs, err := mgr.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("ingesting same (source,title) twice must yield 1 doc, got %d", len(docs))
	}

	got, err := mgr.GetDocument(ctx, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "第二版内容，已更新并替换原文档。" {
		t.Errorf("content must be replaced with the latest, got %q", got.Content)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Errorf("UpdatedAt must be refreshed, created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestBug20260613_IngestDifferentTitleStillInserts(t *testing.T) {
	mgr, ctx := newUpsertTestManager(t)

	// Hot-search style: title carries the date, so each day is a new snapshot.
	if _, err := mgr.AddDocument(ctx, "百度热搜 2026-06-12", "内容一，长度足够切块。", "agent"); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if _, err := mgr.AddDocument(ctx, "百度热搜 2026-06-13", "内容二，长度足够切块。", "agent"); err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 2 {
		t.Errorf("distinct titles must stay distinct docs, got %d", len(docs))
	}
}

func TestBug20260613_IngestSameTitleDifferentSourceStaysDistinct(t *testing.T) {
	mgr, ctx := newUpsertTestManager(t)

	if _, err := mgr.AddDocument(ctx, "周报", "用户手写的周报内容，足够切块。", "user"); err != nil {
		t.Fatalf("ingest user: %v", err)
	}
	if _, err := mgr.AddDocument(ctx, "周报", "agent 生成的周报内容，足够切块。", "agent"); err != nil {
		t.Fatalf("ingest agent: %v", err)
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 2 {
		t.Errorf("same title from different sources must stay distinct, got %d", len(docs))
	}
}

// Review C1/C2: with the production unique index present, a read-then-insert
// race must NOT surface a raw UNIQUE-constraint 500 — AddDocument retries the
// losing insert as an in-place update. This adds the prod index the default
// test schema omits, then hammers the same (source,title) concurrently.
func TestBug20260613_IngestRaceNoConstraintError(t *testing.T) {
	// Shared-cache file:memory DB so concurrent connections see one schema
	// (plain ":memory:" gives each connection its own empty DB).
	db, err := sql.Open("sqlite", "file:kbrace?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	// Mirror production: storage/migrate idx_kb_documents_unique(source,title).
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique ON kb_documents(source, title) WHERE source != ''`); err != nil {
		t.Fatalf("create index: %v", err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(k int) {
			_, err := mgr.AddDocument(ctx, "并发标题", "内容版本，足够长以便切块处理。", "agent")
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent ingest must not error (got constraint 500?): %v", err)
		}
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 1 {
		t.Errorf("concurrent same-(source,title) ingest must converge to 1 doc, got %d", len(docs))
	}
}
