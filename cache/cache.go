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

// Get 查询缓存（按 input + provider 隔离，避免不同模型回复互相命中）
func (c *Cache) Get(input, provider string) (string, bool) {
	if !c.enabled {
		return "", false
	}

	key := hashInput(input, provider)

	// 单次加写锁完成全部操作，避免 RLock→Unlock→Lock 之间的 TOCTOU 竞态
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		c.misses++
		return "", false
	}

	// 检查是否过期
	if time.Since(entry.CreatedAt) > c.ttl {
		delete(c.entries, key)
		c.misses++
		return "", false
	}

	entry.HitCount++
	c.hits++
	return entry.Response, true
}

// Do 缓存击穿防护：对同一 key 的并发请求只执行一次 fn
//
// 如果缓存命中直接返回；否则使用 singleflight 确保
// 同一 key 只有一个 goroutine 调用 fn（LLM 调用等耗时操作）。
func (c *Cache) Do(input, provider string, fn func() (response, model string, err error)) (string, error) {
	if !c.enabled {
		resp, _, err := fn()
		return resp, err
	}

	// 先查缓存
	if cached, ok := c.Get(input, provider); ok {
		return cached, nil
	}

	key := hashInput(input, provider)

	// singleflight：同一 key 只执行一次
	v, err, _ := c.group.Do(key, func() (any, error) {
		// double-check：可能在排队期间另一个请求已填充缓存
		if cached, ok := c.Get(input, provider); ok {
			return cached, nil
		}
		resp, model, err := fn()
		if err != nil {
			return nil, err
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

	key := hashInput(input, provider)

	c.mu.Lock()
	defer c.mu.Unlock()

	// 淘汰过期和超量条目
	c.evictLocked()

	// 如果 key 已存在，只更新条目内容，保留 HitCount
	if existing, exists := c.entries[key]; exists {
		existing.Response = response
		existing.Provider = provider
		existing.Model = model
		existing.CreatedAt = c.jitteredNow()
		return
	}

	c.entries[key] = &Entry{
		Key:       key,
		Response:  response,
		Provider:  provider,
		Model:     model,
		CreatedAt: c.jitteredNow(),
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
	validOrder := make([]string, 0, len(c.order))
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

	// 超量淘汰（移除最早的条目，使用 > 确保淘汰后 entries < maxEntries）
	evictCount := 0
	for len(c.entries)-evictCount > c.maxEntries-1 && evictCount < len(validOrder) {
		delete(c.entries, validOrder[evictCount])
		evictCount++
	}

	// 重新分配 slice 避免 backing array 泄漏
	c.order = append([]string(nil), validOrder[evictCount:]...)
}

// jitteredNow 返回带随机抖动的当前时间（防止缓存雪崩）
func (c *Cache) jitteredNow() time.Time {
	if c.ttlJitter <= 0 {
		return time.Now()
	}
	jitter := time.Duration(rand.Int64N(int64(c.ttlJitter)))
	return time.Now().Add(-jitter)
}

// hashInput 对输入进行归一化和哈希
func hashInput(input, provider string) string {
	normalized := strings.TrimSpace(input)
	normalized = strings.ToLower(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")

	var builder strings.Builder
	builder.WriteString(normalized)
	builder.WriteByte('\x00')
	builder.WriteString(strings.ToLower(strings.TrimSpace(provider)))

	h := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(h[:])
}
