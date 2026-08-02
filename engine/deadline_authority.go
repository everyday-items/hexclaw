package engine

import (
	"context"
	"time"
)

// authoritativeCallerDeadlineContextKey marks an explicitly selected caller
// deadline as the only timeout authority for nested engine work. It is not
// inferred from every context deadline: ordinary callers must keep the engine's
// bounded anti-hang guards.
type authoritativeCallerDeadlineContextKey struct{}

// WithAuthoritativeCallerDeadline preserves a caller's existing deadline
// through nested SolveSkill/sub-agent contexts. It has effect only when the
// marked context actually has a deadline; otherwise normal engine fallbacks
// remain in force.
func WithAuthoritativeCallerDeadline(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authoritativeCallerDeadlineContextKey{}, true)
}

// HasAuthoritativeCallerDeadline reports whether a caller explicitly selected
// its deadline as the authority for nested engine work.
func HasAuthoritativeCallerDeadline(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(authoritativeCallerDeadlineContextKey{}).(bool)
	return marked
}

// newSubAgentAttemptContext keeps the caller's actual deadline only for an
// explicit authority marker. Every other caller keeps the existing finite
// timeout guard. WithCancel is intentionally used in the authority branch so
// callers can release resources early without creating a second deadline.
func newSubAgentAttemptContext(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if HasAuthoritativeCallerDeadline(ctx) {
		if _, ok := ctx.Deadline(); ok {
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, fallback)
}
