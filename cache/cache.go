// Package cache 提供 LLM 响应语义缓存
//
// 语义缓存通过对用户输入进行哈希匹配，复用相同/相似问题的 LLM 响应，
// 大幅减少 API 调用次数和成本。
//
// 当前版本使用精确匹配（归一化后的输入哈希）。
// TODO: v2 版本接入向量化实现真正的语义相似度匹配。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Entry 缓存条目
type Entry struct {
	Key       string    // 缓存键（输入哈希）
	Response  string    // LLM 响应内容
	Provider  string    // 生成响应的 Provider
	Model     string    // 生成响应的模型
	CreatedAt time.Time // 创建时间
	HitCount  int       // 命中次数
}

// Cache LLM 响应缓存
//
// 线程安全的内存缓存，支持 TTL 过期和最大条目数限制。
// 使用 LRU 淘汰策略（当前简化为时间淘汰）。
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	order      []string // 插入顺序，用于淘汰
	ttl        time.Duration
	maxEntries int
	enabled    bool

	// 统计
	hits   int64
	misses int64
}

// Options 缓存配置选项
type Options struct {
	Enabled    bool
	TTL        time.Duration
	MaxEntries int
}

// New 创建缓存实例
func New(cfg Options) *Cache {
	if !cfg.Enabled {
		return &Cache{enabled: false}
	}

	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	maxEntries := cfg.MaxEntries
	if maxEntries == 0 {
		maxEntries = 10000
	}

	return &Cache{
		entries:    make(map[string]*Entry),
		ttl:        ttl,
		maxEntries: maxEntries,
		enabled:    true,
	}
}

// Get 查询缓存
//
// 对输入进行归一化和哈希，查找匹配的缓存条目。
// 命中时返回响应内容和 true，未命中返回空字符串和 false。
func (c *Cache) Get(input string) (string, bool) {
	if !c.enabled {
		return "", false
	}

	key := hashInput(input)

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return "", false
	}

	// 检查是否过期
	if time.Since(entry.CreatedAt) > c.ttl {
		c.mu.Lock()
		delete(c.entries, key)
		c.misses++
		c.mu.Unlock()
		return "", false
	}

	c.mu.Lock()
	entry.HitCount++
	c.hits++
	c.mu.Unlock()

	return entry.Response, true
}

// Put 存入缓存
func (c *Cache) Put(input, response, provider, model string) {
	if !c.enabled || response == "" {
		return
	}

	key := hashInput(input)

	c.mu.Lock()
	defer c.mu.Unlock()

	// 淘汰过期和超量条目
	c.evictLocked()

	// 如果 key 已存在，只更新条目内容，不重复追加到 order 切片
	if _, exists := c.entries[key]; exists {
		c.entries[key] = &Entry{
			Key:       key,
			Response:  response,
			Provider:  provider,
			Model:     model,
			CreatedAt: time.Now(),
		}
		return
	}

	c.entries[key] = &Entry{
		Key:       key,
		Response:  response,
		Provider:  provider,
		Model:     model,
		CreatedAt: time.Now(),
	}
	c.order = append(c.order, key)
}

// Stats 返回缓存统计
type Stats struct {
	Entries int   // 当前条目数
	Hits    int64 // 命中次数
	Misses  int64 // 未命中次数
	HitRate float64 // 命中率
}

// Stats 获取缓存统计信息
func (c *Cache) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}

	return Stats{
		Entries: len(c.entries),
		Hits:    c.hits,
		Misses:  c.misses,
		HitRate: hitRate,
	}
}

// evictLocked 淘汰过期和超量条目（调用者须持有写锁）
func (c *Cache) evictLocked() {
	now := time.Now()

	// 淘汰过期条目
	validOrder := c.order[:0]
	for _, key := range c.order {
		entry, ok := c.entries[key]
		if !ok {
			continue
		}
		if now.Sub(entry.CreatedAt) > c.ttl {
			delete(c.entries, key)
			continue
		}
		validOrder = append(validOrder, key)
	}
	c.order = validOrder

	// 超量淘汰（移除最早的条目）
	for len(c.entries) >= c.maxEntries && len(c.order) > 0 {
		oldKey := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldKey)
	}
}

// hashInput 对输入进行归一化和哈希
//
// 归一化步骤：
//  1. 去除首尾空白
//  2. 转小写
//  3. 合并连续空白为单个空格
//  4. SHA-256 哈希
func hashInput(input string) string {
	// 归一化
	normalized := strings.TrimSpace(input)
	normalized = strings.ToLower(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")

	// 哈希
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}
