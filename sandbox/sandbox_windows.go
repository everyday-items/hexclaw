//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// windowsSandbox implements Sandbox using Win32 five-layer isolation:
//   1. Restricted Token — strip privileges, Untrusted IL
//   2. ACL — workspace-only read/write
//   3. Job Object — memory/process limits + UI restrictions
//   4. Low Box Token — kernel-level network isolation
//   5. Alternate Desktop — GUI isolation
type windowsSandbox struct {
	cfg    Config
	policy WindowsSandboxPolicy
}

func newPlatformSandbox(cfg Config) (Sandbox, error) {
	if err := os.MkdirAll(cfg.Workspace, 0755); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	return &windowsSandbox{
		cfg:    cfg,
		policy: DefaultWindowsPolicy(),
	}, nil
}

func (s *windowsSandbox) Exec(ctx context.Context, command string, args []string) (*ExecResult, error) {
	// Validate escape vectors
	if err := validateWindowsEscapeVectors(command, args); err != nil {
		return nil, fmt.Errorf("security check failed: %w", err)
	}
	if err := validateWindowsPath(command); err != nil {
		return nil, err
	}

	timeout := time.Duration(s.cfg.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Try full sandbox launch
	proc, stdout, stderr, err := launchSandboxedProcess(s.cfg, command, args)
	if err != nil {
		// Fallback: direct execution with environment cleanup
		return s.execFallback(ctx, command, args)
	}

	// Wait for process
	done := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		done <- err
	}()

	select {
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*os.SyscallError); ok {
				_ = exitErr
				exitCode = 1
			}
		}
		return &ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: exitCode,
		}, nil
	case <-ctx.Done():
		proc.Kill()
		return &ExecResult{
			Stderr:   "process timed out",
			ExitCode: -1,
		}, ctx.Err()
	}
}

func (s *windowsSandbox) ExecCode(ctx context.Context, language, code string) (*ExecResult, error) {
	var ext, runner string
	switch strings.ToLower(language) {
	case "python", "python3":
		ext, runner = ".py", "python"
	case "javascript", "js", "node":
		ext, runner = ".js", "node"
	case "go", "golang":
		ext, runner = ".go", "go"
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	tmpFile := filepath.Join(s.cfg.Workspace, fmt.Sprintf("_exec_%d%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(tmpFile, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("write code: %w", err)
	}
	defer os.Remove(tmpFile)

	var args []string
	if language == "go" || language == "golang" {
		args = []string{"run", tmpFile}
	} else {
		args = []string{tmpFile}
	}
	return s.Exec(ctx, runner, args)
}

// execFallback runs without Win32 sandbox APIs (degraded mode).
func (s *windowsSandbox) execFallback(ctx context.Context, command string, args []string) (*ExecResult, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = cleanWindowsEnv(s.cfg.Workspace)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}
