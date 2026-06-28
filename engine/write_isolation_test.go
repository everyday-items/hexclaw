package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNoopIsolator(t *testing.T) {
	ws, err := NoopIsolator{Dir: "/tmp/x"}.Acquire(context.Background(), "a")
	if err != nil || ws.Path() != "/tmp/x" {
		t.Fatalf("noop 应返回原目录: %v %q", err, ws.Path())
	}
	if err := ws.Release(); err != nil {
		t.Fatalf("noop release 应 no-op: %v", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitWorktreeIsolator(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用，跳过")
	}
	ctx := context.Background()
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "t@t.dev")
	mustGit(t, repo, "config", "user.name", "tester")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "init")

	// 非 git 目录应报错
	if _, err := NewGitWorktreeIsolator(t.TempDir(), "", ""); err == nil {
		t.Fatal("非 git 目录应报错")
	}

	iso, err := NewGitWorktreeIsolator(repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ws1, err := iso.Acquire(ctx, "maker-a")
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := iso.Acquire(ctx, "maker-b")
	if err != nil {
		t.Fatal(err)
	}
	if ws1.Dir == ws2.Dir {
		t.Fatal("两个工作区应是不同目录")
	}
	// 基线文件应在每个工作区里
	for _, ws := range []*WriteWorkspace{ws1, ws2} {
		if _, err := os.Stat(filepath.Join(ws.Dir, "base.txt")); err != nil {
			t.Fatalf("工作区缺基线文件: %v", err)
		}
	}
	// 在 ws1 写文件，不应污染 ws2 或主仓（硬隔离）
	if err := os.WriteFile(filepath.Join(ws1.Dir, "only-in-1.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws2.Dir, "only-in-1.txt")); !os.IsNotExist(err) {
		t.Fatal("ws1 的写不应出现在 ws2")
	}
	if _, err := os.Stat(filepath.Join(repo, "only-in-1.txt")); !os.IsNotExist(err) {
		t.Fatal("ws1 的写不应出现在主仓")
	}
	// 回收
	if err := ws1.Release(); err != nil {
		t.Fatalf("release ws1: %v", err)
	}
	if err := ws2.Release(); err != nil {
		t.Fatalf("release ws2: %v", err)
	}
	if _, err := os.Stat(ws1.Dir); !os.IsNotExist(err) {
		t.Fatal("release 后工作区目录应被移除")
	}
	// Release 幂等
	if err := ws1.Release(); err != nil {
		t.Fatalf("重复 release 应 no-op: %v", err)
	}
}
