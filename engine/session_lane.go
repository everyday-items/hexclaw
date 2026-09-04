package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/session"
)

// LaneKey identifies the concurrency lane for one logical conversation request.
type LaneKey struct {
	SessionID string
	RequestID string
}

// LaneLease represents ownership of a session lane.
type LaneLease interface {
	FencingToken() string
	Release(ctx context.Context) error
}

// SessionLane serializes requests that must not interleave side effects.
type SessionLane interface {
	Acquire(ctx context.Context, key LaneKey) (LaneLease, error)
}

// LocalSessionLane is the desktop/sidecar implementation backed by an in-process lock.
type LocalSessionLane struct {
	lock *session.SessionLock
}

func NewLocalSessionLane(lock *session.SessionLock) *LocalSessionLane {
	return &LocalSessionLane{lock: lock}
}

func (l *LocalSessionLane) Acquire(ctx context.Context, key LaneKey) (LaneLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key.SessionID == "" {
		return nil, fmt.Errorf("session lane: empty session id")
	}
	if l == nil || l.lock == nil {
		return noopLaneLease{}, nil
	}
	unlock := l.lock.Acquire(key.SessionID)
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	token := key.RequestID
	if token == "" {
		token = key.SessionID
	}
	return localLaneLease{token: token, unlock: unlock}, nil
}

type localLaneLease struct {
	token  string
	unlock func()
}

func (l localLaneLease) FencingToken() string { return l.token }

func (l localLaneLease) Release(context.Context) error {
	if l.unlock != nil {
		l.unlock()
	}
	return nil
}

type noopLaneLease struct{}

func (noopLaneLease) FencingToken() string          { return "" }
func (noopLaneLease) Release(context.Context) error { return nil }

func (e *ReActEngine) acquireSessionLane(ctx context.Context, sessionID, requestID string) (func(), error) {
	e.mu.RLock()
	lane := e.sessionLane
	lock := e.sessionLock
	e.mu.RUnlock()

	if lane == nil && lock == nil {
		return nil, nil
	}
	waitStarted := time.Now()
	trace.L(ctx).Info("session lane wait started", "stage", "session_wait", "session_id", sessionID, "request_id", requestID)

	if lane != nil {
		lease, err := lane.Acquire(ctx, LaneKey{SessionID: sessionID, RequestID: requestID})
		if err != nil {
			trace.L(ctx).Warn("session lane wait failed", "stage", "session_wait", "session_id", sessionID, "request_id", requestID, "reason", "acquire_error", "err", err, "elapsed_ms", time.Since(waitStarted).Milliseconds())
			return nil, err
		}
		trace.L(ctx).Info("session lane wait completed", "stage", "session_wait", "session_id", sessionID, "request_id", requestID, "elapsed_ms", time.Since(waitStarted).Milliseconds())
		return func() { _ = lease.Release(context.Background()) }, nil
	}
	unlock := lock.Acquire(sessionID)
	trace.L(ctx).Info("session lane wait completed", "stage", "session_wait", "session_id", sessionID, "request_id", requestID, "elapsed_ms", time.Since(waitStarted).Milliseconds())
	return unlock, nil
}
