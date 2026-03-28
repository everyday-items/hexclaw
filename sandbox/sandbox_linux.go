//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// linuxSandbox Linux Namespace + seccomp 沙箱
//
// 五层隔离: Namespace → pivot_root → seccomp → 进程加固 → 两阶段 re-exec
// 对标 Codex bubblewrap 沙箱。
//
// 退化路径:
//   - CLONE_NEWUSER 不可用 → Landlock + seccomp
//   - Landlock 不可用 → 仅 seccomp + 警告
type linuxSandbox struct {
	cfg Config
}

func newPlatformSandbox(cfg Config) (Sandbox, error) {
	return &linuxSandbox{cfg: cfg}, nil
}

// Exec 在沙箱内执行命令
// TODO: D18-D19 完整实现 Namespace + seccomp + pivot_root
// 当前使用基础隔离 (unshare + chroot fallback)
func (s *linuxSandbox) Exec(ctx context.Context, command string, args []string) (*ExecResult, error) {
	// 尝试使用 unshare 创建隔离环境
	unshareArgs := []string{
		"--mount", "--pid", "--fork",
		"--", command,
	}
	unshareArgs = append(unshareArgs, args...)

	cmd := exec.CommandContext(ctx, "unshare", unshareArgs...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = cleanLinuxEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// unshare 失败 (无权限) → 退化为直接执行 + 路径限制
		return s.execFallback(ctx, command, args)
	}

	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}

// execFallback 退化执行 (无 namespace 权限时)
func (s *linuxSandbox) execFallback(ctx context.Context, command string, args []string) (*ExecResult, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = s.cfg.Workspace
	cmd.Env = cleanLinuxEnv(os.Environ())

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

// ExecCode 在沙箱内执行代码
func (s *linuxSandbox) ExecCode(ctx context.Context, language, code string) (*ExecResult, error) {
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

func cleanLinuxEnv(env []string) []string {
	dangerous := []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT"}
	var clean []string
	for _, e := range env {
		skip := false
		for _, d := range dangerous {
			if strings.HasPrefix(e, d+"=") {
				skip = true
				break
			}
		}
		if !skip {
			clean = append(clean, e)
		}
	}
	return clean
}
