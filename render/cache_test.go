package render

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestCache(t *testing.T, maxBytes int64, ttl time.Duration) *Cache {
	t.Helper()
	dir := t.TempDir()
	c, err := NewCache(CacheConfig{
		Dir:           filepath.Join(dir, "cache"),
		MaxBytes:      maxBytes,
		TTL:           ttl,
		EngineVersion: "pandoc-test",
		AssetsHash:    "test-assets",
		DefaultLocale: "zh-CN",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCache_KeyStability(t *testing.T) {
	c := newTestCache(t, 0, 0)
	k1 := c.Key("hello", FormatDocx, RenderOptions{Locale: "zh-CN"})
	k2 := c.Key("hello", FormatDocx, RenderOptions{Locale: "zh-CN"})
	if k1 != k2 {
		t.Errorf("same input → different keys: %s vs %s", k1, k2)
	}
}

func TestCache_KeyChangesByDimension(t *testing.T) {
	c := newTestCache(t, 0, 0)
	base := c.Key("hello", FormatDocx, RenderOptions{Locale: "zh-CN"})

	cases := map[string]string{
		"different content":      c.Key("HELLO", FormatDocx, RenderOptions{Locale: "zh-CN"}),
		"different format":       c.Key("hello", FormatPDF, RenderOptions{Locale: "zh-CN"}),
		"different locale":       c.Key("hello", FormatDocx, RenderOptions{Locale: "en-US"}),
		"different allow_html":   c.Key("hello", FormatDocx, RenderOptions{Locale: "zh-CN", AllowRawHTML: true}),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("%s: key didn't change from base", name)
		}
	}
}

func TestCache_KeyChangesByEngineVersion(t *testing.T) {
	dir := t.TempDir()
	c1, _ := NewCache(CacheConfig{Dir: filepath.Join(dir, "1"), EngineVersion: "v1", AssetsHash: "a"})
	c2, _ := NewCache(CacheConfig{Dir: filepath.Join(dir, "2"), EngineVersion: "v2", AssetsHash: "a"})

	k1 := c1.Key("hello", FormatDocx, RenderOptions{})
	k2 := c2.Key("hello", FormatDocx, RenderOptions{})
	if k1 == k2 {
		t.Errorf("engine version change didn't invalidate cache key")
	}
}

func TestCache_KeyChangesByAssetsHash(t *testing.T) {
	dir := t.TempDir()
	c1, _ := NewCache(CacheConfig{Dir: filepath.Join(dir, "1"), EngineVersion: "v", AssetsHash: "a1"})
	c2, _ := NewCache(CacheConfig{Dir: filepath.Join(dir, "2"), EngineVersion: "v", AssetsHash: "a2"})

	k1 := c1.Key("hello", FormatDocx, RenderOptions{})
	k2 := c2.Key("hello", FormatDocx, RenderOptions{})
	if k1 == k2 {
		t.Errorf("assets hash change didn't invalidate cache key")
	}
}

func TestCache_PutAndLookup(t *testing.T) {
	c := newTestCache(t, 0, 0)

	// 创建一个 src 文件
	src := filepath.Join(t.TempDir(), "src.docx")
	if err := os.WriteFile(src, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	key := "abc123"
	dst, err := c.Put(key, src)
	if err != nil {
		t.Fatal(err)
	}
	// src 应该被 rename（不再存在）
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src file still exists after Put: %s", src)
	}
	// dst 应存在
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst file missing: %v", err)
	}

	// Lookup 应命中
	hit, ok := c.Lookup(key, FormatDocx)
	if !ok {
		t.Fatal("cache miss after Put")
	}
	if !hit.CacheHit {
		t.Error("CacheHit flag not set on hit result")
	}
	if hit.Path != dst {
		t.Errorf("hit path = %s, want %s", hit.Path, dst)
	}
	if hit.Size != 11 {
		t.Errorf("hit size = %d, want 11", hit.Size)
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	c := newTestCache(t, 0, 1*time.Millisecond)

	src := filepath.Join(t.TempDir(), "src.docx")
	_ = os.WriteFile(src, []byte("x"), 0o600)
	_, _ = c.Put("k", src)

	time.Sleep(10 * time.Millisecond)

	if _, ok := c.Lookup("k", FormatDocx); ok {
		t.Error("expired entry still found in cache")
	}
}

func TestCache_LRUEviction(t *testing.T) {
	c := newTestCache(t, 5, 0) // 5 字节上限

	for i, n := range []string{"a", "b", "c"} {
		src := filepath.Join(t.TempDir(), n)
		if err := os.WriteFile(src, []byte("xx"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Put(n, src); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	// 总占用 6 字节 > 5 上限，最旧的 "a" 应被淘汰
	if _, ok := c.Lookup("a", FormatTXT); ok {
		t.Error("oldest entry 'a' not evicted")
	}
	if _, ok := c.Lookup("c", FormatTXT); !ok {
		t.Error("newest entry 'c' should still be present")
	}
}
