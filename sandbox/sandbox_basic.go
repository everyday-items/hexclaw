//go:build !darwin && !linux && !windows

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// basicSandbox 基础沙箱 (无 OS 隔离，仅路径限制 + 超时)
//
// 用于 Windows (Phase 8 前) 和不支持沙箱的平台。
type basicSandbox struct {
	cfg Config
}

func newPlatformSandbox(cfg Config) (Sandbox, error) {
	return &basicSandbox{cfg: cfg}, nil
}

func newBasicSandbox(cfg Config) *basicSandbox {
	return &basicSandbox{cfg: cfg}
}

func (s *basicSandbox) Exec(ctx context.Context, command string, args []string) (*ExecResult, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = s.cfg.Workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("exec failed: %w", err)
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}

func (s *basicSandbox) ExecCode(ctx context.Context, language, code string) (*ExecResult, error) {
	var ext, interpreter string
	switch language {
	case "python", "python3":
		ext = ".py"
		interpreter = "python3"
	case "javascript", "node", "js":
		ext = ".js"
		interpreter = "node"
	case "go":
		ext = ".go"
		interpreter = "go"
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	tmpFile := filepath.Join(s.cfg.Workspace, "_hexclaw_exec"+ext)
	if err := os.WriteFile(tmpFile, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("write temp code: %w", err)
	}
	defer os.Remove(tmpFile)

	if language == "go" {
		return s.Exec(ctx, interpreter, []string{"run", tmpFile})
	}
	return s.Exec(ctx, interpreter, []string{tmpFile})
}
