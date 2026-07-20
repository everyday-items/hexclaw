package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPrepareIngestPagesNarrowsStructuredOffsetsThroughChunking(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()), WithHybridConfig(cfg))
	prefix := "page-prefix:"
	pageText := strings.Repeat("unique algebra fraction geometry sentence. ", 40)
	doc := &Document{ID: "doc-page-split", Content: prefix + pageText, Source: "upload:book.pdf"}
	digest := strings.Repeat("b", 64)
	chunks, err := mgr.PrepareIngestPages(ctx, doc, []SourcePage{{
		PageStart: 9, PageEnd: 9, Text: pageText, SourceDigest: digest,
		SourceOffsetStart: int64(len(prefix)), SourceOffsetEnd: int64(len(prefix + pageText)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("fixture did not split: chunks=%d", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.PageStart != 9 || chunk.PageEnd != 9 || chunk.SourceDigest != digest ||
			chunk.SourceOffsetEnd <= chunk.SourceOffsetStart ||
			chunk.SourceOffsetEnd > int64(len(doc.Content)) {
			t.Fatalf("invalid chunk source span: %+v", chunk)
		}
		sourceText := doc.Content[chunk.SourceOffsetStart:chunk.SourceOffsetEnd]
		if sourceText == pageText || !strings.Contains(chunk.Content, sourceText) {
			t.Fatalf("offset was not narrowed to chunk content: source=%q chunk=%q", sourceText, chunk.Content)
		}
	}
}

func TestManagerSearchReturnsStructuredSourceSpan(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	doc := &Document{
		ID: "doc-span", Title: "教材", Content: "第三页分数练习", Source: "upload:lesson.pdf",
		SourceType: "upload", Status: "indexed", ChunkCount: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Add(ctx, doc, []*Chunk{{
		ID: "chunk-span", DocID: doc.ID, DocTitle: doc.Title, Source: doc.Source,
		SourceType: doc.SourceType, ChunkCount: 1, Content: doc.Content, Index: 0, CreatedAt: now,
		PageStart: 3, PageEnd: 3, SourceDigest: digest,
		SourceOffsetStart: 120, SourceOffsetEnd: 144,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()))
	hits, err := mgr.Search(ctx, "分数练习", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%+v", hits)
	}
	hit := hits[0]
	if hit.PageStart != 3 || hit.PageEnd != 3 || hit.SourceDigest != digest ||
		hit.SourceOffsetStart != 120 || hit.SourceOffsetEnd != 144 {
		t.Fatalf("search source span=%+v", hit)
	}
}

func TestManager_SearchReturnsStructuredHits(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()))
	doc, err := mgr.AddDocument(ctx, "SQLite Guide", "SQLite is a lightweight embedded database.", "upload:sqlite.txt")
	if err != nil {
		t.Fatalf("添加文档失败: %v", err)
	}

	hits, err := mgr.Search(ctx, "SQLite database", 3)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("期望至少 1 条命中")
	}

	hit := hits[0]
	if hit.DocID != doc.ID || hit.DocTitle != doc.Title {
		t.Fatalf("文档元信息不正确: %+v", hit)
	}
	if hit.Source != "upload:sqlite.txt" {
		t.Fatalf("source 不正确: %+v", hit)
	}
	if hit.ChunkCount != doc.ChunkCount || hit.ChunkID == "" {
		t.Fatalf("chunk 元信息不正确: %+v", hit)
	}
}

func TestManager_ReindexDocumentUpdatesUpdatedAt(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()))
	doc, err := mgr.AddDocument(ctx, "Doc", "first sentence. second sentence.", "manual")
	if err != nil {
		t.Fatalf("添加文档失败: %v", err)
	}
	before := doc.UpdatedAt

	reindexed, err := mgr.ReindexDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("重建索引失败: %v", err)
	}
	if !reindexed.UpdatedAt.After(before) && !reindexed.UpdatedAt.Equal(before) {
		t.Fatalf("updated_at 未更新: before=%v after=%v", before, reindexed.UpdatedAt)
	}
	if reindexed.Status != "indexed" {
		t.Fatalf("status 不正确: %+v", reindexed)
	}
}
