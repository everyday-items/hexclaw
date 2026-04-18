package cache

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// StartCleanupLoop 启动后台定时清理：
//   - 每 interval 清一次内存中过期条目（evictLocked 已实现惰性清理，但仅在写入时触发，
//     长期无写入场景下过期条目会堆积）
//   - 同步删除 DB 中过期条目 + 超过 maxEntries 的旧条目（保留最近 maxEntries 条）
//
// 为什么要做：历史状况 llm_cache 条目数虽小（21 条 / 52KB），但若流量上来，
// 没有后台清理会让 DB 跟内存一样长期膨胀。
// 返回 stop 函数用于优雅停止。
func (c *Cache) StartCleanupLoop(ctx context.Context, db *sql.DB, interval time.Duration) func() {
	if !c.enabled || interval <= 0 {
		return func() {}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				c.runCleanup(db)
			}
		}
	}()
	return cancel
}

// runCleanup 执行一轮清理（内存 + DB）
func (c *Cache) runCleanup(db *sql.DB) {
	c.mu.Lock()
	c.evictLocked()
	maxEntries := c.maxEntries
	c.mu.Unlock()

	if db == nil {
		return
	}

	now := time.Now()
	if _, err := db.Exec(
		`DELETE FROM llm_cache WHERE expires_at <= ? OR trim(response) = ''`, now,
	); err != nil {
		logger.Error("[cache] 清理过期 DB 条目失败", "error", err)
		return
	}

	// 超量裁剪：保留最近 maxEntries 条（按 created_at 降序）
	if _, err := db.Exec(`
		DELETE FROM llm_cache
		WHERE key IN (
			SELECT key FROM llm_cache
			ORDER BY created_at DESC
			LIMIT -1 OFFSET ?
		)
	`, maxEntries); err != nil {
		logger.Error("[cache] 裁剪 DB 超量条目失败", "error", err)
	}
}

// LoadFromDB 从 SQLite 加载未过期的缓存条目到内存
//
// 在应用启动时调用，恢复上次运行的缓存数据，减少冷启动后的 API 开销。
func (c *Cache) LoadFromDB(db *sql.DB) {
	if !c.enabled || db == nil {
		return
	}
	if _, err := db.Exec("DELETE FROM llm_cache WHERE trim(response) = ''"); err != nil {
		logger.Error("[cache] 清理空回复缓存失败", "error", err)
	}

	rows, err := db.Query(
		`SELECT key, response, provider, model, hit_count, created_at, expires_at
		 FROM llm_cache WHERE expires_at > ?`, time.Now(),
	)
	if err != nil {
		logger.Error("[cache] 从 SQLite 加载缓存失败", "error", err)
		return
	}
	defer rows.Close()

	c.mu.Lock()
	defer c.mu.Unlock()

	loaded := 0
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Key, &e.Response, &e.Provider, &e.Model, &e.HitCount, &e.CreatedAt, &e.ExpiresAt); err != nil {
			continue
		}
		if strings.TrimSpace(e.Response) == "" {
			continue
		}
		if _, exists := c.entries[e.Key]; exists {
			continue // 内存中已有，跳过
		}
		c.entries[e.Key] = &e
		c.order = append(c.order, e.Key)
		loaded++
		if loaded >= c.maxEntries {
			break
		}
	}

	if loaded > 0 {
		logger.Info("[cache] 从 SQLite 恢复", "loaded", loaded)
	}
}

// PersistToDB 将当前内存缓存的未过期条目写入 SQLite
//
// 在应用关闭前调用，确保缓存数据不会随进程退出而丢失。
func (c *Cache) PersistToDB(db *sql.DB) {
	if !c.enabled || db == nil {
		return
	}

	c.mu.RLock()
	entries := make([]*Entry, 0, len(c.entries))
	now := time.Now()
	for _, e := range c.entries {
		if !c.isExpired(e, now) && strings.TrimSpace(e.Response) != "" {
			entries = append(entries, e)
		}
	}
	c.mu.RUnlock()

	if len(entries) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Error("[cache] 开启持久化事务失败", "error", err)
		return
	}
	defer tx.Rollback()

	// 清理过期条目
	tx.Exec("DELETE FROM llm_cache WHERE expires_at <= ? OR trim(response) = ''", now)

	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO llm_cache (key, response, provider, model, hit_count, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		logger.Error("[cache] 准备持久化语句失败", "error", err)
		return
	}
	defer stmt.Close()

	persisted := 0
	for _, e := range entries {
		if _, err := stmt.Exec(e.Key, e.Response, e.Provider, e.Model, e.HitCount, e.CreatedAt, e.ExpiresAt); err != nil {
			logger.Error("[cache] 持久化条目失败", "error", err)
			continue
		}
		persisted++
	}

	if err := tx.Commit(); err != nil {
		logger.Error("[cache] 提交持久化事务失败", "error", err)
		return
	}

	logger.Info("[cache] 持久化", "persisted", persisted)
}
