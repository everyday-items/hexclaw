package knowledge

import (
	"regexp"
	"testing"
	"time"
)

// snapshotTitle must append a YYYY-MM-DD-HH-MM-SS suffix so every scheduled run
// gets a distinct title (and thus a new document under the (source,title)
// upsert), instead of overwriting the previous run's snapshot.
func TestSnapshotTitle_AppendsTimestampSuffix(t *testing.T) {
	at := time.Date(2026, 6, 26, 23, 38, 2, 0, time.UTC)

	got := snapshotTitle("百度热搜 TOP20", at)
	want := "百度热搜 TOP20 2026-06-26 23:38:02"
	if got != want {
		t.Fatalf("snapshotTitle = %q, want %q", got, want)
	}
}

// Two runs a second apart must yield different titles — the whole point is that
// they no longer collide on the upsert.
func TestSnapshotTitle_DistinctAcrossRuns(t *testing.T) {
	a := snapshotTitle("热搜", time.Date(2026, 6, 26, 23, 38, 2, 0, time.UTC))
	b := snapshotTitle("热搜", time.Date(2026, 6, 26, 23, 38, 3, 0, time.UTC))
	if a == b {
		t.Fatalf("snapshots one second apart must differ, both = %q", a)
	}
}

// An empty/whitespace base title degrades to the bare timestamp (no leading
// space, no empty-title document).
func TestSnapshotTitle_EmptyBaseUsesBareTimestamp(t *testing.T) {
	at := time.Date(2026, 6, 26, 23, 38, 2, 0, time.UTC)
	for _, base := range []string{"", "   ", "\t"} {
		got := snapshotTitle(base, at)
		if got != "2026-06-26 23:38:02" {
			t.Errorf("snapshotTitle(%q) = %q, want bare timestamp", base, got)
		}
	}
}

// Lock the exported wrapper to the documented layout so a layout change is a
// deliberate, test-visible decision.
func TestSnapshotTitle_LayoutShape(t *testing.T) {
	if SnapshotTitleLayout != "2006-01-02 15:04:05" {
		t.Fatalf("unexpected layout %q", SnapshotTitleLayout)
	}
	re := regexp.MustCompile(` \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)
	if got := SnapshotTitle("doc"); !re.MatchString(got) {
		t.Fatalf("SnapshotTitle suffix shape wrong: %q", got)
	}
}
