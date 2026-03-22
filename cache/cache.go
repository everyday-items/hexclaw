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
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Entry 缓存条目
type Entry struct {
	Key       string    // 缓存键（输入哈希）
	Response  string    // LLM 响应内容
	Provider  string    // 生成响应的 Provider
	Model     string    // 生成响应的模型
	CreatedAt time.Time // 创建时间
	ExpiresAt time.Time // 过期时间（含抖动）
	HitCount  int       // 命中次数
}

// Cache LLM 响应缓存
//
// 线程安全的内存缓存，支持 TTL 过期和最大条目数限制。
// 使用 LRU 淘汰策略（当前简化为时间淘汰）。
// 使用 singleflight 防止缓存击穿（同一 key 多并发请求只触发一次 LLM 调用）。
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	order      []string // 插入顺序，用于淘汰
	ttl        time.Duration
	ttlJitter  time.Duration // TTL 抖动量，防止缓存雪崩
	maxEntries int
	enabled    bool

	// singleflight 防止缓存击穿
	group singleflight.Group

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
		return &Cache{
			enabled: false,
			entries: make(map[string]*Entry),
		}
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
		ttlJitter:  ttl / 10, // 10% 随机抖动防止缓存雪崩
		maxEntries: maxEntries,
		enabled:    true,
	}
}

// Reconfigure 原地更新缓存配置，避免热更新时替换整个缓存实例。
func (c *Cache) Reconfigure(cfg Options) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.enabled = cfg.Enabled

	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	c.ttl = ttl
	c.ttlJitter = ttl / 10

	maxEntries := cfg.MaxEntries
	if maxEntries == 0 {
		maxEntries = 10000
	}
	c.maxEntries = maxEntries

	if c.enabled {
		c.evictLocked()
	}
}

// Get 查询缓存（按 input + provider + model 隔离，避免不同模型回复互相命中）
func (c *Cache) Get(input, provider, model string) (string, bool) {
	if !c.enabled {
		return "", false
	}

	key := hashInput(input, provider, model)

	c.mu.Lock()
	defer c.mu.Unlock()

	resp, ok := c.getLocked(key)
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return resp, ok
}

// getLocked 内部查询（调用者须持有写锁），不更新统计计数
func (c *Cache) getLocked(key string) (string, bool) {
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}

	if c.isExpired(entry, time.Now()) {
		delete(c.entries, key)
		return "", false
	}

	entry.HitCount++
	return entry.Response, true
}

// Do 缓存击穿防护：对同一 key 的并发请求只执行一次 fn
//
// 如果缓存命中直接返回；否则使用 singleflight 确保
// 同一 key 只有一个 goroutine 调用 fn（LLM 调用等耗时操作）。
func (c *Cache) Do(input, provider, model string, fn func() (response, model string, err error)) (string, error) {
	if !c.enabled {
		resp, _, err := fn()
		return resp, err
	}

	// 先查缓存
	if cached, ok := c.Get(input, provider, model); ok {
		return cached, nil
	}

	key := hashInput(input, provider, model)

	// singleflight：同一 key 只执行一次
	v, err, _ := c.group.Do(key, func() (any, error) {
		// double-check 不更新统计，避免同一逻辑请求重复计数
		c.mu.Lock()
		if cached, ok := c.getLocked(key); ok {
			c.mu.Unlock()
			return cached, nil
		}
		c.mu.Unlock()

		resp, model, fnErr := fn()
		if fnErr != nil {
			return nil, fnErr
		}
		c.Put(input, resp, provider, model)
		return resp, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Put 存入缓存
func (c *Cache) Put(input, response, provider, model string) {
	if !c.enabled {
		return
	}

	key := hashInput(input, provider, model)

	c.mu.Lock()
	defer c.mu.Unlock()

	// 淘汰过期和超量条目
	c.evictLocked()

	// 如果 key 已存在，只更新条目内容，保留 HitCount
	if existing, exists := c.entries[key]; exists {
		existing.Response = response
		existing.Provider = provider
		existing.Model = model
		existing.CreatedAt = time.Now()
		existing.ExpiresAt = c.jitteredExpiry(existing.CreatedAt)
		return
	}

	createdAt := time.Now()
	c.entries[key] = &Entry{
		Key:       key,
		Response:  response,
		Provider:  provider,
		Model:     model,
		CreatedAt: createdAt,
		ExpiresAt: c.jitteredExpiry(createdAt),
	}
	c.order = append(c.order, key)
	c.evictLocked()
}

// Stats 返回缓存统计
type Stats struct {
	Entries int     // 当前条目数
	Hits    int64   // 命中次数
	Misses  int64   // 未命中次数
	HitRate float64 // 命中率
}

// Stats 获取缓存统计信息
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.enabled {
		c.evictLocked()
	}

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
	validOrder := make([]string, 0, len(c.order))
	for _, key := range c.order {
		entry, ok := c.entries[key]
		if !ok {
			continue
		}
		if c.isExpired(entry, now) {
			delete(c.entries, key)
			continue
		}
		validOrder = append(validOrder, key)
	}

	// 超量淘汰（移除最早的条目，确保淘汰后 entries <= maxEntries）
	evictCount := 0
	for len(c.entries)-evictCount > c.maxEntries && evictCount < len(validOrder) {
		delete(c.entries, validOrder[evictCount])
		evictCount++
	}

	// 重新分配 slice 避免 backing array 泄漏
	c.order = append([]string(nil), validOrder[evictCount:]...)
}

func (c *Cache) isExpired(entry *Entry, now time.Time) bool {
	if entry == nil {
		return true
	}
	if !entry.ExpiresAt.IsZero() {
		return !now.Before(entry.ExpiresAt)
	}
	return now.Sub(entry.CreatedAt) > c.ttl
}

// jitteredExpiry 返回带随机抖动的过期时间（防止缓存雪崩）
//
// 抖动范围: [-ttlJitter/2, +ttlJitter/2]，使过期时间均匀分布在 TTL 附近，
// 而非全部偏向提前过期。
func (c *Cache) jitteredExpiry(createdAt time.Time) time.Time {
	if c.ttlJitter <= 0 {
		return createdAt.Add(c.ttl)
	}
	half := c.ttlJitter / 2
	jitter := time.Duration(rand.Int64N(int64(c.ttlJitter))) - half
	return createdAt.Add(c.ttl + jitter)
}

// hashInput 对输入进行归一化和哈希
func hashInput(input, provider, model string) string {
	normalized := strings.TrimSpace(input)
	normalized = strings.ToLower(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")

	var builder strings.Builder
	builder.WriteString(normalized)
	builder.WriteByte('\x00')
	builder.WriteString(strings.ToLower(strings.TrimSpace(provider)))
	builder.WriteByte('\x00')
	builder.WriteString(strings.ToLower(strings.TrimSpace(model)))

	h := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(h[:])
}
