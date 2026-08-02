package knowledge

// BUG-20260613: AddDocument always inserted a new doc with a fresh ID, so a
// scheduled "summarize and add to KB" job accumulated a duplicate document
// every run (the live model even hallucinated a "uniqueness conflict" that the
// code never enforced). Ingesting the same (source, title) now updates the
// existing document in place and bumps UpdatedAt instead of duplicating.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
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

func newProductionWALUpsertManagers(t *testing.T, count int) (*sql.DB, []*Manager, context.Context) {
	t.Helper()
	if count < 1 {
		t.Fatal("manager count must be positive")
	}

	dbPath := filepath.Join(t.TempDir(), "knowledge.db")
	dsn := dbPath +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=wal_autocheckpoint(1000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open production WAL fixture: %v", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init production WAL fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique ON kb_documents(source, title) WHERE source != ''`); err != nil {
		t.Fatalf("create production identity index: %v", err)
	}

	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode=%q, want wal", journalMode)
	}
	var busyTimeout int
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout=%d, want 5000", busyTimeout)
	}
	if got := db.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("MaxOpenConnections=%d, want 4", got)
	}

	managers := make([]*Manager, 0, count)
	for range count {
		store := NewSQLiteStore(db)
		managers = append(managers,
			NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter())))
	}
	return db, managers, ctx
}

func assertLegacyKnowledgeProjectionConsistent(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var duplicateIdentities int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT source, title FROM kb_documents
		WHERE source != '' GROUP BY source, title HAVING COUNT(*) > 1
	)`).Scan(&duplicateIdentities); err != nil {
		t.Fatalf("count duplicate identities: %v", err)
	}
	if duplicateIdentities != 0 {
		t.Fatalf("duplicate source/title identities=%d", duplicateIdentities)
	}

	var orphanChunks int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks AS c
		LEFT JOIN kb_documents AS d ON d.id = c.doc_id WHERE d.id IS NULL`).Scan(&orphanChunks); err != nil {
		t.Fatalf("count orphan chunks: %v", err)
	}
	if orphanChunks != 0 {
		t.Fatalf("orphan chunks=%d", orphanChunks)
	}

	var chunks, ftsRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks`).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks_fts_v2`).Scan(&ftsRows); err != nil {
		t.Fatalf("count FTS rows: %v", err)
	}
	if chunks != ftsRows {
		t.Fatalf("chunk/FTS projection drift: chunks=%d fts=%d", chunks, ftsRows)
	}
}

// Review C1/C2: with the production unique index present, a read-then-insert
// race must NOT surface a raw UNIQUE-constraint 500 — AddDocument retries the
// losing insert as an in-place update. This adds the prod index the default
// test schema omits, then hammers the same (source,title) concurrently.
func TestBug20260613_IngestRaceNoConstraintError(t *testing.T) {
	db, managers, ctx := newProductionWALUpsertManagers(t, 4)

	const n = 16
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(k int) {
			<-start
			_, err := managers[k%len(managers)].AddDocument(
				ctx,
				"并发标题",
				fmt.Sprintf("内容版本 %02d，足够长以便切块处理。", k),
				"agent",
			)
			errs <- err
		}(i)
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent ingest must not error (got constraint 500?): %v", err)
		}
	}
	docs, err := managers[0].ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list converged documents: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("concurrent same-(source,title) ingest must converge to 1 doc, got %d", len(docs))
	}
	assertLegacyKnowledgeProjectionConsistent(t, db)
}

func TestBug20260802_019ProductionWALDifferentIdentitiesAndDeletesConverge(t *testing.T) {
	db, managers, ctx := newProductionWALUpsertManagers(t, 4)
	const n = 16
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(k int) {
			<-start
			_, err := managers[k%len(managers)].AddDocument(
				ctx,
				fmt.Sprintf("独立标题-%02d", k),
				fmt.Sprintf("独立内容-%02d，足够长以便切块处理。", k),
				"agent",
			)
			errs <- err
		}(i)
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent distinct-identity ingest: %v", err)
		}
	}
	docs, err := managers[0].ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list distinct documents: %v", err)
	}
	if len(docs) != n {
		t.Fatalf("distinct identities=%d, want %d", len(docs), n)
	}
	assertLegacyKnowledgeProjectionConsistent(t, db)

	deleteStart := make(chan struct{})
	deleteErrs := make(chan error, n)
	for i, doc := range docs {
		go func(k int, docID string) {
			<-deleteStart
			deleteErrs <- managers[k%len(managers)].DeleteDocument(ctx, docID)
		}(i, doc.ID)
	}
	close(deleteStart)
	for i := 0; i < n; i++ {
		if err := <-deleteErrs; err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	}
	docs, err = managers[0].ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list after deletes: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("documents after exact-set delete=%d, want 0", len(docs))
	}
	assertLegacyKnowledgeProjectionConsistent(t, db)
}

func TestBug20260802_019ProductionWALContextCancellationLeavesNoPartialProjection(t *testing.T) {
	db, managers, ctx := newProductionWALUpsertManagers(t, 1)
	locker, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("hold production write lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = managers[0].AddDocument(
		cancelCtx,
		"取消中的写入",
		"该内容不得在 context 取消后留下半提交的文档、chunk 或 FTS 投影。",
		"agent",
	)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error=%v, want context cancellation", err)
	}
	if _, rollbackErr := locker.ExecContext(context.Background(), `ROLLBACK`); rollbackErr != nil {
		t.Fatalf("release production write lock: %v", rollbackErr)
	}
	locked = false

	var documents int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_documents WHERE source='agent' AND title='取消中的写入'`).Scan(&documents); err != nil {
		t.Fatalf("count cancelled documents: %v", err)
	}
	if documents != 0 {
		t.Fatalf("cancelled write left documents=%d", documents)
	}
	assertLegacyKnowledgeProjectionConsistent(t, db)
}
