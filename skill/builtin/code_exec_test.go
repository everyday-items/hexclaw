package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// mockSandbox implements sandbox.Sandbox for testing.
type mockSandbox struct {
	execCodeFn func(ctx context.Context, lang, code string) (*sandbox.ExecResult, error)
	execFn     func(ctx context.Context, cmd string, args []string) (*sandbox.ExecResult, error)
}

func (m *mockSandbox) ExecCode(ctx context.Context, lang, code string) (*sandbox.ExecResult, error) {
	if m.execCodeFn != nil {
		return m.execCodeFn(ctx, lang, code)
	}
	return &sandbox.ExecResult{Stdout: "mock output", ExitCode: 0}, nil
}

func (m *mockSandbox) Exec(ctx context.Context, cmd string, args []string) (*sandbox.ExecResult, error) {
	if m.execFn != nil {
		return m.execFn(ctx, cmd, args)
	}
	return &sandbox.ExecResult{ExitCode: 0}, nil
}

func newTestCodeExecSkill(t *testing.T) *CodeExecSkill {
	t.Helper()
	if codeExecTestNeedsRealSandbox(t.Name()) {
		requireCodeExecSandbox(t)
	}
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: true}
	return NewCodeExecSkill(sb, cfg)
}

var (
	codeExecSandboxProbeOnce sync.Once
	codeExecSandboxProbeErr  error
)

func codeExecTestNeedsRealSandbox(name string) bool {
	return strings.Contains(name, "_Execute_") ||
		strings.Contains(name, "StillExecutes") ||
		strings.Contains(name, "SandboxDeniesSecretRead")
}

func requireCodeExecSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("windows code_exec runtime integration depends on toolkit sandbox toolchain/device access")
	}
	if runtime.GOOS != "linux" || os.Getenv("HEXCLAW_P0_SANDBOX_PROOF") == "1" {
		return
	}
	codeExecSandboxProbeOnce.Do(func() {
		ws, err := os.MkdirTemp("", "hexclaw-code-exec-sandbox-probe-*")
		if err != nil {
			codeExecSandboxProbeErr = err
			return
		}
		defer func() {
			if err := os.RemoveAll(ws); err != nil && codeExecSandboxProbeErr == nil {
				codeExecSandboxProbeErr = err
			}
		}()
		cfg := ensureCodeExecConfigDefaults(sandbox.Config{Workspace: ws, Timeout: 5})
		sb, err := sandbox.New(cfg)
		if err != nil {
			codeExecSandboxProbeErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := sb.Exec(ctx, "sh", []string{"-c", "true"})
		if err != nil {
			codeExecSandboxProbeErr = err
			return
		}
		if res == nil {
			codeExecSandboxProbeErr = fmt.Errorf("probe returned nil result")
			return
		}
		if res.ExitCode != 0 {
			codeExecSandboxProbeErr = fmt.Errorf("probe exit code %d", res.ExitCode)
		}
	})
	if codeExecSandboxProbeErr != nil {
		t.Skipf("linux sandbox backend unavailable: %v", codeExecSandboxProbeErr)
	}
}

func TestCodeExecSkill_Meta(t *testing.T) {
	s := newTestCodeExecSkill(t)
	if s.Name() != "code_exec" {
		t.Errorf("Name() = %q, want %q", s.Name(), "code_exec")
	}
	if s.Match("anything") {
		t.Error("Match() should always return false")
	}
	td := s.ToolDefinition()
	if td.Function.Name != "code_exec" {
		t.Errorf("ToolDefinition name = %q", td.Function.Name)
	}
}

func TestCodeExecSkill_ToolDefinitionP0Fields(t *testing.T) {
	s := newTestCodeExecSkill(t)
	props := s.ToolDefinition().Function.Parameters.Properties
	for _, field := range []string{"mode", "command", "entrypoint", "project_root", "files", "artifacts", "timeout"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("ToolDefinition missing P0 field %q", field)
		}
	}
}

func TestCodeExecSkill_Execute_Success(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('hello')",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "hello") {
		t.Errorf("got %q, want output to contain hello", result.Content)
	}
	if !strings.Contains(result.Content, `"run_id"`) {
		t.Errorf("got %q, want run_id metadata", result.Content)
	}
	if result.Metadata["status"] != "success" {
		t.Errorf("status = %q, want success", result.Metadata["status"])
	}
}

func TestCodeExecSkill_Execute_MissingArgs(t *testing.T) {
	s := newTestCodeExecSkill(t)
	_, err := s.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
	if !strings.Contains(err.Error(), "language and code are required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCodeExecSkill_Execute_NonZeroExit(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "invalid(",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "[exit code 1]") {
		t.Errorf("should contain exit code, got: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "syntax") {
		t.Errorf("should contain stderr, got: %s", result.Content)
	}
}

func TestCodeExecSkill_Execute_Artifacts(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "from pathlib import Path\nPath('artifacts/p0_artifact.txt').write_text('P0_ARTIFACT_OK')\nprint('done')",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"artifact", "p0_artifact.txt", `"max_workspace_bytes"`, `"max_artifact_bytes"`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("missing %q in output:\n%s", want, result.Content)
		}
	}
}

func TestCodeExecSkill_Execute_FileModeExistingFile(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "hello.py")
	if err := os.WriteFile(entry, []byte("print('P0_FILE_OK')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":       "file",
		"entrypoint": entry,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_FILE_OK") {
		t.Fatalf("file mode did not run existing file:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_FileModeNodeFile(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "hello.js")
	if err := os.WriteFile(entry, []byte("console.log('P0_NODE_FILE_OK')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":       "file",
		"entrypoint": entry,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_NODE_FILE_OK") {
		t.Fatalf("node file mode did not run existing file:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ModelFriendlyAliases(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "hello.py")
	if err := os.WriteFile(entry, []byte("print('P0_ALIAS_FILE_OK')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestCodeExecSkill(t)
	fileResult, err := s.Execute(context.Background(), map[string]any{
		"mode":      "file",
		"file_path": entry,
	})
	if err != nil {
		t.Fatalf("unexpected file alias error: %v", err)
	}
	if !strings.Contains(fileResult.Content, "P0_ALIAS_FILE_OK") {
		t.Fatalf("file_path alias did not run:\n%s", fileResult.Content)
	}

	project := t.TempDir()
	projectResult, err := s.Execute(context.Background(), map[string]any{
		"mode":              "project",
		"working_directory": project,
		"cmd":               []any{"python3", "-c", "print('P0_ALIAS_CMD_OK')"},
	})
	if err != nil {
		t.Fatalf("unexpected cmd alias error: %v", err)
	}
	if !strings.Contains(projectResult.Content, "P0_ALIAS_CMD_OK") {
		t.Fatalf("cmd alias did not run:\n%s", projectResult.Content)
	}
}

func TestCodeExecSkill_Execute_SnippetPythonCommandArray(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":    "snippet",
		"command": []any{"python3", "-c", "print('P0_SNIPPET_CMD_OK')"},
	})
	if err != nil {
		t.Fatalf("unexpected snippet command error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_SNIPPET_CMD_OK") {
		t.Fatalf("snippet python command did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_SnippetPythonCommandString(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":    "snippet",
		"command": `["python3","-c","print('P0_SNIPPET_CMD_STRING_OK')"]`,
	})
	if err != nil {
		t.Fatalf("unexpected snippet command string error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_SNIPPET_CMD_STRING_OK") {
		t.Fatalf("snippet python command string did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ModulePythonFiles(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "python",
		"files": []any{
			map[string]any{"path": "lib.py", "content": "def value(): return 'P0_MODULE_PY_OK'\n"},
			map[string]any{"path": "main.py", "content": "from lib import value\nprint(value())\n"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_MODULE_PY_OK") {
		t.Fatalf("python module files did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ModuleNodeFiles(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "javascript",
		"files": []any{
			map[string]any{"path": "helper.js", "content": "exports.value = () => 'P0_MODULE_NODE_OK'\n"},
			map[string]any{"path": "index.js", "content": "const helper = require('./helper'); console.log(helper.value())\n"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_MODULE_NODE_OK") {
		t.Fatalf("node module files did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ModuleGoFiles(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "go",
		"files": []any{
			map[string]any{"path": "go.mod", "content": "module example.com/p0module\n\ngo 1.24\n"},
			map[string]any{"path": "main.go", "content": "package p0module\n\nfunc Value() string { return \"P0_MODULE_GO_OK\" }\n"},
			map[string]any{"path": "main_test.go", "content": "package p0module\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != \"P0_MODULE_GO_OK\" { t.Fatal(Value()) } }\n"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") && !strings.Contains(result.Content, "ok  \texample.com/p0module") {
		t.Fatalf("go module files did not run tests:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ModuleGoFilesScrubsHostGoWork(t *testing.T) {
	hostWork := filepath.Join(t.TempDir(), "go.work")
	if err := os.WriteFile(hostWork, []byte("go 1.24\n\nuse /Users/hexagon/work/host-workspace-should-not-leak\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", hostWork)

	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "go",
		"files": []any{
			map[string]any{"path": "go.mod", "content": "module example.com/scrubgowork\n\ngo 1.24\n"},
			map[string]any{"path": "main.go", "content": "package scrubgowork\n\nfunc Value() string { return \"P0_GOWORK_SCRUB_OK\" }\n"},
			map[string]any{"path": "main_test.go", "content": "package scrubgowork\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != \"P0_GOWORK_SCRUB_OK\" { t.Fatal(Value()) } }\n"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") && !strings.Contains(result.Content, "ok  \texample.com/scrubgowork") {
		t.Fatalf("go module files should ignore host GOWORK:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_ProjectCommand(t *testing.T) {
	project := t.TempDir()
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []any{"python3", "-c", "print('P0_PROJECT_OK')"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "P0_PROJECT_OK") {
		t.Fatalf("project command did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_PythonShellCommandTask(t *testing.T) {
	project := t.TempDir()
	s := newTestCodeExecSkill(t)
	code := strings.Join([]string{
		"import os",
		"from pathlib import Path",
		"records = [('A', 3, 19), ('B', 2, 41), ('A', 5, 19), ('C', 7, 11)]",
		"revenue = {}",
		"for sku, qty, price in records:",
		"    revenue[sku] = revenue.get(sku, 0) + qty * price",
		"best = max(revenue, key=revenue.get)",
		"artifact_dir = Path(os.environ['HEXCLAW_ARTIFACT_DIR'])",
		"artifact_dir.mkdir(parents=True, exist_ok=True)",
		"(artifact_dir / 'python_shell_summary.txt').write_text(str(revenue))",
		"print(f'PY_SHELL_TASK_OK total={sum(revenue.values())} best={best}')",
	}, "\n")
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []any{"python3", "-c", code},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"PY_SHELL_TASK_OK total=311 best=A", "python_shell_summary.txt", `"mode":"project"`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("python shell command task missing %q:\n%s", want, result.Content)
		}
	}
}

func TestCodeExecSkill_Execute_CommandJSONStringArrayFromModel(t *testing.T) {
	project := t.TempDir()
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      `["python3","-c","print('PY_JSON_COMMAND_OK')"]`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PY_JSON_COMMAND_OK") {
		t.Fatalf("JSON-string command argv from model did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_CommandLooseJSONStringArrayFromModel(t *testing.T) {
	project := t.TempDir()
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command": `["python3", "-c", "
print('PY_LOOSE_JSON_COMMAND_OK')
"]`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PY_LOOSE_JSON_COMMAND_OK") {
		t.Fatalf("loose JSON-string command argv from model did not run:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_SnippetInfersPythonFromModelCode(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode": "snippet",
		"code": "import os\nfrom pathlib import Path\nartifact_dir = Path(os.environ['HEXCLAW_ARTIFACT_DIR'])\nartifact_dir.mkdir(parents=True, exist_ok=True)\n(artifact_dir / 'inferred_python.txt').write_text('ok')\nprint('PY_INFERRED_SNIPPET_OK')",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"PY_INFERRED_SNIPPET_OK", "inferred_python.txt", `"language":"python3"`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Python snippet language inference missing %q:\n%s", want, result.Content)
		}
	}
}

func TestCodeExecSkill_PreparesEnvDirsOutsideSandbox(t *testing.T) {
	base := t.TempDir()
	run := codeExecRun{
		ID:          "run_test",
		Scratch:     filepath.Join(base, "work"),
		ArtifactDir: filepath.Join(base, "work", "artifacts"),
		CacheDir:    filepath.Join(base, "work", "cache"),
		Config:      sandbox.Config{Network: true},
	}
	exports := codeExecEnv(run)
	if err := ensureCodeExecEnvDirs(exports); err != nil {
		t.Fatalf("ensureCodeExecEnvDirs: %v", err)
	}
	for _, key := range codeExecWritableEnvKeys {
		dir := strings.TrimSpace(exports[key])
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s dir was not created: %s: %v", key, dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s path is not a dir: %s", key, dir)
		}
	}
}

func TestCodeExecSkill_GoRuntimeReadablePathsIncludeSumDB(t *testing.T) {
	gopath := filepath.Join(t.TempDir(), "gopath")
	t.Setenv("GOPATH", gopath)

	paths := goRuntimeReadablePaths()
	want := filepath.Join(gopath, "pkg", "sumdb")
	if !slices.Contains(paths, want) {
		t.Fatalf("go runtime readable paths missing sumdb cache %q: %v", want, paths)
	}
}

func TestCodeExecSkill_PosixWrapperDoesNotMkdirInsideSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell wrapper only")
	}
	var script string
	s := NewCodeExecSkill(&mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(_ context.Context, cmd string, args []string) (*sandbox.ExecResult, error) {
			if cmd != "sh" {
				t.Fatalf("cmd = %q, want sh", cmd)
			}
			if len(args) >= 2 {
				script = args[1]
			}
			return &sandbox.ExecResult{Stdout: "PY_OK\n", ExitCode: 0}, nil
		}}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "snippet",
		"language": "python",
		"code":     "print('PY_OK')",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(script, "mkdir -p") {
		t.Fatalf("wrapper must not create dirs inside sandbox:\n%s", script)
	}
	if strings.Contains(result.Content, "Operation not permitted") || strings.Contains(result.Content, "mkdir: /Users") {
		t.Fatalf("wrapper leaked sandbox mkdir noise:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_RealSnippetNoMkdirNoise(t *testing.T) {
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "snippet",
		"language": "python",
		"code":     "print('PY_NO_MKDIR_NOISE_OK')",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(result.Content, "PY_NO_MKDIR_NOISE_OK") {
		t.Fatalf("snippet did not execute:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "Operation not permitted") || strings.Contains(result.Content, "mkdir: /Users") {
		t.Fatalf("real sandbox leaked wrapper mkdir noise:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_PythonCrawlerNetworkPolicy(t *testing.T) {
	if os.Getenv("HEXCLAW_CODE_EXEC_LIVE_NETWORK") != "1" {
		t.Skip("set HEXCLAW_CODE_EXEC_LIVE_NETWORK=1 to run live Python crawler network tests")
	}
	code := strings.Join([]string{
		"import re",
		"import urllib.request",
		"url = 'http://example.com/'",
		"req = urllib.request.Request(url, headers={'User-Agent': 'HexClawCrawlerTest/1.0'})",
		"try:",
		"    with urllib.request.urlopen(req, timeout=8) as resp:",
		"        body = resp.read(20000).decode('utf-8', 'replace')",
		"        status = getattr(resp, 'status', 0)",
		"    title = re.search(r'<title>(.*?)</title>', body, re.I | re.S)",
		"    title_text = ' '.join(title.group(1).split()) if title else 'NO_TITLE'",
		"    print(f'CRAWL_OK status={status} title={title_text} bytes={len(body)}')",
		"except Exception as e:",
		"    print('CRAWL_ERROR type=' + type(e).__name__ + ' message=' + str(e)[:160])",
	}, "\n")

	run := func(t *testing.T, network bool) string {
		t.Helper()
		ws := t.TempDir()
		cfg := sandbox.Config{Workspace: ws, Timeout: 20, Network: network}
		sb, err := sandbox.New(cfg)
		if err != nil {
			t.Fatalf("create sandbox network=%v: %v", network, err)
		}
		s := NewCodeExecSkill(sb, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, err := s.Execute(ctx, map[string]any{
			"mode":     "snippet",
			"language": "python",
			"code":     code,
			"timeout":  20,
		})
		if err != nil {
			t.Fatalf("execute crawler network=%v: %v", network, err)
		}
		return result.Content
	}

	offline := run(t, false)
	t.Logf("network=false crawler output:\n%s", offline)
	if strings.Contains(offline, "CRAWL_OK") {
		t.Fatalf("network=false should block crawler, got:\n%s", offline)
	}
	if !strings.Contains(offline, "CRAWL_ERROR") {
		t.Fatalf("network=false should report crawler error, got:\n%s", offline)
	}

	online := run(t, true)
	t.Logf("network=true crawler output:\n%s", online)
	if !strings.Contains(online, "CRAWL_OK") || !strings.Contains(online, "Example Domain") {
		t.Fatalf("network=true crawler did not fetch/parse example.com:\n%s", online)
	}
}

func TestCodeExecSkill_Execute_ProjectGoCommand(t *testing.T) {
	requireCodeExecSandbox(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := nearestProjectRoot(wd)
	s := NewCodeExecSkill(&mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		// 沙箱 per-run 冷 GOCACHE：嵌套 go test 每次全量编译整条 go.work 链，空载 ~18s、
		// 并行全量回归下实测 60s 会被挤爆（2026-07-03 负载 flaky 取证）。180s 留足余量，
		// 本测试意图是「project 模式能跑 go 命令」而非性能门。
		Timeout:       180,
		Network:       false,
		ReadablePaths: []string{filepath.Dir(root)},
	})
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": root,
		"command":      []any{"go", "test", "./skill/builtin", "-run", "TestCodeExecSkill_Meta", "-count=1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") && !strings.Contains(result.Content, "ok  \tgithub.com/hexagon-codes/hexclaw/skill/builtin") {
		t.Fatalf("go project command did not pass:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_OutputTruncation(t *testing.T) {
	requireCodeExecSandbox(t)
	sb := &mockSandbox{}
	// 直接字段赋值：sandbox.Config 的限额字段由 go.work 链接的 toolkit 保证存在，
	// 不再走反射设置（旧版曾用反射 + 版本 skip 兜底）。
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30, MaxOutputBytes: 32, MaxStderrBytes: 32}
	s := NewCodeExecSkill(sb, cfg)
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('A' * 200)",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "truncated=true") || !strings.Contains(result.Content, `"stdout_truncated":true`) {
		t.Fatalf("expected truncation metadata:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_RuntimeMissing(t *testing.T) {
	project := t.TempDir()
	s := newTestCodeExecSkill(t)
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []any{"hexclaw-p0-missing-runtime-xyz"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, `"runtime_missing":true`) {
		t.Fatalf("expected runtime_missing metadata:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_Timeout(t *testing.T) {
	requireCodeExecSandbox(t)
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 1, MaxOutputBytes: 1024, MaxStderrBytes: 1024}
	s := NewCodeExecSkill(sb, cfg)
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "import time\ntime.sleep(5)",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Content), "timeout") {
		t.Fatalf("expected timeout metadata:\n%s", result.Content)
	}
}

func TestCodeExecSkill_Execute_NetworkPolicyPropagatesToRunSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test inspects POSIX shell wrapper exports")
	}
	var mu sync.Mutex
	var networks []bool
	var scripts []string

	s := NewCodeExecSkill(&mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: false})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		mu.Lock()
		networks = append(networks, cfg.Network)
		mu.Unlock()
		return &mockSandbox{execFn: func(_ context.Context, _ string, args []string) (*sandbox.ExecResult, error) {
			mu.Lock()
			if len(args) >= 2 {
				scripts = append(scripts, args[1])
			}
			mu.Unlock()
			return &sandbox.ExecResult{Stdout: "ok", ExitCode: 0}, nil
		}}, nil
	}

	if _, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('offline')",
	}); err != nil {
		t.Fatalf("offline execute: %v", err)
	}
	if err := s.UpdateNetwork(true); err != nil {
		t.Fatalf("UpdateNetwork(true): %v", err)
	}
	if _, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('online')",
	}); err != nil {
		t.Fatalf("online execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(networks) < 3 {
		t.Fatalf("expected factory calls for offline execute, rebuild, and online execute; got %v", networks)
	}
	if networks[0] {
		t.Fatalf("offline execute used Network=true: %v", networks)
	}
	if !networks[len(networks)-1] {
		t.Fatalf("online execute did not use Network=true: %v", networks)
	}
	if len(scripts) < 2 {
		t.Fatalf("expected two execution scripts, got %d", len(scripts))
	}
	if strings.Contains(scripts[0], "GOMODCACHE") {
		t.Fatalf("offline execution exported GOMODCACHE:\n%s", scripts[0])
	}
	if !strings.Contains(scripts[len(scripts)-1], "GOMODCACHE") {
		t.Fatalf("online execution did not export GOMODCACHE:\n%s", scripts[len(scripts)-1])
	}
}

func TestCodeExecSkill_Execute_NetworkPolicyControlsDependencyInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test inspects POSIX shell wrapper commands")
	}
	run := func(t *testing.T, network bool) ([]string, string) {
		t.Helper()
		var mu sync.Mutex
		var scripts []string
		calls := 0
		s := NewCodeExecSkill(&mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: network})
		s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
			return &mockSandbox{execFn: func(_ context.Context, _ string, args []string) (*sandbox.ExecResult, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if len(args) >= 2 {
					scripts = append(scripts, args[1])
				}
				switch {
				case calls == 1:
					return &sandbox.ExecResult{Stderr: "ModuleNotFoundError: No module named 'pandas'", ExitCode: 1}, nil
				case calls == 2 && strings.Contains(args[1], "python3 -m pip install pandas"):
					return &sandbox.ExecResult{Stdout: "installed", ExitCode: 0}, nil
				default:
					return &sandbox.ExecResult{Stdout: "P0_DEP_OK", ExitCode: 0}, nil
				}
			}}, nil
		}
		result, err := s.Execute(context.Background(), map[string]any{
			"language": "python",
			"code":     "import pandas\nprint('P0_DEP_OK')",
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), scripts...), result.Content
	}

	offlineScripts, offlineOut := run(t, false)
	if len(offlineScripts) != 1 {
		t.Fatalf("network=false should not attempt dependency install; scripts=%d\n%s", len(offlineScripts), strings.Join(offlineScripts, "\n---\n"))
	}
	if !strings.Contains(offlineOut, `"dependency_missing":["pandas"]`) {
		t.Fatalf("network=false should still report dependency_missing:\n%s", offlineOut)
	}
	if strings.Contains(strings.Join(offlineScripts, "\n"), "pip install") {
		t.Fatalf("network=false attempted pip install:\n%s", strings.Join(offlineScripts, "\n---\n"))
	}

	onlineScripts, onlineOut := run(t, true)
	if len(onlineScripts) != 3 {
		t.Fatalf("network=true should run original, install, retry; scripts=%d\n%s", len(onlineScripts), strings.Join(onlineScripts, "\n---\n"))
	}
	if !strings.Contains(strings.Join(onlineScripts, "\n"), "python3 -m pip install pandas") {
		t.Fatalf("network=true did not attempt pip install:\n%s", strings.Join(onlineScripts, "\n---\n"))
	}
	if !strings.Contains(onlineOut, "P0_DEP_OK") {
		t.Fatalf("network=true did not retry successfully:\n%s", onlineOut)
	}
}

func TestCodeExecSkill_NetworkEnabled(t *testing.T) {
	s := newTestCodeExecSkill(t) // Network: true
	if !s.NetworkEnabled() {
		t.Error("should be true initially")
	}
}

func TestCodeExecSkill_UpdateNetwork_NoChange(t *testing.T) {
	s := newTestCodeExecSkill(t) // Network: true
	err := s.UpdateNetwork(true)
	if err != nil {
		t.Fatalf("no-op update should not error: %v", err)
	}
	if !s.NetworkEnabled() {
		t.Error("should still be true")
	}
}

func TestCodeExecSkill_UpdateNetwork_Toggle(t *testing.T) {
	// UpdateNetwork 调用 sandbox.New 重建实例。
	// 由于 sandbox.New 需要真实 workspace 目录，这里直接用 TempDir。
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: ws, Timeout: 30, Network: true}
	s := NewCodeExecSkill(sb, cfg)

	if !s.NetworkEnabled() {
		t.Fatal("should start with network enabled")
	}

	// 关闭网络 → 重建沙箱
	err := s.UpdateNetwork(false)
	if err != nil {
		t.Fatalf("UpdateNetwork(false) failed: %v", err)
	}
	if s.NetworkEnabled() {
		t.Error("should be disabled after update")
	}

	// 重新打开
	err = s.UpdateNetwork(true)
	if err != nil {
		t.Fatalf("UpdateNetwork(true) failed: %v", err)
	}
	if !s.NetworkEnabled() {
		t.Error("should be enabled after re-enabling")
	}
}

func TestCodeExecSkill_UpdateNetwork_FailureKeepsPreviousState(t *testing.T) {
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: ws, Timeout: 30, Network: true}
	s := NewCodeExecSkill(sb, cfg)

	s.cfg.Workspace = ""

	err := s.UpdateNetwork(false)
	if err == nil {
		t.Fatal("expected update error when rebuild sandbox fails")
	}
	if !s.NetworkEnabled() {
		t.Fatal("network state changed on failed rebuild")
	}
}

func TestCodeExecSkill_ConcurrentSafety(t *testing.T) {
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: ws, Timeout: 30, Network: true}
	s := NewCodeExecSkill(sb, cfg)
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, string, []string) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{Stdout: "1\n", ExitCode: 0}, nil
		}}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)

	// 10 个并发 Execute
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Execute(context.Background(), map[string]any{
				"language": "python",
				"code":     "print(1)",
			})
			if err != nil {
				errs <- fmt.Errorf("execute: %w", err)
			}
		}()
	}

	// 同时 5 个并发 UpdateNetwork 切换
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			enabled := i%2 == 0
			if err := s.UpdateNetwork(enabled); err != nil {
				errs <- fmt.Errorf("update(%v): %w", enabled, err)
			}
		}(i)
	}

	// 同时 5 个并发 NetworkEnabled 读取
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.NetworkEnabled() // 不应 panic
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}
}

func TestDetectMissingPackages(t *testing.T) {
	tests := []struct {
		lang   string
		stderr string
		want   []string
	}{
		{"python", "ModuleNotFoundError: No module named 'pandas'", []string{"pandas"}},
		{"python", "ImportError: No module named 'numpy.core'", []string{"numpy"}},
		{"javascript", "Cannot find module 'lodash'", []string{"lodash"}},
		{"go", "some error", nil},
		{"python", "no error here", nil},
	}
	for _, tt := range tests {
		got := detectMissingPackages(tt.lang, tt.stderr)
		if len(got) != len(tt.want) {
			t.Errorf("detectMissingPackages(%q, %q) = %v, want %v", tt.lang, tt.stderr, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("detectMissingPackages(%q, %q)[%d] = %q, want %q", tt.lang, tt.stderr, i, got[i], tt.want[i])
			}
		}
	}
}

func TestBuildInstallCommand(t *testing.T) {
	tests := []struct {
		lang string
		pkgs []string
		want string
	}{
		{"python", []string{"pandas", "numpy"}, "python3 -m pip install pandas numpy"},
		{"javascript", []string{"lodash"}, "npm install --no-save lodash"},
		{"go", []string{"pkg"}, ""},
	}
	for _, tt := range tests {
		got := buildInstallCommand(tt.lang, tt.pkgs)
		if got != tt.want {
			t.Errorf("buildInstallCommand(%q, %v) = %q, want %q", tt.lang, tt.pkgs, got, tt.want)
		}
	}
}
