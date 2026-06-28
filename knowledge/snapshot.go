package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// DefaultSnapshotRetention is the per-series snapshot cap used when the config
// leaves it unset. 0 means "keep everything" (no pruning).
const DefaultSnapshotRetention = 100

// SnapshotTitleLayout is the wall-clock timestamp appended to a scheduled-task
// ("snapshot") document title. Each scheduled run thus produces a uniquely
// titled document, so the (source, title) upsert in AddDocument inserts a new
// row instead of overwriting the previous run's document.
//
// A scheduled collector (e.g. "百度热搜") is a time series, not a single
// living document: re-running it must append a new snapshot, not replace the
// last one. The interactive save path keeps the upsert dedup (it does not call
// this), so a user re-saving the same note still updates in place.
//
// The hour is 24-hour, two-digit (15) on purpose: a 12-hour hour would make
// 09:xx and 21:xx collide into one title and silently overwrite again.
const SnapshotTitleLayout = "2006-01-02 15:04:05"

// SnapshotTitle returns title with the current wall-clock timestamp appended,
// using SnapshotTitleLayout (e.g. "百度热搜 TOP20 2026-06-27 09:32:00").
func SnapshotTitle(title string) string {
	return snapshotTitle(title, time.Now())
}

// snapshotTitle is the deterministic core of SnapshotTitle, taking the clock as
// a parameter so the suffix is unit-testable. An empty/whitespace-only base
// title degrades to the bare timestamp rather than a leading space.
func snapshotTitle(title string, t time.Time) string {
	stamp := t.Format(SnapshotTitleLayout)
	title = strings.TrimSpace(title)
	if title == "" {
		return stamp
	}
	return title + " " + stamp
}

// snapshotStampRe matches the timestamp suffix a snapshot title carries,
// optionally followed by a " (N)" disambiguator added when two snapshots of the
// same series land in the same second. Used to recognize the documents that
// belong to one snapshot series (so dedup/retention act on that series only,
// never on unrelated docs that happen to share a source).
var snapshotStampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}( \(\d+\))?$`)

// isSnapshotTitleOf reports whether docTitle is a snapshot of the series whose
// base title is baseTitle — i.e. exactly "<baseTitle> <timestamp>" (with the
// snapshot layout). The bare baseTitle (no suffix) does not count: a series is
// only ever written with a suffix, and excluding it avoids capturing a
// same-titled interactive save.
func isSnapshotTitleOf(docTitle, baseTitle string) bool {
	base := strings.TrimSpace(baseTitle)
	if base == "" {
		return false
	}
	rest, ok := strings.CutPrefix(docTitle, base+" ")
	if !ok {
		return false
	}
	return snapshotStampRe.MatchString(rest)
}

// contentHash returns a stable hex digest of content for skip-if-unchanged
// dedup. Whitespace is normalized first so a cosmetic reformat doesn't count as
// a change.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(content), " ")))
	return hex.EncodeToString(sum[:])
}
