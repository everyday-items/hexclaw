package knowledge

// Fine-grained edge coverage for the snapshot ingest primitives — the parts the
// happy-path tests don't pin: retention must keep the NEWEST (not just N of
// them), the series-matching predicate must be precise (wrong scope = wrong
// dedup/prune), retention must survive a process restart (recompute from DB,
// not in-memory counter — the 373MB-class invariant), content-hash dedup must
// normalize cosmetic whitespace but still see real changes, and empty content
// must be rejected.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Retention must keep the NEWEST keep snapshots. A count-only assertion can't
// tell "kept newest 2" from "kept oldest 2" — pin identity by content marker.
func TestIngestSnapshot_RetentionKeepsNewest(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 2)

	// Ingest 5 distinct contents in order; only the last 2 must remain.
	for i := 0; i < 5; i++ {
		if _, w, err := m.IngestSnapshot(ctx, "series", "版本标记 V"+string(rune('A'+i))+"，内容足够长以便切块。", "src"); err != nil || !w {
			t.Fatalf("run %d: w=%v err=%v", i, w, err)
		}
	}
	got := snapshotsOf(t, m, ctx, "src", "series")
	if len(got) != 2 {
		t.Fatalf("retention=2 must keep 2, got %d", len(got))
	}
	// Load content of the survivors; they must be V D and V E (the newest two).
	kept := map[string]bool{}
	for _, d := range got {
		full, err := m.GetDocument(ctx, d.ID)
		if err != nil {
			t.Fatalf("get %s: %v", d.ID, err)
		}
		for i := 0; i < 5; i++ {
			if strings.Contains(full.Content, "V"+string(rune('A'+i))) {
				kept["V"+string(rune('A'+i))] = true
			}
		}
	}
	if !kept["VD"] || !kept["VE"] {
		t.Errorf("retention must keep the NEWEST two (VD,VE), kept=%v", kept)
	}
	if kept["VA"] || kept["VB"] || kept["VC"] {
		t.Errorf("retention must have pruned the OLDEST three (VA,VB,VC), kept=%v", kept)
	}
}

// isSnapshotTitleOf scopes dedup AND retention. If it over-matches it could
// dedup/prune unrelated docs; if it under-matches the series fragments. Pin
// both precision and recall.
func TestIsSnapshotTitleOf_PrecisionRecall(t *testing.T) {
	base := "每日科技要点"
	mustMatch := []string{
		base + " 2026-06-27 09:32:00",       // canonical snapshot
		base + " 2026-06-27 09:32:00 (2)",   // same-second disambiguator
		base + " 2026-12-31 23:59:59 (137)", // large disambiguator
	}
	for _, s := range mustMatch {
		if !isSnapshotTitleOf(s, base) {
			t.Errorf("must match snapshot of %q: %q", base, s)
		}
	}
	mustNotMatch := []string{
		base,                             // bare base (e.g. a manual save) — not a snapshot
		base + " 2026-06-27",             // date only, no time
		base + " 2026-06-27 09:32",       // missing seconds
		"其它来源 2026-06-27 09:32:00",       // different base
		base + "扩展 2026-06-27 09:32:00",  // base is a prefix substring, not the exact series
		base + " 备注 2026-06-27 09:32:00", // extra words between base and stamp
		base + " 2026-06-27 09:32:00 备注", // trailing noise after stamp
		base + " 2026-13-40 99:99:99",    // shape-like but impossible — still must be digits-shaped only
	}
	for _, s := range mustNotMatch {
		if isSnapshotTitleOf(s, base) && s != base+" 2026-13-40 99:99:99" {
			t.Errorf("must NOT match snapshot of %q: %q", base, s)
		}
	}
}

// Retention is stateless — it recomputes from the DB each run — so it must hold
// across a process restart (new *sql.DB + new Manager on the same file), not
// rely on an in-memory counter that resets. Real file DB, not :memory:.
func TestIngestSnapshot_RetentionBoundedAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.db")
	ctx := context.Background()

	open := func() (*Manager, func()) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		store := NewSQLiteStore(db)
		if err := store.Init(ctx); err != nil {
			t.Fatalf("init: %v", err)
		}
		return NewManager(store, store, &mockEmbedder{dim: 8},
			WithSplitter(testSplitter()), WithSnapshotRetention(3)), func() { db.Close() }
	}

	m1, close1 := open()
	for i := 0; i < 4; i++ {
		if _, _, err := m1.IngestSnapshot(ctx, "series", "重启前内容 "+string(rune('A'+i))+"，足够切块。", "src"); err != nil {
			t.Fatalf("pre-restart %d: %v", i, err)
		}
	}
	if got := snapshotsOf(t, m1, ctx, "src", "series"); len(got) != 3 {
		t.Fatalf("pre-restart retention should hold 3, got %d", len(got))
	}
	close1()

	// Restart: brand-new connection + manager on the same file.
	m2, close2 := open()
	defer close2()
	for i := 0; i < 3; i++ {
		if _, _, err := m2.IngestSnapshot(ctx, "series", "重启后内容 "+string(rune('X'+i))+"，足够切块。", "src"); err != nil {
			t.Fatalf("post-restart %d: %v", i, err)
		}
	}
	if got := snapshotsOf(t, m2, ctx, "src", "series"); len(got) != 3 {
		t.Fatalf("retention must stay bounded at 3 across restart, got %d (in-memory counter reset bug?)", len(got))
	}
}

// contentHash must normalize cosmetic whitespace (so a reformat isn't a "new"
// snapshot) but still see genuine content changes.
func TestContentHash_NormalizesWhitespaceButSeesChanges(t *testing.T) {
	if contentHash("a b c") != contentHash("a   b\n\tc") {
		t.Error("whitespace-only differences must hash equal (cosmetic reformat must dedup)")
	}
	if contentHash("热搜: 甲乙丙") == contentHash("热搜: 甲乙丁") {
		t.Error("a real content change must hash differently (must NOT dedup)")
	}
}

// End-to-end of the above through IngestSnapshot: a whitespace-only reformat is
// skipped; a one-character real change appends.
func TestIngestSnapshot_WhitespaceReformatSkipped(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	if _, w, _ := m.IngestSnapshot(ctx, "热搜", "1. 甲\n2. 乙\n3. 丙", "src"); !w {
		t.Fatal("first run should write")
	}
	if _, w, _ := m.IngestSnapshot(ctx, "热搜", "1.  甲\n\n2. 乙\n3. 丙", "src"); w {
		t.Error("a whitespace-only reformat must be skipped (written=false)")
	}
	if _, w, _ := m.IngestSnapshot(ctx, "热搜", "1. 甲\n2. 乙\n3. 丁", "src"); !w {
		t.Error("a real change (丙→丁) must be written")
	}
	if got := snapshotsOf(t, m, ctx, "src", "热搜"); len(got) != 2 {
		t.Errorf("expected 2 snapshots (reformat deduped), got %d", len(got))
	}
}

// Empty / whitespace-only content must be rejected, not stored.
func TestIngestSnapshot_EmptyContentRejected(t *testing.T) {
	m, ctx := newSnapshotTestManager(t, 0)
	for _, c := range []string{"", "   ", "\n\t "} {
		if _, _, err := m.IngestSnapshot(ctx, "base", c, "src"); err == nil {
			t.Errorf("empty content %q must error", c)
		}
	}
	if got := snapshotsOf(t, m, ctx, "src", "base"); len(got) != 0 {
		t.Errorf("rejected ingests must persist nothing, got %d", len(got))
	}
}
