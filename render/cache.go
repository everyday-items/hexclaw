package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Cache 磁盘 LRU 缓存。
//
// 缓存键 = sha256(content + format + canonical_options + locale + assets_hash + engine_version)
// 缓存值 = sandbox/cache/<sha256>.<ext> 文件
//
// 命中时返回 cache file 路径，HTTP handler 走 ServeContent 流式下发，
// 不读到 Go heap。LRU 达到上限或 TTL 过期时清理。
type Cache struct {
	dir            string
	maxBytes       int64
	ttl            time.Duration
	engineVersion  string
	assetsHash     string
	defaultLocale  string

	mu      sync.Mutex
	entries map[string]*cacheEntry // key = cacheKey, value = entry
	order   []string               // LRU access order, oldest first
}

type cacheEntry struct {
	Path       string
	Size       int64
	AccessedAt time.Time
	CreatedAt  time.Time
}

// CacheConfig 缓存配置。
type CacheConfig struct {
	// Dir 缓存目录（建议在 sandbox 之内单独子目录）。
	Dir string
	// MaxBytes 缓存总字节上限（达到上限触发 LRU 淘汰）。零值 = 不限。
	MaxBytes int64
	// TTL 条目过期时间。零值 = 不过期。
	TTL time.Duration
	// EngineVersion 引擎版本字符串（如 "pandoc-3.9.0.2+typst-0.13.1"）。
	// 任一引擎升级 → 字符串变化 → 旧缓存哈希全部失效。
	EngineVersion string
	// AssetsHash renderer_assets_hash（来自 AssetsHash()）。
	AssetsHash string
	// DefaultLocale sidecar 默认 locale，参与缓存键计算。
	DefaultLocale string
}

// NewCache 构造缓存；Dir 必填。启动时不会读盘恢复历史条目（保持简单）；
// LRU 在运行期生效，进程重启后旧文件会作为孤儿存在直到超 maxBytes 触发清理。
func NewCache(cfg CacheConfig) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("render/cache: Dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	return &Cache{
		dir:           cfg.Dir,
		maxBytes:      cfg.MaxBytes,
		ttl:           cfg.TTL,
		engineVersion: cfg.EngineVersion,
		assetsHash:    cfg.AssetsHash,
		defaultLocale: cfg.DefaultLocale,
		entries:       make(map[string]*cacheEntry),
	}, nil
}

// Key 计算缓存键。所有影响输出的维度都进 hash。
func (c *Cache) Key(content string, format Format, opts RenderOptions) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte("|F:"))
	h.Write([]byte(format))

	// canonical options（字段排序后 marshal）
	optMap := map[string]any{
		"AllowRawHTML": opts.AllowRawHTML,
		"AllowRawTeX":  opts.AllowRawTeX,
		"Locale":       opts.Locale,
	}
	keys := make([]string, 0, len(optMap))
	for k := range optMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	canonical := make([][2]any, 0, len(keys))
	for _, k := range keys {
		canonical = append(canonical, [2]any{k, optMap[k]})
	}
	enc, _ := json.Marshal(canonical)
	h.Write([]byte("|O:"))
	h.Write(enc)

	h.Write([]byte("|L:"))
	h.Write([]byte(opts.effectiveLocale(c.defaultLocale)))

	h.Write([]byte("|A:"))
	h.Write([]byte(c.assetsHash))

	h.Write([]byte("|V:"))
	h.Write([]byte(c.engineVersion))

	return hex.EncodeToString(h.Sum(nil))
}

// Lookup 查找缓存。命中返回 RenderResult{CacheHit: true}，否则返回 nil, false。
func (c *Cache) Lookup(key string, format Format) (*RenderResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// TTL 过期
	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		c.evictLocked(key)
		return nil, false
	}
	// 文件被外部清理掉了
	stat, err := os.Stat(entry.Path)
	if err != nil {
		c.evictLocked(key)
		return nil, false
	}
	entry.AccessedAt = time.Now()
	c.touchLocked(key)
	return &RenderResult{
		Path:        entry.Path,
		Size:        stat.Size(),
		ContentType: format.MIME(),
		CacheHit:    true,
	}, true
}

// Put 把渲染产物存入缓存（移动而非复制，原 path 不再可用）。
func (c *Cache) Put(key string, srcPath string) (string, error) {
	stat, err := os.Stat(srcPath)
	if err != nil {
		return "", err
	}

	dst := filepath.Join(c.dir, key+filepath.Ext(srcPath))
	if err := os.Rename(srcPath, dst); err != nil {
		// 跨 fs rename 失败时降级为复制（很少见）
		if err := copyFile(srcPath, dst); err != nil {
			return "", err
		}
		_ = os.Remove(srcPath)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{
		Path:       dst,
		Size:       stat.Size(),
		AccessedAt: time.Now(),
		CreatedAt:  time.Now(),
	}
	c.order = append(c.order, key)
	c.evictIfOverLimitLocked()
	return dst, nil
}

func (c *Cache) touchLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			return
		}
	}
}

func (c *Cache) evictLocked(key string) {
	if entry, ok := c.entries[key]; ok {
		_ = os.Remove(entry.Path)
		delete(c.entries, key)
	}
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *Cache) evictIfOverLimitLocked() {
	if c.maxBytes <= 0 {
		return
	}
	var total int64
	for _, e := range c.entries {
		total += e.Size
	}
	for total > c.maxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		if entry, ok := c.entries[oldest]; ok {
			total -= entry.Size
		}
		c.evictLocked(oldest)
	}
}

func copyFile(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()
	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer dstF.Close()
	_, err = io.Copy(dstF, srcF)
	return err
}
