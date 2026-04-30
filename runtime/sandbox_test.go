package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagRuntimeSandboxV1: on})
	return featureflag.WithContext(ctx, flags)
}

func TestProcessSandbox_FlagOff(t *testing.T) {
	ctx := withFlag(context.Background(), false)
	if _, err := NewProcessSandbox().Run(ctx, SandboxSpec{Cmd: "echo"}); !errors.Is(err, ErrSandboxDisabled) {
		t.Errorf("flag OFF 应返回 ErrSandboxDisabled；got %v", err)
	}
}

func TestProcessSandbox_FlagOnHappy(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	res, err := NewProcessSandbox().Run(ctx, SandboxSpec{Cmd: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout == "" {
		t.Error("stdout 应有占位输出")
	}
}

func TestProcessSandbox_EmptyCmd(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	if _, err := NewProcessSandbox().Run(ctx, SandboxSpec{}); err == nil {
		t.Error("空 cmd 应报错")
	}
}

func TestDockerSandbox_NotImplemented(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	if _, err := NewDockerSandbox().Run(ctx, SandboxSpec{Cmd: "x"}); err == nil {
		t.Error("docker 占位应返回 not implemented")
	}
}

func TestCheckpoint_BeginRollbackRestoresInline(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("v1"), 0o644)

	store := NewCheckpointStore()
	cpID, err := store.Begin(ctx, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	// 修改文件
	os.WriteFile(path, []byte("v2-modified"), 0o644)

	// rollback
	if err := store.Rollback(ctx, cpID); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v1" {
		t.Errorf("应恢复 v1；got %q", got)
	}
}

func TestCheckpoint_RollbackDeletesNewFile(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	// Begin 时不存在
	store := NewCheckpointStore()
	cpID, _ := store.Begin(ctx, []string{path})

	// 之后创建文件
	os.WriteFile(path, []byte("created"), 0o644)
	if err := store.Rollback(ctx, cpID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("Rollback 应删除 Begin 时不存在的文件")
	}
}

func TestCheckpoint_FlagOff(t *testing.T) {
	ctx := withFlag(context.Background(), false)
	store := NewCheckpointStore()
	if _, err := store.Begin(ctx, nil); !errors.Is(err, ErrSandboxDisabled) {
		t.Error("flag OFF 应返回 ErrSandboxDisabled")
	}
}

func TestCheckpoint_RollbackUnknownID(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	store := NewCheckpointStore()
	if err := store.Rollback(ctx, "nope"); err == nil {
		t.Error("不存在 ID 应报错")
	}
}

func TestResolvePathInWorkspace_RejectsEscape(t *testing.T) {
	ws := t.TempDir()
	if _, err := ResolvePathInWorkspace(ws, "/etc/passwd"); err == nil {
		t.Error("逃出 workspace 应被拒")
	}
	if _, err := ResolvePathInWorkspace(ws, filepath.Join(ws, "ok.txt")); err != nil {
		t.Errorf("workspace 内应通过；got %v", err)
	}
}

func TestSandboxKinds(t *testing.T) {
	if NewProcessSandbox().Kind() != KindProcess {
		t.Error("kind err")
	}
	if NewDockerSandbox().Kind() != KindDocker {
		t.Error("kind err")
	}
}
