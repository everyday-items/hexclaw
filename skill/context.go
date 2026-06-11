package skill

import "context"

// authUserCtxKey is the context key carrying the authenticated user ID of the
// message that triggered the current tool execution. The engine stamps it at
// Process/ProcessStream entry so skills can trust it over LLM-supplied
// arguments (LLM args are model-controlled and forgeable; see BUG-20260611 M7).
type authUserCtxKey struct{}

// WithAuthenticatedUser returns a context carrying the authenticated user ID.
// An empty id returns ctx unchanged.
func WithAuthenticatedUser(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, authUserCtxKey{}, id)
}

// AuthenticatedUserID returns the authenticated user ID stamped by the
// engine, or "" when the execution context has none (e.g. direct skill
// invocation in tests).
func AuthenticatedUserID(ctx context.Context) string {
	id, _ := ctx.Value(authUserCtxKey{}).(string)
	return id
}
