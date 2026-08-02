package knowledge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCJKFTSAnalyzerFingerprintIsBoundToIndexVersion(t *testing.T) {
	samples := []string{
		"牛顿第一定律描述物体运动状态的规律。",
		"hello world 你好世界",
		"分数、单位与负号（需要确认）",
		"ひらがな カタカナ 漢字",
		"力",
	}
	projection := make([]string, 0, len(samples))
	for _, sample := range samples {
		projection = append(projection, sample+"\x00"+cjkFTSIndexText(sample))
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(projection, "\x01"))))
	expectedByIndexVersion := map[int]string{
		2: "725eb39c71785154e8dd71b536e339e2ab5a9e3b1f35f43dc0712af68fee55b9",
	}
	if expected, ok := expectedByIndexVersion[cjkFTSIndexVersion]; !ok || fingerprint != expected {
		t.Fatalf("CJK analyzer fingerprint=%s is not registered for index version=%d; bump the index version and add the reviewed fingerprint",
			fingerprint, cjkFTSIndexVersion)
	}
}

func TestCJKFTSV2BackfillsLegacyChunksOnInit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,chunk_count,created_at,updated_at,status,error_message,source_type,deleted)
		VALUES('legacy-cjk','教材','牛顿第一定律描述物体运动状态的规律。','legacy',1,?,?,'indexed','','manual',0)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_chunks
		(id,doc_id,content,chunk_index,created_at)
		VALUES('legacy-cjk-0','legacy-cjk','牛顿第一定律描述物体运动状态的规律。',0,?)`, now); err != nil {
		t.Fatal(err)
	}
	// Simulate an existing database that predates the v2 index.
	if _, err := db.ExecContext(ctx, `DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("backfill CJK FTS v2: %v", err)
	}

	results, err := store.TextSearch(ctx, "牛顿定律", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.Content != "牛顿第一定律描述物体运动状态的规律。" {
		t.Fatalf("backfilled CJK search results=%+v", results)
	}
}

func TestCJKFTSV2RepairsSameCardinalityIdentityDriftOnInit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	doc := &Document{
		ID: "cjk-drift", Title: "教材", Content: "光合作用需要阳光。",
		Source: "test", CreatedAt: time.Now().UTC(),
	}
	chunk := &Chunk{ID: "cjk-drift-0", DocID: doc.ID, Content: doc.Content, Index: 0}
	if err := store.Add(ctx, doc, []*Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM kb_chunks_fts_v2`); err != nil {
		t.Fatal(err)
	}
	// Preserve row cardinality while replacing the indexed identity. A count-only
	// startup check would incorrectly accept this corrupt projection as current.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO kb_chunks_fts_v2(tokens,chunk_id) VALUES('无关 内容','stale-chunk')`); err != nil {
		t.Fatal(err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("repair same-cardinality CJK FTS drift: %v", err)
	}
	results, err := store.TextSearch(ctx, "光合作用", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.ID != chunk.ID {
		t.Fatalf("repaired CJK search results=%+v", results)
	}
	var staleRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_chunks_fts_v2 WHERE chunk_id='stale-chunk'`).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 {
		t.Fatalf("same-cardinality stale CJK row survived startup: %d", staleRows)
	}
}

func TestCJKFTSV2RebuildsStaleTokensAfterLegacyContentMutation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	doc := &Document{
		ID: "cjk-legacy-update", Title: "教材", Content: "光合作用需要阳光。",
		Source: "test", CreatedAt: time.Now().UTC(),
	}
	chunk := &Chunk{ID: "cjk-legacy-update-0", DocID: doc.ID, Content: doc.Content, Index: 0}
	if err := store.Add(ctx, doc, []*Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	// Model a rollback binary or external legacy writer that still updates the
	// canonical chunk but knows nothing about kb_chunks_fts_v2. Identity and row
	// cardinality stay unchanged, so only a durable dirty fence can detect it.
	if _, err := db.ExecContext(ctx,
		`UPDATE kb_chunks SET content='蒸发作用受到温度影响。' WHERE id=?`, chunk.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("rebuild stale CJK tokens after legacy mutation: %v", err)
	}
	var ftsMatches int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_chunks_fts_v2 WHERE kb_chunks_fts_v2 MATCH ?`,
		cjkFTSQuery([]string{"温度", "度影", "影响", "温度影响"}),
	).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if ftsMatches != 1 {
		t.Fatalf("legacy content mutation left stale CJK token rows=%d", ftsMatches)
	}
	results, err := store.TextSearch(ctx, "温度影响", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.ID != chunk.ID ||
		results[0].Chunk.Content != "蒸发作用受到温度影响。" {
		t.Fatalf("rebuilt CJK search results=%+v", results)
	}
}

func TestCJKFTSV2UnrelatedMutationCannotClearPriorDirtyFence(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	staleDoc := &Document{
		ID: "cjk-prior-dirty", Title: "旧教材", Content: "光合作用需要阳光。",
		Source: "test", CreatedAt: now,
	}
	staleChunk := &Chunk{ID: "cjk-prior-dirty-0", DocID: staleDoc.ID, Content: staleDoc.Content, Index: 0}
	if err := store.Add(ctx, staleDoc, []*Chunk{staleChunk}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE kb_chunks SET content='温度影响液体蒸发速度。' WHERE id=?`, staleChunk.ID); err != nil {
		t.Fatal(err)
	}
	// A correct, unrelated v2 mutation must not certify the whole projection:
	// the stale row above was already dirty before this transaction began.
	newDoc := &Document{
		ID: "cjk-unrelated", Title: "新教材", Content: "月球围绕地球运动。",
		Source: "test", CreatedAt: now,
	}
	newChunk := &Chunk{ID: "cjk-unrelated-0", DocID: newDoc.ID, Content: newDoc.Content, Index: 0}
	if err := store.Add(ctx, newDoc, []*Chunk{newChunk}); err != nil {
		t.Fatal(err)
	}
	var markerRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_search_index_metadata
		WHERE index_name='chunks_cjk_fts'`).Scan(&markerRows); err != nil {
		t.Fatal(err)
	}
	if markerRows != 0 {
		t.Fatalf("unrelated mutation cleared a prior dirty CJK projection fence")
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("rebuild projection after prior dirty fence: %v", err)
	}
	var ftsMatches int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_chunks_fts_v2 WHERE kb_chunks_fts_v2 MATCH ?`,
		cjkFTSQuery([]string{"温度", "度影", "影响", "温度影响"}),
	).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if ftsMatches != 1 {
		t.Fatalf("prior dirty CJK row was not rebuilt, matches=%d", ftsMatches)
	}
}

func TestCJKFTSV2AsyncAcceptanceCannotClearPriorDirtyFence(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	store := NewSQLiteStore(db)
	now := time.Now().UTC()
	doc := &Document{
		ID: "cjk-dirty-before-accept", Title: "旧教材", Content: "光合作用需要阳光。",
		Source: "test", CreatedAt: now,
	}
	chunk := &Chunk{ID: doc.ID + "-0", DocID: doc.ID, Content: doc.Content, Index: 0}
	if err := store.Add(ctx, doc, []*Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE kb_chunks SET content='温度影响液体蒸发速度。' WHERE id=?`, chunk.ID); err != nil {
		t.Fatal(err)
	}
	body := "unrelated accepted upload"
	if _, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "cjk-unrelated-accept", Filename: "unrelated.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	}); err != nil {
		t.Fatal(err)
	}
	var markerRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_search_index_metadata
		WHERE index_name='chunks_cjk_fts'`).Scan(&markerRows); err != nil {
		t.Fatal(err)
	}
	if markerRows != 0 {
		t.Fatal("async acceptance cleared a prior dirty CJK projection fence")
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var matches int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_chunks_fts_v2 WHERE kb_chunks_fts_v2 MATCH ?`,
		cjkFTSQuery([]string{"温度", "度影", "影响", "温度影响"}),
	).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 1 {
		t.Fatalf("async acceptance allowed stale CJK tokens, matches=%d", matches)
	}
}

func TestRevisionSemanticSearcherCJKUsesFTSWithoutLikeFallback(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("cjk-textbook", "牛顿第一定律描述物体运动状态的规律。", nil)
	h.bindDocument("owner-1", corpusUID, "cjk-textbook")

	metrics := &retrievalMetricsCollector{}
	ctx := withRetrievalMetrics(h.ctx, metrics)
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", &semanticExecutorRegistry{})
	results, err := searcher.TextSearch(ctx, "牛顿定律", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.Content != "牛顿第一定律描述物体运动状态的规律。" {
		t.Fatalf("revision CJK text results=%+v", results)
	}
	snapshot := metrics.snapshot()
	if snapshot.FTS.Hits != 1 || snapshot.FTS.Fallbacks != 0 || snapshot.Like.Calls != 0 {
		t.Fatalf("revision CJK query did not stay on FTS lane: %+v", snapshot)
	}
}
