package knowledge

// Benchmark for the M3 fix: GetBySourceTitle resolves an upsert hit via the
// (source, title) index instead of List()-scanning every document. Times a
// single lookup against a seeded corpus — per-op cost stays roughly flat as the
// corpus grows (index), instead of climbing linearly (the old full scan).

import (
	"context"
	"fmt"
	"testing"
)

func benchLookupAtScale(b *testing.B, n int) {
	_, store, ctx := benchSetup(b, n)
	target := fmt.Sprintf("文档-%d", n/2) // a middle row (worst-ish for a linear scan)

	// Sanity: the target resolves before timing.
	if doc, err := store.GetBySourceTitle(ctx, "bench", target); err != nil || doc == nil {
		b.Fatalf("setup lookup miss: doc=%v err=%v", doc, err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.GetBySourceTitle(context.Background(), "bench", target); err != nil {
			b.Fatalf("lookup: %v", err)
		}
	}
}

func BenchmarkGetBySourceTitle_100(b *testing.B)  { benchLookupAtScale(b, 100) }
func BenchmarkGetBySourceTitle_500(b *testing.B)  { benchLookupAtScale(b, 500) }
func BenchmarkGetBySourceTitle_2000(b *testing.B) { benchLookupAtScale(b, 2000) }
