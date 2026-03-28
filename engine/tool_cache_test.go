package engine

import (
	"testing"
	"time"
)

func TestToolCache_HitAndMiss(t *testing.T) {
	c := NewToolCache(100, 5*time.Minute)

	args := map[string]any{"query": "weather in Beijing"}

	// Miss
	if _, ok := c.Get("search", args); ok {
		t.Fatal("expected cache miss")
	}

	// Put
	c.Put("search", args, "sunny 25°C")

	// Hit
	result, ok := c.Get("search", args)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if result != "sunny 25°C" {
		t.Fatalf("expected 'sunny 25°C', got %q", result)
	}

	hits, misses := c.Stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("expected 1 hit 1 miss, got %d/%d", hits, misses)
	}
}

func TestToolCache_TTLExpiry(t *testing.T) {
	c := NewToolCache(100, 50*time.Millisecond)

	args := map[string]any{"q": "test"}
	c.Put("search", args, "result")

	// Immediate hit
	if _, ok := c.Get("search", args); !ok {
		t.Fatal("expected hit before TTL")
	}

	time.Sleep(60 * time.Millisecond)

	// Should expire
	if _, ok := c.Get("search", args); ok {
		t.Fatal("expected miss after TTL")
	}
}

func TestToolCache_UncacheableTools(t *testing.T) {
	c := NewToolCache(100, 5*time.Minute)

	for _, tool := range []string{"shell", "code", "code_exec", "create_skill"} {
		c.Put(tool, map[string]any{"x": 1}, "result")
		if _, ok := c.Get(tool, map[string]any{"x": 1}); ok {
			t.Fatalf("tool %q should not be cached", tool)
		}
	}
}

func TestToolCache_Eviction(t *testing.T) {
	c := NewToolCache(3, 5*time.Minute)

	for i := 0; i < 5; i++ {
		c.Put("search", map[string]any{"i": i}, "result")
	}

	c.mu.RLock()
	count := len(c.entries)
	c.mu.RUnlock()

	if count > 3 {
		t.Fatalf("expected max 3 entries, got %d", count)
	}
}

func TestToolCache_CustomTTL(t *testing.T) {
	c := NewToolCache(100, 5*time.Minute)
	c.SetTTL("weather", 30*time.Millisecond)

	args := map[string]any{"loc": "Beijing"}
	c.Put("weather", args, "sunny")

	if _, ok := c.Get("weather", args); !ok {
		t.Fatal("expected hit before custom TTL")
	}

	time.Sleep(40 * time.Millisecond)

	if _, ok := c.Get("weather", args); ok {
		t.Fatal("expected miss after custom TTL")
	}
}
