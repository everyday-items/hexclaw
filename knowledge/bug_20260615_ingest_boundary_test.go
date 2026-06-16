package knowledge

// BUG-20260615: boundary/exception coverage for the KB write side of the cron
// "script run → KB write" main chain. Upsert happy paths live in
// bug_20260613_ingest_upsert_test.go; here we pin the edges that a scheduled
// collector actually hits:
//   - empty content rejected before any write
//   - empty title always inserts (cannot upsert on a blank title)
//   - same (source,title) re-ingest is an in-place update (upsert), not a dup,
//     across MANY repeats (idempotent) and across content shrink/grow
//   - oversize content survives the chunk→embed→store round trip
// No source changes.

import (
	"context"
	"strings"
	"testing"
)

func newBoundaryManager(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	return NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter())), ctx
}

// ── content boundaries ────────────────────────────────────────────────────

// Empty content must be rejected before any document row is written.
func TestBug20260615_AddDocument_EmptyContentRejected(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	if _, err := mgr.AddDocument(ctx, "标题", "", "agent"); err == nil {
		t.Fatal("empty content must be rejected")
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 0 {
		t.Errorf("a rejected ingest must write nothing, got %d docs", len(docs))
	}
}

// Whitespace-only content has a non-empty string but no splittable text — the
// chunk builder rejects it, so no partial/zero-chunk document is persisted.
func TestBug20260615_AddDocument_WhitespaceContentRejected(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	if _, err := mgr.AddDocument(ctx, "标题", "   \n\t  ", "agent"); err == nil {
		t.Fatal("whitespace-only content must be rejected (no splittable text)")
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 0 {
		t.Errorf("whitespace content must persist nothing, got %d docs", len(docs))
	}
}

// ── title boundaries ──────────────────────────────────────────────────────

// An empty title cannot be matched for upsert, so two empty-title ingests of
// the same source produce TWO distinct documents (the documented fall-through).
func TestBug20260615_AddDocument_EmptyTitleAlwaysInserts(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	first, err := mgr.AddDocument(ctx, "", "第一段内容，长度足够切块处理。", "agent")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := mgr.AddDocument(ctx, "", "第二段内容，长度足够切块处理。", "agent")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("empty-title ingests must NOT upsert into one doc, both got id %q", first.ID)
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 2 {
		t.Errorf("two empty-title ingests must yield 2 docs, got %d", len(docs))
	}
}

// ── repeated upsert idempotency ───────────────────────────────────────────

// Many repeated ingests of the same (source,title) converge to exactly one
// document carrying the latest content — the upsert is idempotent, not
// accumulating duplicates run after run (the core BUG-20260613 invariant, here
// stressed across 10 repeats with changing content).
func TestBug20260615_AddDocument_RepeatedUpsertConvergesToOne(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	var firstID string
	for i := 0; i < 10; i++ {
		content := strings.Repeat("版本"+string(rune('A'+i))+" ", 50) + "正文内容足够长以便切块。"
		doc, err := mgr.AddDocument(ctx, "每日快照", content, "cron:snap")
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		if i == 0 {
			firstID = doc.ID
		} else if doc.ID != firstID {
			t.Fatalf("ingest %d got a new id %q, want stable %q (upsert broken)", i, doc.ID, firstID)
		}
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 1 {
		t.Fatalf("10 same-(source,title) ingests must converge to 1 doc, got %d", len(docs))
	}
	got, _ := mgr.GetDocument(ctx, firstID)
	if !strings.Contains(got.Content, "版本J") {
		t.Errorf("the surviving doc must hold the LATEST content, got %q", got.Content)
	}
}

// Upsert where the new content is much SHORTER must replace the chunks fully —
// a stale tail chunk from the longer prior version must not survive (Replace,
// not append). We assert the chunk count drops to match the shorter content.
func TestBug20260615_AddDocument_UpsertShrinksChunks(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	// First: long content → many chunks (chunk size 400 in testSplitter).
	long := strings.Repeat("这是一段较长的中文内容，用于产生多个切块。", 80)
	big, err := mgr.AddDocument(ctx, "可变长文档", long, "agent")
	if err != nil {
		t.Fatalf("long ingest: %v", err)
	}
	if big.ChunkCount < 2 {
		t.Fatalf("long content should produce >=2 chunks, got %d", big.ChunkCount)
	}
	// Second: short content → fewer chunks, same doc id.
	small, err := mgr.AddDocument(ctx, "可变长文档", "短内容，仅一小段。", "agent")
	if err != nil {
		t.Fatalf("short ingest: %v", err)
	}
	if small.ID != big.ID {
		t.Errorf("upsert must reuse the id, got %q vs %q", small.ID, big.ID)
	}
	if small.ChunkCount >= big.ChunkCount {
		t.Errorf("shrinking content must reduce chunk count, big=%d small=%d", big.ChunkCount, small.ChunkCount)
	}
	got, _ := mgr.GetDocument(ctx, big.ID)
	if got.ChunkCount != small.ChunkCount {
		t.Errorf("stored chunk count must reflect the replaced (shorter) doc, got %d want %d", got.ChunkCount, small.ChunkCount)
	}
}

// ── oversize content ──────────────────────────────────────────────────────

// A large collected page (~512 KiB) must survive chunk→embed→store intact: the
// document persists, splits into many chunks, and the stored content round-trips
// byte-for-byte.
func TestBug20260615_AddDocument_OversizeContentRoundTrips(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	big := strings.Repeat("0123456789abcdef", 512*1024/16) // exactly 512 KiB
	doc, err := mgr.AddDocument(ctx, "大页面", big, "cron:big")
	if err != nil {
		t.Fatalf("oversize ingest: %v", err)
	}
	if doc.ChunkCount < 10 {
		t.Errorf("512 KiB must split into many chunks, got %d", doc.ChunkCount)
	}
	got, err := mgr.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Content) != len(big) {
		t.Errorf("oversize content must round-trip un-truncated, got %d bytes want %d", len(got.Content), len(big))
	}
}

// Source-type tagging: a cron-ingested document (source "cron:..." or "agent")
// is classified as "agent", not "file" — the KB UI groups these as automated.
func TestBug20260615_AddDocument_CronSourceTypeIsAgent(t *testing.T) {
	mgr, ctx := newBoundaryManager(t)
	doc, err := mgr.AddDocument(ctx, "热搜快照", "采集到的热搜内容，足够切块。", "cron:hotsearch")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got, _ := mgr.GetDocument(ctx, doc.ID)
	if got.SourceType != "agent" {
		t.Errorf("cron-sourced doc must be tagged source_type=agent, got %q", got.SourceType)
	}
}
