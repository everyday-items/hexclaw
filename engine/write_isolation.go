// write_isolation.go 实现 Loop Engineering #7：**并行写隔离**。
//
// orchestrate 扇出的并行子 Agent 若各自改同一文件/工作区，会互相踩踏——这正是 K8s 早就
// 踩过的 reconcile storm。Loop 工程的解法是 git worktree：每个并行单元一份独立 checkout，
// 硬隔离写。本文件提供这层原语：WriteIsolator.Acquire 给一个并行单元开一个隔离工作区，
// 用完 Release 回收。不写文件的并行单元用 NoopIsolator（零开销）。
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"
)

// WriteWorkspace 是一个隔离的可写工作区。
type WriteWorkspace struct {
	Dir     string
	release func() error
}

// Path 返回工作区目录（nil-safe）。
func (w *WriteWorkspace) Path() string {
	if w == nil {
		return ""
	}
	return w.Dir
}

// Release 回收工作区（幂等、nil-safe）。回收失败时保留 release 供重试——不能提前清空
// 变 no-op，否则一次瞬时失败（worktree 占用/git 锁）就让目录 + git 元数据永久泄漏（#9）。
func (w *WriteWorkspace) Release() error {
	if w == nil || w.release == nil {
		return nil
	}
	if err := w.release(); err != nil {
		return err
	}
	w.release = nil
	return nil
}

// WriteIsolator 为并行单元分配隔离工作区。
type WriteIsolator interface {
	Acquire(ctx context.Context, label string) (*WriteWorkspace, error)
}

// NoopIsolator 不隔离：所有单元共用 Dir，Release 无操作。用于不写文件的并行单元。
type NoopIsolator struct{ Dir string }

// Acquire 实现 WriteIsolator。
func (n NoopIsolator) Acquire(_ context.Context, _ string) (*WriteWorkspace, error) {
	return &WriteWorkspace{Dir: n.Dir}, nil
}

// GitWorktreeIsolator 用 git worktree 做硬写隔离：每个并行单元一个独立 detached checkout，
// 基于 baseRef；Release 时 `git worktree remove --force` 回收。
type GitWorktreeIsolator struct {
	repoDir string // 主仓目录（必须是 git 仓库）
	baseRef string // 基线 ref（空 = HEAD）
	root    string // worktree 存放根（空 = repoDir/.hexclaw-worktrees）
}

// NewGitWorktreeIsolator 校验 repoDir 是 git 仓库后构造隔离器。
func NewGitWorktreeIsolator(repoDir, baseRef, root string) (*GitWorktreeIsolator, error) {
	if !isGitRepo(repoDir) {
		return nil, fmt.Errorf("%s 不是 git 仓库（worktree 写隔离需要）", repoDir)
	}
	if strings.TrimSpace(baseRef) == "" {
		baseRef = "HEAD"
	}
	if strings.TrimSpace(root) == "" {
		root = filepath.Join(repoDir, ".hexclaw-worktrees")
	}
	return &GitWorktreeIsolator{repoDir: repoDir, baseRef: baseRef, root: root}, nil
}

// Acquire 实现 WriteIsolator：git worktree add --detach <root>/<label-id> <baseRef>。
func (g *GitWorktreeIsolator) Acquire(ctx context.Context, label string) (*WriteWorkspace, error) {
	if err := os.MkdirAll(g.root, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Join(g.root, fileSafe(label)+"-"+idgen.NanoID())
	if out, err := runGit(ctx, g.repoDir, "worktree", "add", "--detach", dir, g.baseRef); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(out))
	}
	repo := g.repoDir
	return &WriteWorkspace{
		Dir: dir,
		release: func() error {
			_, err := runGit(context.Background(), repo, "worktree", "remove", "--force", dir)
			return err
		},
	}, nil
}

func isGitRepo(dir string) bool {
	out, err := runGit(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
