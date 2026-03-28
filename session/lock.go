package session

import (
	"sync"
)

// SessionLock serializes concurrent requests per session.
//
// Design: sync.Map<sessionID, *lockEntry>.
// Entries are never deleted — each is a single Mutex (tiny).
// Aligns with OpenClaw Session Lane pattern.
type SessionLock struct {
	locks sync.Map // map[string]*lockEntry
}

type lockEntry struct {
	mu sync.Mutex
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
	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()
	}
}
