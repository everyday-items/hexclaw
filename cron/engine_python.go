package cron

import (
	"context"
	"os/exec"
)

// ScriptExecutor satisfies ScriptEngine as the python3 engine: an optional,
// subprocess-based runtime present only on hosts that ship python3. The pure-Go
// StarlarkEngine is the default; this engine keeps legacy python3 specs working
// where the interpreter exists.

func (e *ScriptExecutor) Name() string { return RuntimePython3 }

// Available reports whether python3 is on PATH. When it is not, the scheduler
// surfaces an explicit error instead of letting exec fail silently.
func (e *ScriptExecutor) Available() bool {
	_, err := exec.LookPath(e.pythonBin)
	return err == nil
}

// Validate runs the full python validation chain (syntax + AST allowlist +
// output contract) used at job admission.
func (e *ScriptExecutor) Validate(script string) error {
	if err := validatePythonSyntax(script); err != nil {
		return err
	}
	if err := validateNoForbiddenImports(script); err != nil {
		return err
	}
	return validateOutputContract(script)
}

// Execute runs the python script (delegates to Run).
func (e *ScriptExecutor) Execute(ctx context.Context, spec *JobSpec) (*RunResult, error) {
	return e.Run(ctx, spec)
}
