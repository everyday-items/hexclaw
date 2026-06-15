package cron

import "context"

// ScriptEngine validates and executes a cron script in a sandbox. Engines are
// interchangeable; the scheduler routes by JobSpec.Runtime.
//
// StarlarkEngine is pure-Go: always available, runs in-process, and is a
// structural sandbox (a script has zero file/network/system capability except
// the host builtins explicitly injected). PythonEngine shells out to a system
// python3 and is therefore optional — present only on hosts that ship python3.
//
// This indirection is what lets the desktop app run deterministic cron scripts
// with no runtime dependency (Starlark default) while still honoring legacy
// python3 specs where the interpreter exists (BUG-20260615 architecture).
type ScriptEngine interface {
	// Name is the runtime identifier matched against JobSpec.Runtime.
	Name() string
	// Available reports whether this engine can run on the current host.
	Available() bool
	// Validate statically checks a script for syntax/safety before admission.
	Validate(script string) error
	// Execute runs the script and returns its structured result.
	Execute(ctx context.Context, spec *JobSpec) (*RunResult, error)
}

// Runtime identifiers routed by the scheduler.
const (
	RuntimeStarlark = "starlark"
	RuntimePython3  = "python3"
)
