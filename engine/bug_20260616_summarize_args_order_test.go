package engine

// BUG-20260616: summarizeArgs iterated the args map directly when building the
// human-readable permission/audit line. Go randomizes map iteration order, so the
// same tool call rendered its arguments in a different order each time, making the
// approval prompt and audit log non-reproducible. Keys must be sorted.

import "testing"

func TestBug20260616_SummarizeArgsDeterministic(t *testing.T) {
	args := map[string]any{"zebra": 1, "apple": 2, "mango": 3}

	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		seen[summarizeArgs(args)]++
	}
	if len(seen) != 1 {
		t.Fatalf("summarizeArgs non-deterministic across 256 renders: %v", seen)
	}
	const want = "apple=2, mango=3, zebra=1"
	if got := summarizeArgs(args); got != want {
		t.Fatalf("args must render in sorted key order: want %q got %q", want, got)
	}
}
