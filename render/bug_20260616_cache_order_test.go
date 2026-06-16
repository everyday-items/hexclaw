package render

// BUG-20260616: Cache.Put unconditionally appended the key to c.order, even when
// the key already existed. Re-Put of the same key (concurrent or repeat render)
// left len(order) > len(entries); evictLocked removed only the first occurrence,
// so a stale duplicate consumed an eviction slot and skewed LRU order.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBug20260616_CachePutSameKeyNoDupOrder(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(CacheConfig{Dir: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	mkSrc := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := c.Put("k", mkSrc("a.txt")); err != nil {
		t.Fatalf("put1: %v", err)
	}
	if _, err := c.Put("k", mkSrc("b.txt")); err != nil {
		t.Fatalf("put2: %v", err)
	}

	if len(c.order) != len(c.entries) {
		t.Fatalf("order/entries invariant broken: order=%d entries=%d", len(c.order), len(c.entries))
	}
	if len(c.order) != 1 {
		t.Fatalf("re-Put of same key duplicated order: %v", c.order)
	}
}
