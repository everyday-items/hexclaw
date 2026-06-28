package knowledge

// IngestSnapshot is the scheduled-task write path: append (never overwrite),
// skip-if-unchanged, and per-series retention. These lock the three behaviors
// against regression.

import (
	"context"
	"fmt"
	"testing"
)

func newSnapshotTestManager(t *testing.T, retention int) (*Manager, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	return NewManager(store, store, &mockEmbedder{dim: 8},
		WithSplitter(testSplitter()), WithSnapshotRetention(retention)), ctx
}

func snapshotsOf(t *testing.T, m *Manager, ctx context.Context, source, base string) []*Document {
	t.Helper()
	all, err := m.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []*Document
	for _, d := range all {
		if d.Source == source && isSnapshotTitleOf(d.Title, base) {
			out = append(out, d)
		}
	}
	return out
}

// Changed content across runs must accumulate distinct, non-overwriting
// documents (the whole point: a time series).
func TestIngestSnapshot_AppendsChangedRuns(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)

	d1, w1, err := m.IngestSnapshot(ctx, "百度热搜 TOP20", "第一版内容，足够长以便切块。", "Baidu Hot Search")
	if err != nil || !w1 {
		t.Fatalf("run1: doc=%v written=%v err=%v", d1, w1, err)
	}
	d2, w2, err := m.IngestSnapshot(ctx, "百度热搜 TOP20", "第二版内容，已变化，足够长以便切块。", "Baidu Hot Search")
	if err != nil || !w2 {
		t.Fatalf("run2: written=%v err=%v", w2, err)
	}
	if d1.ID == d2.ID {
		t.Fatalf("changed runs must be distinct documents, both id=%s", d1.ID)
	}
	if d1.Title == d2.Title {
		t.Fatalf("snapshots must have distinct titles, both %q", d1.Title)
	}
	if got := snapshotsOf(t, m, ctx, "Baidu Hot Search", "百度热搜 TOP20"); len(got) != 2 {
		t.Fatalf("two changed runs must keep 2 snapshots, got %d", len(got))
	}
}

// Unchanged content must be skipped — no new document, no re-embed.
func TestIngestSnapshot_SkipsUnchanged(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)

	d1, w1, err := m.IngestSnapshot(ctx, "热搜", "完全一样的内容，长度足够切块。", "src")
	if err != nil || !w1 {
		t.Fatalf("run1: %v %v", w1, err)
	}
	d2, w2, err := m.IngestSnapshot(ctx, "热搜", "完全一样的内容，长度足够切块。", "src")
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if w2 {
		t.Fatalf("identical content must be skipped (written=false)")
	}
	if d2.ID != d1.ID {
		t.Fatalf("skip must return the existing latest doc, got %s want %s", d2.ID, d1.ID)
	}
	if got := snapshotsOf(t, m, ctx, "src", "热搜"); len(got) != 1 {
		t.Fatalf("unchanged re-run must not add a doc, got %d", len(got))
	}

	// A later change after an unchanged run still appends.
	if _, w3, _ := m.IngestSnapshot(ctx, "热搜", "这次变了，长度足够切块处理。", "src"); !w3 {
		t.Fatalf("changed content after a skip must be written")
	}
	if got := snapshotsOf(t, m, ctx, "src", "热搜"); len(got) != 2 {
		t.Fatalf("expected 2 snapshots after change, got %d", len(got))
	}
}

// Retention caps a series to the newest N snapshots.
func TestIngestSnapshot_RetentionPrunesOldest(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 3)

	for i := 0; i < 6; i++ {
		if _, w, err := m.IngestSnapshot(ctx, "series", fmt.Sprintf("内容版本 %d，长度足够切块处理。", i), "src"); err != nil || !w {
			t.Fatalf("run %d: written=%v err=%v", i, w, err)
		}
	}
	got := snapshotsOf(t, m, ctx, "src", "series")
	if len(got) != 3 {
		t.Fatalf("retention=3 must keep 3 snapshots, got %d", len(got))
	}
}

// A cron snapshot is autonomous → source_type "agent", even when the (model-
// supplied) source label would otherwise classify as "file" (real-LLM E2E
// caught the model attaching labels like "用户输入" → was mis-typed "file").
func TestIngestSnapshot_SourceTypeIsAgent(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	for _, src := range []string{"用户输入", "Baidu Hot Search", "some-file-label"} {
		doc, _, err := m.IngestSnapshot(ctx, "系列", "内容，长度足够切块处理。"+src, src)
		if err != nil {
			t.Fatalf("ingest source=%q: %v", src, err)
		}
		if doc.SourceType != "agent" {
			t.Errorf("cron snapshot source=%q must be source_type=agent, got %q", src, doc.SourceType)
		}
	}
}

// Two series (different base title, or different source) must not dedup or prune
// against each other.
func TestIngestSnapshot_SeriesIsolation(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 2)

	// Same content, different base titles → both written (no cross-series dedup).
	if _, w, _ := m.IngestSnapshot(ctx, "甲", "同样的内容，长度足够切块处理。", "src"); !w {
		t.Fatalf("series 甲 first run should write")
	}
	if _, w, _ := m.IngestSnapshot(ctx, "乙", "同样的内容，长度足够切块处理。", "src"); !w {
		t.Fatalf("series 乙 must write despite identical content in series 甲")
	}
	// Fill series 甲 past its cap; series 乙 must be untouched.
	for i := 0; i < 3; i++ {
		m.IngestSnapshot(ctx, "甲", fmt.Sprintf("甲内容 %d，长度足够切块处理。", i), "src")
	}
	if got := snapshotsOf(t, m, ctx, "src", "甲"); len(got) != 2 {
		t.Fatalf("series 甲 retention=2 should keep 2, got %d", len(got))
	}
	if got := snapshotsOf(t, m, ctx, "src", "乙"); len(got) != 1 {
		t.Fatalf("series 乙 must be untouched by 甲's pruning, got %d", len(got))
	}
}

// No-overwrite invariant under rapid distinct-content writes: even if several
// land in the same wall-clock second, titles are disambiguated and every run is
// retained (none clobbers another).
func TestIngestSnapshot_NoOverwriteRapidWrites(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)

	const n = 5
	titles := map[string]bool{}
	for i := 0; i < n; i++ {
		d, w, err := m.IngestSnapshot(ctx, "rapid", fmt.Sprintf("快速第 %d 版内容，长度足够切块处理。", i), "src")
		if err != nil || !w {
			t.Fatalf("rapid run %d: written=%v err=%v", i, w, err)
		}
		if titles[d.Title] {
			t.Fatalf("rapid run %d produced a duplicate title %q (would overwrite)", i, d.Title)
		}
		titles[d.Title] = true
	}
	if got := snapshotsOf(t, m, ctx, "src", "rapid"); len(got) != n {
		t.Fatalf("all %d rapid writes must be retained, got %d", n, len(got))
	}
}
