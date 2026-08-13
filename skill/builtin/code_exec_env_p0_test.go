package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type inheritingProcessSandbox struct{}

func (inheritingProcessSandbox) Close() error {
	return nil
}

func (inheritingProcessSandbox) Exec(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append([]string(nil), command.Env...)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return &sandbox.ExecResult{Stdout: string(out), ExitCode: exitCode}, err
}

func TestCodeExecSecurityP0_PosixPayloadGetsCleanEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX structured command contract")
	}
	t.Setenv("HEXCLAW_TEST_HOST_SECRET", "must-not-leak")

	result, err := runPosixSandboxCommandInDir(
		context.Background(),
		inheritingProcessSandbox{},
		t.TempDir(),
		[]string{"sh", "-c", `printf 'secret=%s;run=%s;goenv=%s;pwd=%s;path=%s' "${HEXCLAW_TEST_HOST_SECRET-}" "${HEXCLAW_RUN_ID-}" "${GOENV-}" "${PWD-}" "${PATH-}"`},
		map[string]string{"HEXCLAW_RUN_ID": "run_clean_env"},
	)
	if err != nil {
		t.Fatalf("run structured command: %v; output=%q", err, result.Stdout)
	}
	if strings.Contains(result.Stdout, "must-not-leak") {
		t.Fatalf("host credential environment leaked into payload: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "secret=;run=run_clean_env;goenv=off;pwd=") {
		t.Fatalf("payload did not receive the expected explicit environment: %q", result.Stdout)
	}
	if strings.HasSuffix(result.Stdout, "path=") {
		t.Fatalf("clean environment must retain a controlled runtime PATH: %q", result.Stdout)
	}
}

func TestCodeExecSecurityP0_DisablesGoTelemetryInsideIsolatedHome(t *testing.T) {
	home := t.TempDir()
	exports := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
	}
	if err := ensureCodeExecGoTelemetryOff(exports); err != nil {
		t.Fatal(err)
	}
	var configRoot string
	switch runtime.GOOS {
	case "darwin":
		configRoot = filepath.Join(home, "Library", "Application Support")
	case "windows":
		configRoot = exports["APPDATA"]
	default:
		configRoot = exports["XDG_CONFIG_HOME"]
	}
	mode, err := os.ReadFile(filepath.Join(configRoot, "go", "telemetry", "mode"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(mode), "off ") {
		t.Fatalf("Go telemetry mode = %q, want off with effective date", mode)
	}
}
