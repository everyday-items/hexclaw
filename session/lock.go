package session

import (
	"sync"
	"sync/atomic"
	"time"
)

// SessionLock serializes concurrent requests per session.
//
// Design: sync.Map<sessionID, *lockEntry>.
// Fix 11: Entries now track lastAccess time and can be cleaned up
// via Cleanup() to prevent unbounded memory growth.
type SessionLock struct {
	locks sync.Map // map[string]*lockEntry
}

type lockEntry struct {
	mu         sync.Mutex
	lastAccess atomic.Int64 // unix timestamp (seconds)
}

// NewSessionLock creates a new session lock manager.
func NewSessionLock() *SessionLock {
	return &SessionLock{}
}

// Acquire locks the given session for exclusive access.
// Returns an unlock function that MUST be called (typically via defer).
//
// Usage:
//
//	unlock := lock.Acquire(sessionID)
//	defer unlock()
func (sl *SessionLock) Acquire(sessionID string) func() {
	val, _ := sl.locks.LoadOrStore(sessionID, &lockEntry{})
	entry := val.(*lockEntry)
	entry.lastAccess.Store(time.Now().Unix())
	entry.mu.Lock()

	return sync.OnceFunc(func() {
		entry.lastAccess.Store(time.Now().Unix())
		entry.mu.Unlock()
	})
}

// Cleanup removes lock entries that haven't been accessed within maxAge.
// Call this periodically (e.g., every 10 minutes) from the engine or session manager.
func (sl *SessionLock) Cleanup(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge).Unix()
	removed := 0
	sl.locks.Range(func(key, value any) bool {
		entry := value.(*lockEntry)
		if entry.lastAccess.Load() < cutoff {
			// Only delete if we can confirm it's not currently locked
			// by successfully acquiring and immediately releasing the lock
			if entry.mu.TryLock() {
				entry.mu.Unlock()
				sl.locks.Delete(key)
				removed++
			}
		}
		return true
	})
	return removed
}
