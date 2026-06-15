package knowledge

// BUG-20260615 (audit 🟡): two Manager defects.
//   1. findBySourceTitle swallowed REAL DB errors as "not found" → a transient
//      DB failure during upsert silently inserted a duplicate instead of
//      surfacing the error. AddDocument must propagate a genuine DB error.
//   2. NewManager accepted nil repo/searcher and deferred the nil-deref panic to
//      deep inside a request. It must fail fast at construction.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dbErrRepo wraps a real store but forces GetBySourceTitle to fail, simulating a
// transient DB error on the upsert-hit lookup.
type dbErrRepo struct {
	*SQLiteStore
	err error
}

func (r *dbErrRepo) GetBySourceTitle(_ context.Context, _, _ string) (*Document, error) {
	return nil, r.err
}

func TestBug20260615_AddDocument_PropagatesDBError(t *testing.T) {
	store, ctx := newM3Store(t)
	repo := &dbErrRepo{SQLiteStore: store, err: errors.New("db connection lost")}
	mgr := NewManager(repo, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	_, err := mgr.AddDocument(ctx, "标题", "正文内容。", "src")
	if err == nil || !strings.Contains(err.Error(), "db connection lost") {
		t.Fatalf("a real DB error from GetBySourceTitle must propagate, got: %v", err)
	}
}

func TestBug20260615_NewManager_FailsFastOnNilDeps(t *testing.T) {
	store, _ := newM3Store(t)
	mustPanic := func(name string, fn func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("NewManager with nil %s must panic (fail-fast), it did not", name)
			}
		}()
		fn()
	}
	mustPanic("repo", func() { NewManager(nil, store, nil) })
	mustPanic("searcher", func() { NewManager(store, nil, nil) })
}
