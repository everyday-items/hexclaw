package knowledge

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestBug20260717_EmptyKnowledgeBaseSkipsAuxLLMAndEmbedding(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	inner := &readinessGateEmbedder{}
	gated := NewReadinessGatedEmbedder(
		inner,
		func(context.Context) bool { return true },
		true,
		0,
	)
	aux := &fakeAuxLLM{mode: "ok", okResp: "expanded query"}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	mgr := NewManager(store, store, NewTruncatingEmbedder(gated, 0),
		WithHybridConfig(cfg), WithLLM(aux))

	got, err := mgr.Search(ctx, "nothing can match an empty corpus", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty corpus returned %d results", len(got))
	}
	if calls := aux.calls.Load(); calls != 0 {
		t.Fatalf("empty corpus aux LLM calls = %d, want 0", calls)
	}
	if calls := inner.calls.Load(); calls != 0 {
		t.Fatalf("empty corpus embedding calls = %d, want 0", calls)
	}
}

func TestBug20260717_StandbyEmbeddingUsesFTSWithoutAuxLLMOrEndpointCalls(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	inner := &readinessGateEmbedder{}
	var ready atomic.Bool
	gated := NewReadinessGatedEmbedder(
		inner,
		func(context.Context) bool { return ready.Load() },
		false,
		0,
	)
	aux := &fakeAuxLLM{mode: "ok", okResp: "expanded query"}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	mgr := NewManager(store, store, NewTruncatingEmbedder(gated, 0),
		WithSplitter(testSplitter()), WithHybridConfig(cfg), WithLLM(aux))

	if _, err := mgr.AddDocument(ctx, "standby", "hexclaw standby lexical marker", "test"); err != nil {
		t.Fatal(err)
	}
	aux.calls.Store(0)

	got, err := mgr.Search(ctx, "hexclaw standby lexical marker", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("FTS retrieval must remain available while embedding is in standby")
	}
	if calls := aux.calls.Load(); calls != 0 {
		t.Fatalf("standby aux LLM calls = %d, want 0", calls)
	}
	if calls := inner.calls.Load(); calls != 0 {
		t.Fatalf("standby endpoint calls = %d, want 0", calls)
	}
}
