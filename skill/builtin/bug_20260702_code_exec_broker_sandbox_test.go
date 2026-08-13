package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// bug_20260702：code_exec 的 P0 安全收口。
//
// A) mode=file/project 旁路 FileAccessBroker：任意绝对入口 / 任意存在目录都能读/执行，
//    集中裁决被彻底绕过。修后必须过 broker allow-list（fail-closed）。
// B) 沙箱无默认 secrets DeniedPaths：~/.ssh、keystore 等在沙箱内可读。修后必须默认遮蔽。

// ── A ──────────────────────────────────────────────────────────────────────

// mode=project 请求一个未授权目录必须被拒（旧代码放行 → FAIL）。
func TestBug20260702_ProjectRootRequiresBrokerAuthorization(t *testing.T) {
	authorized := t.TempDir()
	unauthorized := t.TempDir()
	broker := NewFileAccessBroker([]string{authorized})

	s := newTestCodeExecSkill(t)
	s.SetFileAccess(broker)

	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": unauthorized,
		"command":      []any{"sh", "-c", "echo PROJECT_LEAK"},
	})
	if err == nil {
		t.Fatal("SECURITY: mode=project executed an UNAUTHORIZED project_root — FileAccessBroker bypassed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "author") && !strings.Contains(strings.ToLower(err.Error()), "denied") {
		t.Fatalf("expected authorization-denied error, got: %v", err)
	}
}

// mode=file 请求一个未授权的绝对入口必须被拒（旧代码 os.ReadFile 直读 → FAIL）。
func TestBug20260702_FileEntrypointRequiresBrokerAuthorization(t *testing.T) {
	authorized := t.TempDir()
	broker := NewFileAccessBroker([]string{authorized})

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.py")
	if err := os.WriteFile(secret, []byte("print('SECRET_LEAK')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestCodeExecSkill(t)
	s.SetFileAccess(broker)

	res, err := s.Execute(context.Background(), map[string]any{
		"mode":       "file",
		"entrypoint": secret,
	})
	if err == nil {
		t.Fatalf("SECURITY: mode=file read+executed an UNAUTHORIZED absolute entrypoint — FileAccessBroker bypassed\noutput:\n%s", res.Content)
	}
}

// 空 broker 允许集（用户未授权任何目录）时，project_root=$HOME 必被拒（fail-closed）。
func TestBug20260702_EmptyBrokerAllowSetDeniesHome(t *testing.T) {
	broker := NewFileAccessBroker(nil)
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("no home dir")
	}
	s := newTestCodeExecSkill(t)
	s.SetFileAccess(broker)

	_, err = s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": home,
		"command":      []any{"sh", "-c", "echo HOME_LEAK"},
	})
	if err == nil {
		t.Fatal("SECURITY: empty broker allow-set still permitted project_root=$HOME")
	}
}

// 正向防误伤：已授权目录仍可正常执行。
func TestBug20260702_AuthorizedProjectRootStillExecutes(t *testing.T) {
	authorized := t.TempDir()
	broker := NewFileAccessBroker([]string{authorized})
	s := newTestCodeExecSkill(t)
	s.SetFileAccess(broker)

	res, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": authorized,
		"command":      []any{"sh", "-c", "echo AUTHORIZED_OK"},
	})
	if err != nil {
		t.Fatalf("authorized project_root must still run: %v", err)
	}
	if !strings.Contains(res.Content, "AUTHORIZED_OK") {
		t.Fatalf("authorized run output missing marker:\n%s", res.Content)
	}
}

// ── B ──────────────────────────────────────────────────────────────────────

// 默认 DeniedPaths 必须遮蔽关键 secrets 路径（旧代码为空 → FAIL）。
func TestBug20260702_DefaultSandboxDeniedPathsCoverSecrets(t *testing.T) {
	// toolkit v0.3.0 的 DeniedPaths 必须真实存在（fail-closed），默认列表只保留
	// 本机存在的凭据路径；用 fake home 构造齐全的目录再断言覆盖完整性。
	fakeHome := t.TempDir()
	for _, p := range []string{".ssh", ".aws", ".config/gcloud", ".gnupg"} {
		if err := os.MkdirAll(filepath.Join(fakeHome, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// master.key 是文件而非目录。
	if err := os.MkdirAll(filepath.Join(fakeHome, ".hexclaw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".hexclaw", "master.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	cfg := ensureCodeExecConfigDefaults(sandbox.Config{Workspace: t.TempDir()})
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("no home dir")
	}
	want := []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "gcloud"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".hexclaw", "master.key"),
	}
	for _, w := range want {
		if !slices.Contains(cfg.DeniedPaths, w) {
			t.Fatalf("default DeniedPaths missing secret path %q; got %v", w, cfg.DeniedPaths)
		}
	}
}

// 行为门（darwin/linux 真沙箱）：project 模式即便把 $HOME 当 workspace，也不得读到 ~/.ssh。
func TestBug20260702_SandboxDeniesSecretRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix sandbox behavior test")
	}
	// macOS 的 t.TempDir() 位于 /var/folders → /private/var 符号链接下；真沙箱按解析后
	// 的真实路径裁决，生产环境 $HOME(/Users/xxx) 非符号链接。解析一次，消除测试假象。
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("TOPSECRET_PRIVATE_KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	broker := NewFileAccessBroker([]string{home})
	s := newTestCodeExecSkill(t)
	s.SetFileAccess(broker)

	code := "import pathlib\n" +
		"try:\n" +
		"    print('READ ' + pathlib.Path(" + pyStr(secret) + ").read_text())\n" +
		"except Exception as e:\n" +
		"    print('DENIED', type(e).__name__)\n"
	res, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": home,
		"command":      []any{"python3", "-c", code},
	})
	// 唯一失败条件：密钥泄漏。沙箱拒绝（err 或 DENIED）都算安全。
	if err == nil && strings.Contains(res.Content, "TOPSECRET_PRIVATE_KEY") {
		t.Fatalf("SECURITY: sandbox leaked ~/.ssh/id_rsa despite DeniedPaths:\n%s", res.Content)
	}
}
