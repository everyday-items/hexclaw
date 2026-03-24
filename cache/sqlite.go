package cache

import (
	"database/sql"
	"log"
	"time"
)

// LoadFromDB 从 SQLite 加载未过期的缓存条目到内存
//
// 在应用启动时调用，恢复上次运行的缓存数据，减少冷启动后的 API 开销。
func (c *Cache) LoadFromDB(db *sql.DB) {
	if !c.enabled || db == nil {
		return
	}

	rows, err := db.Query(
		`SELECT key, response, provider, model, hit_count, created_at, expires_at
		 FROM llm_cache WHERE expires_at > ?`, time.Now(),
	)
	if err != nil {
		log.Printf("[cache] 从 SQLite 加载缓存失败: %v", err)
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
		log.Printf("[cache] 从 SQLite 恢复 %d 条缓存", loaded)
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
		if !c.isExpired(e, now) {
			entries = append(entries, e)
		}
	}
	c.mu.RUnlock()

	if len(entries) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[cache] 开启持久化事务失败: %v", err)
		return
	}
	defer tx.Rollback()

	// 清理过期条目
	tx.Exec("DELETE FROM llm_cache WHERE expires_at <= ?", now)

	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO llm_cache (key, response, provider, model, hit_count, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		log.Printf("[cache] 准备持久化语句失败: %v", err)
		return
	}
	defer stmt.Close()

	persisted := 0
	for _, e := range entries {
		if _, err := stmt.Exec(e.Key, e.Response, e.Provider, e.Model, e.HitCount, e.CreatedAt, e.ExpiresAt); err != nil {
			log.Printf("[cache] 持久化条目失败: %v", err)
			continue
		}
		persisted++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[cache] 提交持久化事务失败: %v", err)
		return
	}

	log.Printf("[cache] 持久化 %d 条缓存到 SQLite", persisted)
}
