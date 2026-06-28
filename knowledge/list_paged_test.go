package knowledge

// ListDocumentsPaged backs the KB list page's pagination + source grouping, so
// a few thousand scheduled snapshots don't ship as one giant payload.

import (
	"context"
	"fmt"
	"testing"
)

func seedDocs(t *testing.T, m *Manager, ctx context.Context, source string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := m.AddDocument(ctx, fmt.Sprintf("%s-doc-%02d", source, i), "正文内容，长度足够切块处理。", source); err != nil {
			t.Fatalf("seed %s #%d: %v", source, i, err)
		}
	}
}

func TestListDocumentsPaged_PageAndTotal(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 25)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if res.Total != 25 {
		t.Fatalf("total must be the full count, got %d", res.Total)
	}
	if len(res.Documents) != 10 {
		t.Fatalf("page must respect limit=10, got %d", len(res.Documents))
	}

	// Second page.
	res2, _ := m.ListDocumentsPaged(ctx, DocListQuery{Limit: 10, Offset: 20})
	if len(res2.Documents) != 5 {
		t.Fatalf("last page must hold the remainder (5), got %d", len(res2.Documents))
	}
	if res2.Total != 25 {
		t.Fatalf("total stays the full count across pages, got %d", res2.Total)
	}
}

func TestListDocumentsPaged_NoLimitReturnsAll(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 7)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{}) // limit 0 → no paging
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(res.Documents) != 7 || res.Total != 7 {
		t.Fatalf("no-limit must return all, got len=%d total=%d", len(res.Documents), res.Total)
	}
}

func TestListDocumentsPaged_SourceFilterAndFacet(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 3)
	seedDocs(t, m, ctx, "beta", 5)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Source: "beta"})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if res.Total != 5 || len(res.Documents) != 5 {
		t.Fatalf("source filter must scope to beta (5), got total=%d len=%d", res.Total, len(res.Documents))
	}
	for _, d := range res.Documents {
		if d.Source != "beta" {
			t.Fatalf("filtered page must contain only beta, got source %q", d.Source)
		}
	}
	// Facet is over the UNFILTERED set so the UI can show all source groups.
	facet := map[string]int{}
	for _, sc := range res.Sources {
		facet[sc.Source] = sc.Count
	}
	if facet["alpha"] != 3 || facet["beta"] != 5 {
		t.Fatalf("facet must count the full set: %+v", res.Sources)
	}
}

func TestListDocumentsPaged_LimitExceedsTotal(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 3)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Limit: 100, Offset: 0})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(res.Documents) != 3 || res.Total != 3 {
		t.Fatalf("limit>total must return all without overrun, got len=%d total=%d", len(res.Documents), res.Total)
	}
}

func TestListDocumentsPaged_SourceFilterNoMatch(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 2)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Source: "不存在"})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if res.Total != 0 || len(res.Documents) != 0 {
		t.Fatalf("no-match source must yield empty page, got total=%d len=%d", res.Total, len(res.Documents))
	}
	if res.Documents == nil {
		t.Fatalf("documents must be non-nil empty slice for JSON")
	}
	// Facet still reflects the full set so the UI can offer the other source.
	if len(res.Sources) != 1 || res.Sources[0].Source != "alpha" {
		t.Fatalf("facet must still list the real sources, got %+v", res.Sources)
	}
}

func TestListDocumentsPaged_NegativeOffsetClamped(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 4)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Limit: 2, Offset: -5})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if res.Offset != 0 || len(res.Documents) != 2 {
		t.Fatalf("negative offset must clamp to 0, got offset=%d len=%d", res.Offset, len(res.Documents))
	}
}

func TestListDocumentsPaged_OffsetBeyondEnd(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	seedDocs(t, m, ctx, "alpha", 2)

	res, err := m.ListDocumentsPaged(ctx, DocListQuery{Limit: 10, Offset: 100})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(res.Documents) != 0 {
		t.Fatalf("offset past the end must yield an empty (non-nil) page, got %d", len(res.Documents))
	}
	if res.Documents == nil {
		t.Fatalf("documents must be non-nil empty slice for JSON")
	}
	if res.Total != 2 {
		t.Fatalf("total must still reflect the set, got %d", res.Total)
	}
}
