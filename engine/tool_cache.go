package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// ToolCache caches tool execution results to avoid duplicate calls.
//
// Key = sha256(tool_name + json(args)). TTL is per-tool configurable.
// Destructive tools (shell, code_exec, file_ops write/edit) skip cache.
type ToolCache struct {
	mu         sync.RWMutex
	entries    map[string]*cacheEntry
	maxEntries int
	defaultTTL time.Duration
	ttlOverrides map[string]time.Duration // tool_name → custom TTL

	hits   int64
	misses int64
}

type cacheEntry struct {
	result    string
	createdAt time.Time
	ttl       time.Duration
}

func (e *cacheEntry) expired() bool {
	return time.Since(e.createdAt) > e.ttl
}

// NewToolCache creates a tool result cache.
func NewToolCache(maxEntries int, defaultTTL time.Duration) *ToolCache {
	if maxEntries <= 0 {
		maxEntries = 500
	}
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &ToolCache{
		entries:      make(map[string]*cacheEntry),
		maxEntries:   maxEntries,
		defaultTTL:   defaultTTL,
		ttlOverrides: make(map[string]time.Duration),
	}
}

// SetTTL sets a custom TTL for a specific tool.
func (c *ToolCache) SetTTL(toolName string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttlOverrides[toolName] = ttl
}

// destructive tools that should never be cached
var uncacheableTools = map[string]bool{
	"shell": true, "code": true, "code_exec": true,
	"create_skill": true, "manage_skill": true, "manage_mcp_server": true,
}

// Get returns a cached result if available and not expired.
func (c *ToolCache) Get(toolName string, args map[string]any) (string, bool) {
	if uncacheableTools[toolName] {
		return "", false
	}

	key := cacheKey(toolName, args)

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || entry.expired() {
		c.misses++
		if ok && entry.expired() {
			delete(c.entries, key)
		}
		return "", false
	}

	c.hits++
	return entry.result, true
}

// Put stores a tool result in the cache.
func (c *ToolCache) Put(toolName string, args map[string]any, result string) {
	if uncacheableTools[toolName] {
		return
	}

	key := cacheKey(toolName, args)
	ttl := c.defaultTTL
	c.mu.RLock()
	if override, ok := c.ttlOverrides[toolName]; ok {
		ttl = override
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict oldest if at capacity
	if len(c.entries) >= c.maxEntries {
		c.evictOldest()
	}

	c.entries[key] = &cacheEntry{
		result:    result,
		createdAt: time.Now(),
		ttl:       ttl,
	}
}

// Stats returns cache hit/miss counts.
func (c *ToolCache) Stats() (hits, misses int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses
}

func (c *ToolCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for k, v := range c.entries {
		if oldestKey == "" || v.createdAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.createdAt
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func cacheKey(toolName string, args map[string]any) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	if args != nil {
		data, _ := json.Marshal(args)
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
