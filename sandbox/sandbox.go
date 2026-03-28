// Package sandbox 提供跨平台进程沙箱
//
// 三平台隔离策略:
//   - macOS: Seatbelt (sandbox-exec + SBPL 策略)
//   - Linux: Namespace + seccomp + pivot_root (Phase 7 D18-D19)
//   - Windows: Restricted Token + ACL + Job Object (Phase 8 D29-D35)
//
// 默认 ON，零外部依赖，对齐 Codex 沙箱能力。
package sandbox

import (
	"context"
	"fmt"
)

// Config 沙箱配置
type Config struct {
	Workspace   string   `yaml:"workspace"`    // 工作区目录 (可读写)
	Timeout     int      `yaml:"timeout"`      // 超时秒数，默认 60
	DeniedPaths []string `yaml:"denied_paths"` // 禁止访问的路径
	Network     bool     `yaml:"network"`      // 是否允许网络，默认 false
}

// ExecResult 沙箱执行结果
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox 沙箱接口
type Sandbox interface {
	// Exec 在沙箱内执行命令
	Exec(ctx context.Context, command string, args []string) (*ExecResult, error)

	// ExecCode 在沙箱内执行代码 (language: python/javascript/go)
	ExecCode(ctx context.Context, language, code string) (*ExecResult, error)
}

// New 创建当前平台的沙箱实例
func New(cfg Config) (Sandbox, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("sandbox workspace is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60
	}

	return newPlatformSandbox(cfg)
}
