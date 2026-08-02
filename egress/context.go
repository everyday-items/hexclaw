package egress

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}
type providerClientRequestKeyContextKey struct{}
type providerRequestResponseHeaderTimeoutContextKey struct{}

// WithProviderClientRequestKey binds a durable upstream request identity to
// this request only. Provider transports may map it to an idempotency contract;
// an upstream that ignores that contract remains at-least-once.
func WithProviderClientRequestKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, providerClientRequestKeyContextKey{}, key)
}

// ProviderClientRequestKeyFromContext returns the request-local durable
// identity, if one was explicitly bound by the caller.
func ProviderClientRequestKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	key, ok := ctx.Value(providerClientRequestKeyContextKey{}).(string)
	key = strings.TrimSpace(key)
	return key, ok && key != ""
}

// WithProviderRequestResponseHeaderTimeout binds a request-local upper bound
// for the provider transport's response-header guard. A request deadline always
// wins, and an already-bound stricter value is retained. This never changes a
// shared client's transport configuration.
func WithProviderRequestResponseHeaderTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return ctx
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx
		}
		if timeout > remaining {
			timeout = remaining
		}
	}
	if existing, ok := ProviderRequestResponseHeaderTimeoutFromContext(ctx); ok && existing < timeout {
		timeout = existing
	}
	return context.WithValue(ctx, providerRequestResponseHeaderTimeoutContextKey{}, timeout)
}

// ProviderRequestResponseHeaderTimeoutFromContext returns the request-local
// response-header budget, if one was explicitly bound by the caller.
func ProviderRequestResponseHeaderTimeoutFromContext(ctx context.Context) (time.Duration, bool) {
	if ctx == nil {
		return 0, false
	}
	timeout, ok := ctx.Value(providerRequestResponseHeaderTimeoutContextKey{}).(time.Duration)
	return timeout, ok && timeout > 0
}

// requestEnvelope is mutable on purpose: a request acquires data classes while
// the engine assembles prompt context (attachments, memory, records). The
// envelope is request-scoped and concurrency-safe so helper goroutines can only
// make the final decision more restrictive, never silently remove a class.
type requestEnvelope struct {
	mu      sync.RWMutex
	purpose Purpose
	auditID string
	classes map[DataClass]struct{}
}

// WithRequest starts an isolated egress envelope. It intentionally does not
// inherit a parent's purpose/classes: a solve verifier or RAG sub-operation has
// a different allowlist from its parent general-chat request.
func WithRequest(ctx context.Context, purpose Purpose, auditID string, classes ...DataClass) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	env := &requestEnvelope{
		purpose: purpose,
		auditID: auditID,
		classes: make(map[DataClass]struct{}, len(classes)),
	}
	for _, class := range classes {
		env.classes[class] = struct{}{}
	}
	return context.WithValue(ctx, contextKey{}, env)
}

// AddDataClasses adds classifications discovered while assembling a request.
// If no envelope exists it creates an intentionally invalid (empty-purpose)
// envelope; the cloud boundary will reject it instead of guessing a purpose.
func AddDataClasses(ctx context.Context, classes ...DataClass) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	env, _ := ctx.Value(contextKey{}).(*requestEnvelope)
	if env == nil {
		env = &requestEnvelope{classes: make(map[DataClass]struct{}, len(classes))}
		ctx = context.WithValue(ctx, contextKey{}, env)
	}
	env.mu.Lock()
	if env.classes == nil {
		env.classes = make(map[DataClass]struct{}, len(classes))
	}
	for _, class := range classes {
		env.classes[class] = struct{}{}
	}
	env.mu.Unlock()
	return ctx
}

// RequestsFromContext returns one policy request per distinct data class. The
// stable ordering makes audits and regression tests deterministic.
func RequestsFromContext(ctx context.Context) ([]Request, bool) {
	if ctx == nil {
		return nil, false
	}
	env, ok := ctx.Value(contextKey{}).(*requestEnvelope)
	if !ok || env == nil {
		return nil, false
	}
	env.mu.RLock()
	purpose, auditID := env.purpose, env.auditID
	classes := make([]DataClass, 0, len(env.classes))
	for class := range env.classes {
		classes = append(classes, class)
	}
	env.mu.RUnlock()
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	requests := make([]Request, 0, len(classes))
	for _, class := range classes {
		requests = append(requests, Request{Purpose: purpose, DataClass: class, AuditID: auditID})
	}
	return requests, true
}

// GuardContext enforces the complete request envelope at a cloud boundary.
// Missing metadata and empty class sets fail closed; callers must classify a
// request explicitly instead of relying on a permissive default.
func (p *Policy) GuardContext(ctx context.Context) error {
	requests, ok := RequestsFromContext(ctx)
	if !ok {
		return fmt.Errorf("egress 拦截: 缺少 purpose/data-class envelope")
	}
	if len(requests) == 0 {
		return fmt.Errorf("egress 拦截: data-class envelope 为空")
	}
	var firstErr error
	for _, req := range requests {
		if err := p.Guard(req); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
