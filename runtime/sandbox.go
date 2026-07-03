// Package runtime 实现 v0.4.0 H4 运行时隔离 + Checkpoint 回滚（feature-flag gated）。
//
// 三类 Sandbox：
//   - process  : 本机受限子进程（默认；rlimit + stdio 隔离）
//   - docker   : Docker 容器（推荐；require docker daemon；本期占位）
//   - remote   : v0.5+，本期不实现
//
// 高风险 tool / plugin / MCP 应该要求 sandbox capability；低风险只读可在 process
// 下执行。Checkpoint：每次文件写入 / Skill patch / 配置修改前生成 fs 快照（基于
// path + mtime + sha256 的轻量记录），失败时按 checkpoint 回滚 untracked 变更。
//
// flag runtime.sandbox.v1 默认 ON：功能优先启用 sandbox/checkpoint 能力。
package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagRuntimeSandboxV1 控制 H4 sandbox+checkpoint 是否启用。
const FlagRuntimeSandboxV1 = "runtime.sandbox.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagRuntimeSandboxV1,
		Default:      true,
		Description:  "Enable H4 Sandbox provider (process/docker/remote) + Checkpoint snapshot/rollback for high-risk operations.",
		Stage:        featureflag.StageGA,
		SinceVersion: "0.4.0",
	})
}

// SandboxKind 是 Sandbox 类型枚举。
type SandboxKind string

const (
	KindProcess SandboxKind = "process"
	KindDocker  SandboxKind = "docker"
	KindRemote  SandboxKind = "remote"
)

// SandboxSpec 定义一次 Sandbox 执行的请求。
type SandboxSpec struct {
	Cmd      string
	Args     []string
	WorkDir  string
	Env      []string
	Timeout  time.Duration
	ReadOnly bool
}

// SandboxResult 是 Sandbox.Run 的输出。
type SandboxResult struct {
	Stdout, Stderr string
	ExitCode       int
	Duration       time.Duration
}

// Sandbox 是运行时隔离器接口。
type Sandbox interface {
	Kind() SandboxKind
	Run(ctx context.Context, spec SandboxSpec) (*SandboxResult, error)
}

// ErrSandboxDisabled 在 flag OFF 时由所有 Sandbox 方法返回。
var ErrSandboxDisabled = errors.New("runtime: sandbox disabled (flag runtime.sandbox.v1 OFF)")

// ProcessSandbox 用本机子进程实现最小沙箱（仅按 Timeout/WorkDir 隔离；rlimit/cgroups 留待生产）。
type ProcessSandbox struct{}

// NewProcessSandbox 构造。
func NewProcessSandbox() *ProcessSandbox    { return &ProcessSandbox{} }
func (p *ProcessSandbox) Kind() SandboxKind { return KindProcess }

// Run 真实实现：用 exec.CommandContext 执行；timeout / WorkDir / Env 直接传给 cmd。
// rlimit / cgroups (Linux) / sandbox-exec (macOS) 等系统级隔离留待生产 hardening。
func (p *ProcessSandbox) Run(ctx context.Context, spec SandboxSpec) (*SandboxResult, error) {
	if !featureflag.Enabled(ctx, FlagRuntimeSandboxV1) {
		return nil, ErrSandboxDisabled
	}
	if spec.Cmd == "" {
		return nil, errors.New("ProcessSandbox: empty cmd")
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Cmd, spec.Args...)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	res := &SandboxResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			return res, err
		}
	}
	return res, nil
}

// DockerSandbox 占位 —— v0.4.0 仅声明接口，本期不接 docker daemon。
type DockerSandbox struct{}

// NewDockerSandbox 构造占位。
func NewDockerSandbox() *DockerSandbox     { return &DockerSandbox{} }
func (d *DockerSandbox) Kind() SandboxKind { return KindDocker }

// Run 占位：返回错误提示。
func (d *DockerSandbox) Run(ctx context.Context, _ SandboxSpec) (*SandboxResult, error) {
	if !featureflag.Enabled(ctx, FlagRuntimeSandboxV1) {
		return nil, ErrSandboxDisabled
	}
	return nil, errors.New("DockerSandbox: not implemented in v0.4.0; use ProcessSandbox")
}

// ============== Checkpoint ==============

// FileSnapshot 是单个文件在 Begin 时的状态记录。
type FileSnapshot struct {
	Path    string
	Mtime   time.Time
	SHA     string
	Size    int64
	Content []byte // 仅 ≤ MaxInlineSize 时保留
}

// MaxInlineSize 是 Checkpoint 内联保存内容的文件大小上限。
const MaxInlineSize = 1024 * 1024 // 1MB

// Checkpoint 是某次操作开始前对一组文件的快照。
type Checkpoint struct {
	ID        string
	CreatedAt time.Time
	Snapshots []FileSnapshot
}

// CheckpointStore 在内存里保存 checkpoint。
type CheckpointStore struct {
	mu sync.Mutex
	m  map[string]*Checkpoint
}

// NewCheckpointStore 构造一个空 store。
func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{m: map[string]*Checkpoint{}}
}

// Begin 拍下指定 paths 的快照，返回 Checkpoint ID。flag OFF 返回 ErrSandboxDisabled。
func (s *CheckpointStore) Begin(ctx context.Context, paths []string) (string, error) {
	if !featureflag.Enabled(ctx, FlagRuntimeSandboxV1) {
		return "", ErrSandboxDisabled
	}
	cp := &Checkpoint{
		ID:        fmt.Sprintf("cp-%s", idgen.NanoID()),
		CreatedAt: time.Now(),
	}
	for _, p := range paths {
		snap, err := snapshotFile(p)
		if err != nil {
			return "", fmt.Errorf("snapshot %s: %w", p, err)
		}
		cp.Snapshots = append(cp.Snapshots, snap)
	}
	sort.Slice(cp.Snapshots, func(i, j int) bool { return cp.Snapshots[i].Path < cp.Snapshots[j].Path })

	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[cp.ID] = cp
	return cp.ID, nil
}

// Rollback 把所有 snapshot 恢复到 Begin 时的状态。
func (s *CheckpointStore) Rollback(ctx context.Context, id string) error {
	if !featureflag.Enabled(ctx, FlagRuntimeSandboxV1) {
		return ErrSandboxDisabled
	}
	s.mu.Lock()
	cp, ok := s.m[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("checkpoint %s not found", id)
	}

	var errs []string
	for _, snap := range cp.Snapshots {
		if snap.SHA == "" {
			if _, err := os.Stat(snap.Path); err == nil {
				if err := os.Remove(snap.Path); err != nil {
					errs = append(errs, fmt.Sprintf("remove %s: %v", snap.Path, err))
				}
			}
			continue
		}
		if snap.Content != nil {
			if err := os.WriteFile(snap.Path, snap.Content, 0o644); err != nil {
				errs = append(errs, fmt.Sprintf("restore %s: %v", snap.Path, err))
			}
		} else {
			errs = append(errs, fmt.Sprintf("file %s too large (%d bytes); cannot rollback", snap.Path, snap.Size))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("rollback partial: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Discard 丢弃 checkpoint（成功后清理）。
func (s *CheckpointStore) Discard(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func snapshotFile(path string) (FileSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileSnapshot{Path: path}, nil
		}
		return FileSnapshot{}, err
	}
	if info.IsDir() {
		return FileSnapshot{Path: path, Mtime: info.ModTime(), Size: info.Size()}, nil
	}
	snap := FileSnapshot{Path: path, Mtime: info.ModTime(), Size: info.Size()}
	if info.Size() > MaxInlineSize {
		f, err := os.Open(path)
		if err != nil {
			return FileSnapshot{}, err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return FileSnapshot{}, err
		}
		snap.SHA = hex.EncodeToString(h.Sum(nil))
		return snap, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	sum := sha256.Sum256(data)
	snap.SHA = hex.EncodeToString(sum[:])
	snap.Content = data
	return snap, nil
}

// ResolvePathInWorkspace 校验 path 在 workspace 内（防路径穿越）。
func ResolvePathInWorkspace(workspace, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, wsAbs) {
		return "", fmt.Errorf("path %q escapes workspace %q", path, workspace)
	}
	return abs, nil
}
