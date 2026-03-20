package knowledge

import (
	"context"
	"testing"
)

func TestManager_SearchReturnsStructuredHits(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	mgr := NewManager(store, nil)
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

	mgr := NewManager(store, nil)
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
