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

// systemDispatchSourceKey carries the dispatch source (e.g. "cron") for
// non-interactive engine runs. The engine stamps it at Process entry for
// system dispatches (cron/heartbeat/webhook/spawn). Permission gating reads it
// to auto-approve pre-authorized scheduled tools, and skills read it to adapt
// behavior — e.g. knowledge_ingest snapshot-names a cron write so each run
// creates a new document instead of overwriting the previous one.
type systemDispatchSourceKey struct{}

// WithSystemDispatchSource returns a context carrying the non-interactive
// dispatch source. An empty source returns ctx unchanged (an interactive run
// has no source to stamp).
func WithSystemDispatchSource(ctx context.Context, source string) context.Context {
	if source == "" {
		return ctx
	}
	return context.WithValue(ctx, systemDispatchSourceKey{}, source)
}

// SystemDispatchSource returns the stamped dispatch source, or "" for
// interactive runs (and direct skill invocation in tests).
func SystemDispatchSource(ctx context.Context) string {
	s, _ := ctx.Value(systemDispatchSourceKey{}).(string)
	return s
}

// systemDispatchTaskKey carries the stable task reference of a system dispatch
// ("cron:<jobID>" / "webhook:<id>" / "workflow:<id>"). The engine stamps it at
// Process/ProcessStream entry alongside the dispatch source. The permission
// gate reads it to evaluate task-scoped grants and to attribute persisted
// permission decisions to the task that triggered the run.
type systemDispatchTaskKey struct{}

// WithSystemDispatchTask returns a context carrying the task reference of the
// current system dispatch. An empty ref returns ctx unchanged.
func WithSystemDispatchTask(ctx context.Context, taskRef string) context.Context {
	if taskRef == "" {
		return ctx
	}
	return context.WithValue(ctx, systemDispatchTaskKey{}, taskRef)
}

// SystemDispatchTaskRef returns the stamped task reference, or "" when the
// dispatch carries no task identity (heartbeat, interactive runs, tests).
func SystemDispatchTaskRef(ctx context.Context) string {
	s, _ := ctx.Value(systemDispatchTaskKey{}).(string)
	return s
}

// snapshotBaseTitleKey carries a stable base title for scheduled knowledge-base
// writes. The cron dispatcher stamps the job's name so every run of a job
// ingests under one coherent snapshot series, even when the model varies the
// title it passes to knowledge_ingest.
type snapshotBaseTitleKey struct{}

// WithSnapshotBaseTitle returns a context carrying the stable snapshot base
// title. An empty title returns ctx unchanged.
func WithSnapshotBaseTitle(ctx context.Context, title string) context.Context {
	if title == "" {
		return ctx
	}
	return context.WithValue(ctx, snapshotBaseTitleKey{}, title)
}

// SnapshotBaseTitle returns the stable snapshot base title, or "" when none was
// stamped (interactive runs, or a cron job with no name).
func SnapshotBaseTitle(ctx context.Context) string {
	s, _ := ctx.Value(snapshotBaseTitleKey{}).(string)
	return s
}
