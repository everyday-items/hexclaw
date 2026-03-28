//go:build darwin

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

// darwinSandbox macOS Seatbelt 沙箱
//
// 使用 sandbox-exec + SBPL (Sandbox Profile Language) 限制进程。
// 与 Codex 和 Claude Code 使用完全相同的技术。
type darwinSandbox struct {
	cfg Config
}

func newPlatformSandbox(cfg Config) (Sandbox, error) {
	return newDarwinSandbox(cfg), nil
}

func newDarwinSandbox(cfg Config) *darwinSandbox {
	return &darwinSandbox{cfg: cfg}
}

// generateSBPL 生成 Seatbelt Profile Language 策略
func (s *darwinSandbox) generateSBPL() string {
	workspace := s.cfg.Workspace

	var sb strings.Builder
	sb.WriteString("(version 1)\n")
	sb.WriteString("(deny default)\n")

	// 允许进程执行基本操作
	sb.WriteString("(allow process-exec)\n")
	sb.WriteString("(allow process-fork)\n")
	sb.WriteString("(allow sysctl-read)\n")
	sb.WriteString("(allow mach-lookup)\n")
	sb.WriteString("(allow signal)\n")
	sb.WriteString("(allow system-socket)\n")

	// 允许读取系统文件 (运行时、库等)
	sb.WriteString("(allow file-read*\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/bin\")\n")
	sb.WriteString("  (subpath \"/sbin\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/private/var\")\n")
	sb.WriteString("  (subpath \"/private/tmp\")\n")
	sb.WriteString("  (subpath \"/var\")\n")
	sb.WriteString("  (subpath \"/tmp\")\n")
	sb.WriteString("  (subpath \"/etc\")\n")
	sb.WriteString("  (subpath \"/dev\")\n")
	sb.WriteString("  (subpath \"/opt\")\n")

	// Homebrew paths
	sb.WriteString("  (subpath \"/opt/homebrew\")\n")
	sb.WriteString("  (subpath \"/usr/local\")\n")

	// Python/Node 运行时
	home, _ := os.UserHomeDir()
	sb.WriteString(fmt.Sprintf("  (subpath \"%s/.pyenv\")\n", home))
	sb.WriteString(fmt.Sprintf("  (subpath \"%s/.nvm\")\n", home))
	sb.WriteString(fmt.Sprintf("  (subpath \"%s/.local\")\n", home))
	sb.WriteString(")\n")

	// 工作区读写
	sb.WriteString(fmt.Sprintf("(allow file-read* (subpath \"%s\"))\n", workspace))
	sb.WriteString(fmt.Sprintf("(allow file-write* (subpath \"%s\"))\n", workspace))

	// /tmp 读写 (临时文件)
	sb.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	sb.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")

	// 明确拒绝的路径
	for _, denied := range s.cfg.DeniedPaths {
		expanded := expandPath(denied)
		sb.WriteString(fmt.Sprintf("(deny file-read* (subpath \"%s\"))\n", expanded))
		sb.WriteString(fmt.Sprintf("(deny file-write* (subpath \"%s\"))\n", expanded))
	}

	// 网络控制
	if s.cfg.Network {
		sb.WriteString("(allow network*)\n")
	} else {
		sb.WriteString("(deny network*)\n")
		// 允许本地 DNS 和 loopback (某些运行时需要)
		sb.WriteString("(allow network-outbound (to unix-socket))\n")
	}

	return sb.String()
}

// Exec 在 Seatbelt 沙箱内执行命令
func (s *darwinSandbox) Exec(ctx context.Context, command string, args []string) (*ExecResult, error) {
	sbpl := s.generateSBPL()

	sandboxArgs := []string{"-p", sbpl, command}
	sandboxArgs = append(sandboxArgs, args...)

	cmd := exec.CommandContext(ctx, "sandbox-exec", sandboxArgs...)
	cmd.Dir = s.cfg.Workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 清理危险环境变量
	cmd.Env = cleanEnv(os.Environ())

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("sandbox exec failed: %w", err)
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}

// ExecCode 在沙箱内执行代码
func (s *darwinSandbox) ExecCode(ctx context.Context, language, code string) (*ExecResult, error) {
	// 写代码到临时文件
	var ext, cmd string
	switch language {
	case "python", "python3":
		ext = ".py"
		cmd = "python3"
	case "javascript", "node", "js":
		ext = ".js"
		cmd = "node"
	case "go":
		ext = ".go"
		cmd = "go"
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	tmpFile := filepath.Join(s.cfg.Workspace, "_hexclaw_exec"+ext)
	if err := os.WriteFile(tmpFile, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("write temp code: %w", err)
	}
	defer os.Remove(tmpFile)

	if language == "go" {
		return s.Exec(ctx, cmd, []string{"run", tmpFile})
	}
	return s.Exec(ctx, cmd, []string{tmpFile})
}

// cleanEnv 清理危险环境变量
func cleanEnv(env []string) []string {
	dangerousPrefixes := []string{
		"LD_", "DYLD_", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	}
	var clean []string
	for _, e := range env {
		dangerous := false
		for _, prefix := range dangerousPrefixes {
			if strings.HasPrefix(e, prefix+"=") {
				dangerous = true
				break
			}
		}
		if !dangerous {
			clean = append(clean, e)
		}
	}
	return clean
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
