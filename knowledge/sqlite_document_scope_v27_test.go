package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestSQLiteStoreCorpusScopeMatrixIsolatesCRUDAndSearch(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "scope-matrix.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	base := NewSQLiteStore(db)
	if err := base.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeDocumentScopeV27,
	}); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	for _, owner := range []string{"owner-a", "owner-b"} {
		if _, err := repository.BindLegacyDefaultCorpus(ctx, owner, "default"); err != nil {
			t.Fatalf("bind %s/default: %v", owner, err)
		}
	}

	storeA := NewSQLiteStore(db, WithSQLiteSemanticMutations("owner-a", "default"))
	storeB := NewSQLiteStore(db, WithSQLiteSemanticMutations("owner-b", "default"))
	embedder := &mockEmbedder{dim: 8}
	managerA := NewManager(storeA, storeA, embedder, WithSplitter(testSplitter()))
	managerB := NewManager(storeB, storeB, embedder, WithSplitter(testSplitter()))
	docA, err := managerA.AddDocument(ctx, "lesson.txt", "alpha_scope_token", "upload:lesson.txt")
	if err != nil {
		t.Fatal(err)
	}
	docB, err := managerB.AddDocument(ctx, "lesson.txt", "beta_scope_token", "upload:lesson.txt")
	if err != nil {
		t.Fatalf("same source/title in another corpus: %v", err)
	}
	if docA.ID == docB.ID {
		t.Fatal("cross-corpus add reused document id")
	}

	for _, testCase := range []struct {
		name                string
		store               *SQLiteStore
		ownID, foreignID    string
		token, foreignToken string
	}{
		{"a", storeA, docA.ID, docB.ID, "alpha_scope_token", "beta_scope_token"},
		{"b", storeB, docB.ID, docA.ID, "beta_scope_token", "alpha_scope_token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			docs, err := testCase.store.List(ctx)
			if err != nil || len(docs) != 1 || docs[0].ID != testCase.ownID {
				t.Fatalf("list=%+v err=%v", docs, err)
			}
			if _, err := testCase.store.Get(ctx, testCase.foreignID); err == nil {
				t.Fatal("cross-corpus detail unexpectedly visible")
			}
			hits, err := testCase.store.TextSearch(ctx, testCase.token, 10, Filter{})
			if err != nil || len(hits) == 0 {
				t.Fatalf("own search hits=%+v err=%v", hits, err)
			}
			for _, hit := range hits {
				if hit.Chunk == nil || hit.Chunk.DocID != testCase.ownID {
					t.Fatalf("search leaked foreign hit: %+v", hit)
				}
			}
			foreignHits, err := testCase.store.TextSearch(ctx, testCase.foreignToken, 10, Filter{})
			if err != nil || len(foreignHits) != 0 {
				t.Fatalf("foreign text search hits=%+v err=%v", foreignHits, err)
			}
			queryVector, err := embedder.EmbedOne(ctx, testCase.token)
			if err != nil {
				t.Fatal(err)
			}
			vectorHits, err := testCase.store.VectorSearch(ctx, queryVector, 10, Filter{})
			if err != nil || len(vectorHits) == 0 {
				t.Fatalf("vector search hits=%+v err=%v", vectorHits, err)
			}
			for _, hit := range vectorHits {
				if hit.Chunk == nil || hit.Chunk.DocID != testCase.ownID {
					t.Fatalf("vector search leaked foreign hit: %+v", hit)
				}
			}
		})
	}

	if err := managerB.DeleteDocument(ctx, docA.ID); err == nil {
		t.Fatal("cross-corpus delete unexpectedly succeeded")
	}
	if _, err := managerA.GetDocument(ctx, docA.ID); err != nil {
		t.Fatalf("cross-corpus delete mutated owner document: %v", err)
	}
	if _, err := managerB.ReindexDocument(ctx, docA.ID); err == nil {
		t.Fatal("cross-corpus reindex unexpectedly succeeded")
	}
	if err := managerA.DeleteDocument(ctx, docA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := managerA.GetDocument(ctx, docA.ID); err == nil {
		t.Fatal("owner delete did not tombstone document")
	}
	if got, err := managerB.GetDocument(ctx, docB.ID); err != nil || got.ID != docB.ID {
		t.Fatalf("owner-a delete affected owner-b: got=%+v err=%v", got, err)
	}

	// Keep the not-found contract implementation-agnostic: callers only need a
	// fail-closed error, not a sentinel exposed by the legacy repository API.
	if _, err := storeA.Get(ctx, docB.ID); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cross-scope detail error: %v", err)
	}
}
