package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

var codeExecTestGoBuildCacheBase string

func TestMain(m *testing.M) {
	userCache, cacheErr := os.UserCacheDir()
	if cacheErr != nil {
		fmt.Fprintf(os.Stderr, "locate CodeExec test cache: %v\n", cacheErr)
		os.Exit(1)
	}
	if mkdirErr := os.MkdirAll(userCache, 0700); mkdirErr != nil {
		fmt.Fprintf(os.Stderr, "create CodeExec test cache parent: %v\n", mkdirErr)
		os.Exit(1)
	}
	base, cacheErr := os.MkdirTemp(userCache, "hexclaw-code-exec-go-cache-test-*")
	if cacheErr != nil {
		fmt.Fprintf(os.Stderr, "create CodeExec test cache: %v\n", cacheErr)
		os.Exit(1)
	}
	codeExecTestGoBuildCacheBase = base
	exitCode := m.Run()
	if err := os.RemoveAll(base); err != nil {
		fmt.Fprintf(os.Stderr, "remove CodeExec test cache: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

// mockSandbox implements sandbox.Sandbox for testing.
type mockSandbox struct {
	execFn func(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error)
}

type codeExecTestCloser struct {
	err error
}

func (c codeExecTestCloser) Close() error {
	return c.err
}

func TestJoinCodeExecResourceClosePreservesErrors(t *testing.T) {
	runErr := errors.New("operation failed")
	closeErr := errors.New("close failed")
	joinCodeExecResourceClose(&runErr, codeExecTestCloser{err: closeErr}, "close test resource")
	if !errors.Is(runErr, closeErr) || !strings.Contains(runErr.Error(), "close test resource") {
		t.Fatalf("joined error = %v, want close error with operation", runErr)
	}
}

func testSandboxNetworkMode(enabled bool) sandbox.NetworkMode {
	if enabled {
		return sandbox.NetworkHost
	}
	return sandbox.NetworkDisabled
}

func (m *mockSandbox) Close() error {
	return nil
}

func (m *mockSandbox) Capabilities(context.Context) (sandbox.CapabilitySet, error) {
	return sandbox.UntrustedCodeIsolationCapabilities |
		sandbox.CapabilityMemory |
		sandbox.CapabilityProcesses |
		sandbox.CapabilityStorage, nil
}

func (m *mockSandbox) Exec(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
	if m.execFn != nil {
		return m.execFn(ctx, command)
	}
	return &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
		ProcessContainment: sandbox.LimitStatusEnforced,
	}}, nil
}

func newTestCodeExecSkill(t *testing.T) *CodeExecSkill {
	t.Helper()
	if codeExecTestNeedsRealSandbox(t.Name()) {
		requireCodeExecSandbox(t)
	}
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: sandbox.NetworkDisabled}
	return newConfiguredTestCodeExecSkill(t, sb, cfg)
}

func newConfiguredTestCodeExecSkill(
	t *testing.T,
	sb sandbox.Sandbox,
	cfg sandbox.Config,
) *CodeExecSkill {
	t.Helper()
	s := NewCodeExecSkill(sb, cfg)
	s.scratchBase = filepath.Join(cfg.Workspace, "project-scratch")
	s.goBuildCacheBase = codeExecTestGoBuildCacheBase
	return s
}

func codeExecConfigForTest(s *CodeExecSkill) sandbox.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCodeExecSandboxConfig(cloneCodeExecPolicyState(s.policy).cfg)
}

func mutateCodeExecConfigForTest(s *CodeExecSkill, mutate func(*sandbox.Config)) {
	s.policyUpdateMu.Lock()
	defer s.policyUpdateMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	state := cloneCodeExecPolicyState(s.policy)
	mutate(&state.cfg)
	s.policy = state
}

func commitCodeExecPolicyForTest(t *testing.T, s *CodeExecSkill, policy SandboxPolicy) {
	t.Helper()
	candidate, err := s.PrepareSandboxPolicy(context.Background(), policy)
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy: %v", err)
	}
	candidate.Commit()
}

func writeCodeExecTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

func codeExecTestGOROOT(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locate Go executable: %v", err)
	}
	output, err := exec.CommandContext(t.Context(), goBinary, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("query Go root: %v", err)
	}
	goRoot := strings.TrimSpace(string(output))
	if goRoot == "" {
		t.Fatal("Go root is empty")
	}
	return goRoot
}

func newBoundCodeExecTestToolchain(
	t *testing.T,
	goBinary string,
	goRoot string,
	identity string,
) *codeExecGoToolchainDescriptor {
	t.Helper()
	canonicalBinary, err := codeExecCanonicalRuntimeExecutable(goBinary)
	if err != nil {
		t.Fatalf("canonicalize test Go executable: %v", err)
	}
	canonicalGOROOT, err := canonicalCodeExecPath(goRoot)
	if err != nil {
		t.Fatalf("canonicalize test GOROOT: %v", err)
	}
	binding, err := inspectCodeExecRegularFileNoFollow(canonicalBinary, true)
	if err != nil {
		t.Fatalf("bind test Go executable: %v", err)
	}
	return &codeExecGoToolchainDescriptor{
		Binary:         canonicalBinary,
		BinarySHA256:   binding.SHA256,
		CompileVersion: "compile version test",
		GOROOT:         canonicalGOROOT,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		GOVERSION:      "go-test",
		Identity:       identity,
		binaryIdentity: binding,
	}
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
		res, err := sb.Exec(ctx, sandbox.Command{Path: "/bin/sh", Args: []string{"-c", "true"}, Dir: ws})
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
	language := props["language"]
	if language == nil {
		t.Fatal("ToolDefinition missing language field")
	}
	for _, want := range []string{"project", "structured direct go argv"} {
		if !strings.Contains(language.Description, want) {
			t.Fatalf("language description must mention %q: %s", want, language.Description)
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

func TestCodeExecRequestRequiresStructuredArgvAndDisablesArtifactsByDefault(t *testing.T) {
	_, err := parseCodeExecRequest(map[string]any{
		"mode":         "project",
		"project_root": t.TempDir(),
		"command":      "python3 -c 'print(1)'",
	})
	if err == nil || !strings.Contains(err.Error(), "structured command argv") {
		t.Fatalf("command string error = %v, want structured argv rejection", err)
	}

	request, err := parseCodeExecRequest(map[string]any{
		"mode":     "snippet",
		"language": "python",
		"code":     "print(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Artifacts {
		t.Fatal("artifact collection must be explicitly requested")
	}
}

func TestCodeExecSkill_Execute_Artifacts(t *testing.T) {
	report := executeCodeExecArtifactReport(t, true, 1024, func(workspace string) {
		writeCodeExecTestFile(t, filepath.Join(workspace, "artifacts", "p0_artifact.txt"), "P0_ARTIFACT_OK")
	}, nil)
	if report.Status != "success" || len(report.Artifacts) != 1 {
		t.Fatalf("artifact report = %#v, want one successful artifact", report)
	}
	artifact := report.Artifacts[0]
	if artifact.Path != "p0_artifact.txt" || artifact.Size != int64(len("P0_ARTIFACT_OK")) || len(artifact.SHA256) != 64 {
		t.Fatalf("artifact manifest entry = %#v", artifact)
	}
}

func TestCodeExecArtifactManifestRejectsSymlinkAndPreservesExistingError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links may require elevated Windows privileges")
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	writeCodeExecTestFile(t, outside, "outside-secret")
	report := executeCodeExecArtifactReport(t, true, 1024, func(workspace string) {
		if err := os.Symlink(outside, filepath.Join(workspace, "artifacts", "leak.txt")); err != nil {
			t.Fatalf("create artifact symlink: %v", err)
		}
	}, errors.New("payload failed marker"))

	if report.Status != "failed" {
		t.Fatalf("status = %q, want failed", report.Status)
	}
	for _, marker := range []string{"payload failed marker", "artifact"} {
		if !strings.Contains(report.Error, marker) {
			t.Fatalf("report error missing %q: %q", marker, report.Error)
		}
	}
	if len(report.Artifacts) != 0 {
		t.Fatalf("symlink artifact escaped collection boundary: %#v", report.Artifacts)
	}
}

func TestCodeExecArtifactManifestRejectsHardLink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	writeCodeExecTestFile(t, outside, "outside-secret")
	report := executeCodeExecArtifactReport(t, true, 1024, func(workspace string) {
		if err := os.Link(outside, filepath.Join(workspace, "artifacts", "leak.txt")); err != nil {
			t.Skipf("hard links are unavailable: %v", err)
		}
	}, nil)

	if report.Status != "failed" || !strings.Contains(report.Error, "artifact") {
		t.Fatalf("hard-linked artifact did not fail closed: %#v", report)
	}
	if len(report.Artifacts) != 0 {
		t.Fatalf("hard-linked artifact was collected: %#v", report.Artifacts)
	}
}

func TestCodeExecArtifactManifestRejectsFileAboveLimit(t *testing.T) {
	report := executeCodeExecArtifactReport(t, true, 8, func(workspace string) {
		writeCodeExecTestFile(t, filepath.Join(workspace, "artifacts", "too-large.txt"), "123456789")
	}, nil)

	if report.Status != "failed" || !strings.Contains(report.Error, "artifact") ||
		!strings.Contains(report.Error, "size limit") {
		t.Fatalf("oversized artifact did not fail closed: %#v", report)
	}
}

func TestCodeExecArtifactsFalseSkipsCollection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links may require elevated Windows privileges")
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	writeCodeExecTestFile(t, outside, "outside-secret")
	report := executeCodeExecArtifactReport(t, false, 1024, func(workspace string) {
		if err := os.Symlink(outside, filepath.Join(workspace, "artifacts", "must-not-scan.txt")); err != nil {
			t.Fatalf("create disabled artifact symlink: %v", err)
		}
	}, nil)

	if report.Status != "success" || report.Error != "" || len(report.Artifacts) != 0 {
		t.Fatalf("artifacts=false still scanned artifacts: %#v", report)
	}
	if enabled, ok := report.Capabilities["artifact_manifest"].(bool); !ok || enabled {
		t.Fatalf("artifact_manifest capability = %#v, want false", report.Capabilities["artifact_manifest"])
	}
}

func TestCodeExecArtifactsFailClosedWithoutProcessContainment(t *testing.T) {
	workspace := t.TempDir()
	artifactDir := filepath.Join(workspace, "artifacts")
	if err := os.MkdirAll(artifactDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeCodeExecTestFile(t, filepath.Join(artifactDir, "must-not-scan.txt"), "payload")
	readCalls := 0
	report := codeExecReport{
		MaxWorkspaceBytes: 1024,
		MaxArtifactBytes:  1024,
		Capabilities:      buildCodeExecCapabilities(sandbox.LimitReport{ProcessContainment: sandbox.LimitStatusUnsupported}),
	}
	err := finalizeCodeExecReportWithObserver(
		context.Background(),
		codeExecRun{Scratch: workspace, ArtifactDir: artifactDir},
		true,
		&report,
		func() { readCalls++ },
	)
	if err == nil || !strings.Contains(err.Error(), "process containment") {
		t.Fatalf("artifact finalization error = %v, want process-containment fail-closed rejection", err)
	}
	if readCalls != 0 || len(report.Artifacts) != 0 {
		t.Fatalf("unsupported process containment performed traversal: reads=%d artifacts=%v", readCalls, report.Artifacts)
	}
}

func TestCodeExecManifestWriteFailureFailsReport(t *testing.T) {
	report := executeCodeExecArtifactReport(t, true, 1024, func(workspace string) {
		manifestPath := filepath.Join(filepath.Dir(workspace), "manifest.json")
		if err := os.Mkdir(manifestPath, 0700); err != nil {
			t.Fatalf("create manifest collision: %v", err)
		}
	}, nil)

	if report.Status != "failed" || !strings.Contains(report.Error, "write code execution manifest") {
		t.Fatalf("manifest write failure did not fail report: %#v", report)
	}
	if enabled, ok := report.Capabilities["artifact_manifest"].(bool); !ok || enabled {
		t.Fatalf("artifact_manifest capability = %#v, want false", report.Capabilities["artifact_manifest"])
	}
}

func TestCodeExecFinalizeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace:        t.TempDir(),
		Timeout:          30,
		MaxArtifactBytes: 1024,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			cancel()
			return &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
				ProcessContainment: sandbox.LimitStatusEnforced,
			}}, nil
		}}, nil
	}
	result, err := s.Execute(ctx, map[string]any{
		"mode":      "snippet",
		"language":  "python",
		"code":      "print('ok')",
		"artifacts": true,
	})
	if err != nil {
		t.Fatalf("execute canceled finalize fixture: %v", err)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("report type = %T, want codeExecReport", result.Data)
	}
	if report.Status != "failed" || !strings.Contains(report.Error, context.Canceled.Error()) {
		t.Fatalf("finalize ignored cancellation: %#v", report)
	}
}

func executeCodeExecArtifactReport(
	t *testing.T,
	artifacts bool,
	maxArtifactBytes int64,
	setup func(workspace string),
	execErr error,
) codeExecReport {
	t.Helper()
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace:        t.TempDir(),
		Timeout:          30,
		MaxArtifactBytes: maxArtifactBytes,
	})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			if setup != nil {
				setup(cfg.Workspace)
			}
			result := &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
				ProcessContainment: sandbox.LimitStatusEnforced,
			}}
			if execErr != nil {
				result.ExitCode = 1
			}
			return result, execErr
		}}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":      "snippet",
		"language":  "python",
		"code":      "print('ok')",
		"artifacts": artifacts,
	})
	if err != nil {
		t.Fatalf("execute artifact fixture: %v", err)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("report type = %T, want codeExecReport", result.Data)
	}
	return report
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
	mutateCodeExecConfigForTest(s, func(cfg *sandbox.Config) { cfg.Timeout = 180 })
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
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data = %T, want codeExecReport", result.Data)
	}
	wantCWD := filepath.Join(report.Paths["workspace"], "work")
	if report.Paths["cwd"] != wantCWD {
		t.Fatalf("reported cwd = %q, want final Go runtime directory %q", report.Paths["cwd"], wantCWD)
	}
	goBuildCache := filepath.Join(report.Paths["workspace"], "cache", "go-build")
	if _, err := os.Lstat(goBuildCache); !os.IsNotExist(err) {
		t.Fatalf("run-local Go build cache persisted after execution: %v", err)
	}
}

func TestCodeExecSkill_Execute_ModuleGoFilesScrubsHostGoWork(t *testing.T) {
	hostWork := filepath.Join(t.TempDir(), "go.work")
	if err := os.WriteFile(hostWork, []byte("go 1.24\n\nuse /Users/hexagon/work/host-workspace-should-not-leak\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", hostWork)

	s := newTestCodeExecSkill(t)
	mutateCodeExecConfigForTest(s, func(cfg *sandbox.Config) { cfg.Timeout = 180 })
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
		"records = [('A', 3, 19), ('B', 2, 41), ('A', 5, 19), ('C', 7, 11)]",
		"revenue = {}",
		"for sku, qty, price in records:",
		"    revenue[sku] = revenue.get(sku, 0) + qty * price",
		"best = max(revenue, key=revenue.get)",
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
	for _, want := range []string{"PY_SHELL_TASK_OK total=311 best=A", `"mode":"project"`} {
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
		"code": "print('PY_INFERRED_SNIPPET_OK')",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"PY_INFERRED_SNIPPET_OK", `"language":"python3"`} {
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
	if err := ensureCodeExecEnvDirs(run, exports); err != nil {
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

func TestCodeExecGoRunUsesOnlyPrivateModuleCache(t *testing.T) {
	goRoot := codeExecTestGOROOT(t)
	home := t.TempDir()
	gopath := filepath.Join(home, "go")
	hostModuleCache := filepath.Join(gopath, "pkg", "mod")
	for _, path := range []string{
		hostModuleCache,
		filepath.Join(gopath, "pkg", "sumdb"),
		filepath.Join(home, ".cache", "go-build"),
		filepath.Join(home, "Library", "Caches", "go-build"),
	} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatalf("create host Go cache fixture: %v", err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("GOPATH", gopath)
	t.Setenv("GOMODCACHE", hostModuleCache)

	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: true}
	plan := codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: &codeExecGoToolchainDescriptor{GOROOT: goRoot},
	}
	run, err := prepareCodeExecRun(
		context.Background(),
		cfg,
		codeExecRequest{Mode: "module"},
		nil,
		"",
		nil,
		plan,
	)
	if err != nil {
		t.Fatalf("prepare Go module run: %v", err)
	}
	if run.Config.Network {
		t.Fatal("Go module run retained network access")
	}
	for _, readable := range run.Config.ReadablePaths {
		if pathWithinResolved(home, readable) {
			t.Fatalf("Go module run exposed host cache path %q", readable)
		}
	}
	exports := codeExecEnv(run)
	wantModuleCache := filepath.Join(run.CacheDir, "gomod")
	if got := exports["GOMODCACHE"]; got != wantModuleCache {
		t.Fatalf("GOMODCACHE = %q, want private cache %q", got, wantModuleCache)
	}
}

func TestCodeExecGoClosureDoesNotExposeHostCacheWithoutDependencies(t *testing.T) {
	hostModuleCache := t.TempDir()
	t.Setenv("GOMODCACHE", hostModuleCache)
	run := newCodeExecGoClosureTestRun(t, codeExecGoModEdit{
		Module: codeExecGoModuleRef{Path: "example.com/app"},
	}, nil)

	if err := prepareCodeExecRunGoDependencyClosure(context.Background(), &run); err != nil {
		t.Fatalf("prepare dependency-free Go closure: %v", err)
	}
	if run.GoVendored {
		t.Fatal("dependency-free Go run unexpectedly enabled vendor mode")
	}
	for _, readable := range run.Config.ReadablePaths {
		if pathWithinResolved(hostModuleCache, readable) {
			t.Fatalf("final Go run exposed host module cache %q", readable)
		}
	}
	if !slices.Contains(run.Config.DeniedPaths, resolveRealPath(hostModuleCache)) {
		t.Fatalf("final Go run did not deny host module cache: %v", run.Config.DeniedPaths)
	}
}

func TestCodeExecGoClosureProjectsDependenciesBeforeFinalSandbox(t *testing.T) {
	hostModuleCache := t.TempDir()
	t.Setenv("GOMODCACHE", hostModuleCache)
	var helperConfigs []sandbox.Config
	run := newCodeExecGoClosureTestRun(t, codeExecGoModEdit{
		Module: codeExecGoModuleRef{Path: "example.com/app"},
		Require: []codeExecGoModuleRef{
			{Path: "example.com/dependency", Version: "v1.0.0"},
		},
	}, func(cfg sandbox.Config, operation string, workspace string) {
		helperConfigs = append(helperConfigs, cfg)
		if strings.Contains(operation, "mod") && strings.Contains(operation, "vendor") {
			writeCodeExecTestFile(t, filepath.Join(workspace, "vendor", "modules.txt"), "# example.com/dependency v1.0.0\n")
			writeCodeExecTestFile(t, filepath.Join(workspace, "vendor", "example.com", "dependency", "dependency.go"), "package dependency\n")
		}
	})

	if err := prepareCodeExecRunGoDependencyClosure(context.Background(), &run); err != nil {
		t.Fatalf("prepare private Go dependency closure: %v", err)
	}
	if !run.GoVendored {
		t.Fatal("projected Go dependency closure did not enable vendor mode")
	}
	canonicalHostCache := resolveRealPath(hostModuleCache)
	var inspectWithoutHost, vendorWithHost bool
	for _, cfg := range helperConfigs {
		if cfg.Network {
			t.Fatalf("Go dependency helper retained network access: %#v", cfg)
		}
		hasHost := slices.Contains(cfg.ReadablePaths, canonicalHostCache)
		if hasHost {
			vendorWithHost = true
		} else {
			inspectWithoutHost = true
		}
	}
	if !inspectWithoutHost || !vendorWithHost {
		t.Fatalf("helper cache authorization was not phase-scoped: %#v", helperConfigs)
	}
	for _, readable := range run.Config.ReadablePaths {
		if pathWithinResolved(hostModuleCache, readable) {
			t.Fatalf("final Go run exposed host module cache %q", readable)
		}
	}
	if !slices.Contains(run.Config.DeniedPaths, canonicalHostCache) {
		t.Fatalf("final Go run did not deny host module cache: %v", run.Config.DeniedPaths)
	}
	if got := codeExecEnv(run)["GOMODCACHE"]; got != filepath.Join(run.CacheDir, "gomod") {
		t.Fatalf("final GOMODCACHE = %q, want private run cache", got)
	}
}

func newCodeExecGoClosureTestRun(
	t *testing.T,
	edit codeExecGoModEdit,
	onCall func(cfg sandbox.Config, operation string, workspace string),
) codeExecRun {
	t.Helper()
	workspace := t.TempDir()
	cacheDir := filepath.Join(workspace, "cache")
	writeCodeExecTestFile(t, filepath.Join(workspace, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	encodedEdit, err := json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	var currentConfig sandbox.Config
	helper := codeExecGoHelper{Factory: func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		currentConfig = cfg
		return &mockSandbox{execFn: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			operation := strings.Join(command.Args, " ")
			if onCall != nil {
				onCall(currentConfig, operation, workspace)
			}
			switch {
			case strings.Contains(operation, "mod") && strings.Contains(operation, "edit"):
				return &sandbox.ExecResult{Stdout: string(encodedEdit), ExitCode: 0}, nil
			case strings.Contains(operation, "mod") && strings.Contains(operation, "vendor"):
				return &sandbox.ExecResult{ExitCode: 0}, nil
			default:
				return &sandbox.ExecResult{ExitCode: 1}, nil
			}
		}}, nil
	}}
	cfg := ensureCodeExecConfigDefaults(sandbox.Config{
		Workspace: workspace,
		Timeout:   30,
		Network:   false,
	})
	return codeExecRun{
		ID:        "closure-test",
		Base:      workspace,
		Workspace: workspace,
		Scratch:   workspace,
		CacheDir:  cacheDir,
		Plan: codeExecExecutionPlan{
			GoRuntime: true,
			Toolchain: newBoundCodeExecTestToolchain(
				t,
				goBinary,
				t.TempDir(),
				strings.Repeat("a", 64),
			),
			Helper: helper,
		},
		Config: cfg,
	}
}

func TestCodeExecSkill_UsesStructuredCommandWithoutInternalShell(t *testing.T) {
	expectedRuntime, err := codeExecFindRuntimeExecutable("python3")
	if err != nil {
		t.Skipf("python3 is unavailable: %v", err)
	}
	var captured sandbox.Command
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			if command.Path != expectedRuntime {
				t.Fatalf("command path = %q, want frozen runtime %q", command.Path, expectedRuntime)
			}
			captured = command
			return &sandbox.ExecResult{Stdout: "PY_OK\n", ExitCode: 0}, nil
		}}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":      "snippet",
		"language":  "python",
		"code":      "print('PY_OK')",
		"artifacts": false,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured.Path != expectedRuntime || !filepath.IsAbs(captured.Path) || len(captured.Args) != 1 ||
		strings.Contains(strings.Join(captured.Args, " "), "env -i") {
		t.Fatalf("structured command = %#v, want direct python3 argv", captured)
	}
	if strings.Contains(result.Content, "Operation not permitted") || strings.Contains(result.Content, "mkdir: /Users") {
		t.Fatalf("structured command leaked setup noise:\n%s", result.Content)
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

func TestCodeExecSkill_Execute_PythonCrawlerHostNetworkRejected(t *testing.T) {
	if os.Getenv("HEXCLAW_CODE_EXEC_LIVE_NETWORK") != "1" {
		t.Skip("set HEXCLAW_CODE_EXEC_LIVE_NETWORK=1 to run the crawler network rejection contract")
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

	run := func(t *testing.T, network bool) (string, error) {
		t.Helper()
		ws := t.TempDir()
		validatedConfig := sandbox.Config{
			Workspace:            ws,
			Timeout:              20,
			Network:              sandbox.NetworkDisabled,
			RequiredCapabilities: sandbox.UntrustedCodeIsolationCapabilities,
		}
		sb, err := sandbox.New(validatedConfig)
		if err != nil {
			t.Fatalf("create offline sandbox: %v", err)
		}
		cfg := validatedConfig
		cfg.Network = testSandboxNetworkMode(network)
		s := newConfiguredTestCodeExecSkill(t, sb, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, err := s.Execute(ctx, map[string]any{
			"mode":     "snippet",
			"language": "python",
			"code":     code,
			"timeout":  20,
		})
		if err != nil {
			return "", err
		}
		return result.Content, nil
	}

	offline, err := run(t, false)
	if err != nil {
		t.Fatalf("execute offline crawler: %v", err)
	}
	t.Logf("network=false crawler output:\n%s", offline)
	if strings.Contains(offline, "CRAWL_OK") {
		t.Fatalf("network=false should block crawler, got:\n%s", offline)
	}
	if !strings.Contains(offline, "CRAWL_ERROR") {
		t.Fatalf("network=false should report crawler error, got:\n%s", offline)
	}

	online, err := run(t, true)
	if online != "" || !errors.Is(err, errCodeExecHostNetworkUnsupported) {
		t.Fatalf("host-network crawler = (%q, %v), want fail-closed rejection", online, err)
	}
}

func TestBUG20260727001_CodeExecProjectGoCommandUsesSelfContainedStagedWorkspace(t *testing.T) {
	requireCodeExecSandbox(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := nearestProjectRoot(wd)
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
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
		"command":      []any{"go", "test", "./internal/upstreamerr", "-run", "TestPublicMessage_StripsRawProviderBody", "-count=1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") && !strings.Contains(result.Content, "ok  \tgithub.com/hexagon-codes/hexclaw/internal/upstreamerr") {
		t.Fatalf("go project command did not pass:\n%s", result.Content)
	}
}

func TestCodeExecGoStageSourcesExcludeUnrelatedGoWorkMembers(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	dependency := filepath.Join(root, "dependency")
	unrelated := filepath.Join(root, "unrelated-large-repository")
	sources := []string{app, dependency, unrelated}
	moduleEdits := map[string]codeExecGoModEdit{
		app: {
			Module:  codeExecGoModuleRef{Path: "example.com/app"},
			Require: []codeExecGoModuleRef{{Path: "example.com/dependency", Version: "v0.0.0"}},
		},
		dependency: {Module: codeExecGoModuleRef{Path: "example.com/dependency"}},
		unrelated:  {Module: codeExecGoModuleRef{Path: "example.com/unrelated"}},
	}
	got, err := selectCodeExecGoStageSources(sources, app, moduleEdits, codeExecGoWorkEdit{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{app, dependency}; !slices.Equal(got, want) {
		t.Fatalf("selected Go workspace closure = %q, want %q", got, want)
	}
}

func TestBUG20260727001_CodeExecProjectShellWrappedGoCommandStagesClosure(t *testing.T) {
	project := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(project, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	workspace := t.TempDir()
	stageCalls := 0
	finalCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: workspace,
		Timeout:   180,
	})
	s.projectStager = func(context.Context, string, string, codeExecExecutionPlan, *FileAccessBroker, sandbox.Config) (string, string, bool, error) {
		stageCalls++
		return "", "", false, errors.New("unexpected staging")
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "go",
		"project_root": project,
		"command":      []any{"bash", "--norc", "-c", "env | sort && go test ./... -count=1"},
	})
	if err == nil || !strings.Contains(err.Error(), "structured direct go argv") {
		t.Fatalf("shell-wrapped Go command error = %v, want direct-only contract rejection", err)
	}
	if stageCalls != 0 || finalCalls != 0 {
		t.Fatalf("rejected wrapper reached staging/final sandbox: stage=%d final=%d", stageCalls, finalCalls)
	}
	entries, readErr := os.ReadDir(workspace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected wrapper created %d workspace entries, want 0", len(entries))
	}
}

func TestCodeExecNeedsGoRuntimeUsesExplicitMetadata(t *testing.T) {
	tests := []struct {
		name    string
		request codeExecRequest
		want    bool
	}{
		{name: "direct", request: codeExecRequest{Command: []string{"go", "test", "./..."}}, want: true},
		{name: "direct executable", request: codeExecRequest{Command: []string{"go.exe", "test", "./..."}}, want: true},
		{name: "absolute direct", request: codeExecRequest{Command: []string{"/usr/local/go/bin/go", "test", "./..."}}, want: false},
		{name: "explicit Go command text", request: codeExecRequest{Language: "go", CommandText: "printf '%s\\n' 'literal; go test is data only'"}, want: false},
		{name: "explicit Golang Bash", request: codeExecRequest{Language: "golang", Command: []string{"bash", "--norc", "-c", "go test ./..."}}, want: false},
		{name: "explicit safe environment", request: codeExecRequest{Language: "go", Command: []string{"env", "LANG=C", "go", "test", "./..."}}, want: true},
		{name: "explicit Go command prompt", request: codeExecRequest{Language: "go", Command: []string{"cmd.exe", "/D", "/S", "/C", "go test ./..."}}, want: false},
		{name: "explicit Go PowerShell", request: codeExecRequest{Language: "go", Command: []string{"powershell", "-NoProfile", "-Command", "& { go test ./... }"}}, want: false},
		{name: "wrapped shell without metadata", request: codeExecRequest{Command: []string{"sh", "-c", "go test ./..."}}, want: false},
		{name: "wrapped environment without metadata", request: codeExecRequest{Command: []string{"env", "-u", "GOWORK", "go", "test", "./..."}}, want: false},
		{name: "command text without metadata", request: codeExecRequest{CommandText: "printf '%s\\n' 'literal; go test is data only'"}, want: false},
		{name: "direct non-Go", request: codeExecRequest{Command: []string{"echo", "go", "test"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeExecNeedsGoRuntime(tt.request); got != tt.want {
				t.Fatalf("codeExecNeedsGoRuntime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodeExecGoModeRejectsUnstructuredExecutionPlans(t *testing.T) {
	tests := []struct {
		name    string
		request codeExecRequest
	}{
		{name: "command text", request: codeExecRequest{Language: "go", CommandText: "go test ./..."}},
		{name: "POSIX shell", request: codeExecRequest{Language: "go", Command: []string{"sh", "-c", "go test ./..."}}},
		{name: "Bash", request: codeExecRequest{Language: "go", Command: []string{"bash", "-c", "go test ./..."}}},
		{name: "Zsh", request: codeExecRequest{Language: "go", Command: []string{"zsh", "-c", "go test ./..."}}},
		{name: "Make", request: codeExecRequest{Language: "go", Command: []string{"make", "test"}}},
		{name: "Just", request: codeExecRequest{Language: "go", Command: []string{"just", "test"}}},
		{name: "Task", request: codeExecRequest{Language: "go", Command: []string{"task", "test"}}},
		{name: "project script", request: codeExecRequest{Language: "go", Command: []string{"./scripts/test"}}},
		{name: "absolute Go binary", request: codeExecRequest{Language: "go", Command: []string{filepath.Join(t.TempDir(), "go"), "test", "./..."}}},
		{name: "controlled environment", request: codeExecRequest{Language: "go", Command: []string{"env", "GOMODCACHE=/tmp/host-cache", "go", "test", "./..."}}},
		{name: "environment option", request: codeExecRequest{Language: "go", Command: []string{"env", "-u", "GOWORK", "go", "test", "./..."}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := codeExecRequestMayUseGo(tt.request); err == nil || !strings.Contains(err.Error(), "go execution") {
				t.Fatalf("Go execution plan error = %v, want fail-closed direct argv rejection", err)
			}
		})
	}
}

func TestCodeExecGoModeAllowsExplicitSafeEnvironmentAssignments(t *testing.T) {
	req := codeExecRequest{
		Language: "go",
		Command:  []string{"env", "LANG=C", "CUSTOM_BUILD_MARKER=verified", "go", "test", "./..."},
	}
	usesGo, err := codeExecRequestMayUseGo(req)
	if err != nil {
		t.Fatalf("validate direct Go argv: %v", err)
	}
	if !usesGo {
		t.Fatal("safe environment assignments did not select the Go execution plan")
	}
}

func TestPrepareCodeExecRunClearsHostReadablePathsAfterTrustedStaging(t *testing.T) {
	hostProject := t.TempDir()
	hostReadable := filepath.Dir(hostProject)
	stageSawHostAuthorization := false
	run, err := prepareCodeExecRun(
		context.Background(),
		sandbox.Config{Workspace: t.TempDir(), ReadablePaths: []string{hostReadable}},
		codeExecRequest{Mode: "project", Language: "python", ProjectRoot: hostProject},
		nil,
		t.TempDir(),
		func(
			_ context.Context,
			gotHostProject string,
			stageRoot string,
			_ codeExecExecutionPlan,
			_ *FileAccessBroker,
			cfg sandbox.Config,
		) (string, string, bool, error) {
			stageSawHostAuthorization = slices.ContainsFunc(cfg.ReadablePaths, func(path string) bool {
				return pathWithinResolved(path, hostProject) || pathWithinResolved(hostProject, path)
			})
			stagedProject := filepath.Join(stageRoot, "project")
			if err := os.MkdirAll(stagedProject, 0700); err != nil {
				return "", "", false, err
			}
			if resolveRealPath(gotHostProject) != resolveRealPath(hostProject) {
				return "", "", false, fmt.Errorf("trusted stager received project %q, want %q", gotHostProject, hostProject)
			}
			return stagedProject, "", false, nil
		},
		codeExecExecutionPlan{},
	)
	if err != nil {
		t.Fatalf("prepare staged run: %v", err)
	}
	if !stageSawHostAuthorization {
		t.Fatal("trusted stager did not receive the original readable paths")
	}
	for _, readable := range run.Config.ReadablePaths {
		if pathWithinResolved(readable, hostProject) || pathWithinResolved(hostProject, readable) {
			t.Fatalf("final sandbox retained host readable path %q for project %q", readable, hostProject)
		}
	}
	if !slices.ContainsFunc(run.Config.DeniedPaths, func(path string) bool {
		return pathWithinResolved(path, hostProject)
	}) {
		t.Fatalf("final sandbox denied paths do not contain host project %q: %v", hostProject, run.Config.DeniedPaths)
	}
}

func TestCodeExecSkill_Execute_FinalProjectCannotReadOriginalHostTree(t *testing.T) {
	requireCodeExecSandbox(t)
	hostProject := t.TempDir()
	secretPath := filepath.Join(hostProject, "host-secret.txt")
	writeCodeExecTestFile(t, secretPath, "HOST_SECRET_MUST_NOT_LEAK")
	program := fmt.Sprintf(`from pathlib import Path
try:
    print(Path(%q).read_text())
except Exception:
    print("HOST_READ_BLOCKED")
`, secretPath)
	writeCodeExecTestFile(t, filepath.Join(hostProject, "main.py"), program)
	s := newConfiguredTestCodeExecSkill(t, nil, sandbox.Config{
		Workspace:     t.TempDir(),
		Timeout:       30,
		ReadablePaths: []string{hostProject},
	})
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "python",
		"project_root": hostProject,
		"command":      []any{"python3", "main.py"},
		"artifacts":    false,
	})
	if err != nil {
		t.Fatalf("execute staged host-read probe: %v", err)
	}
	if !strings.Contains(result.Content, "HOST_READ_BLOCKED") || strings.Contains(result.Content, "HOST_SECRET_MUST_NOT_LEAK") {
		t.Fatalf("final process host-read boundary failed:\n%s", result.Content)
	}
}

func TestFinalizeCodeExecReportArtifactsFalsePerformsZeroTraversal(t *testing.T) {
	readCalls := 0
	run := codeExecRun{
		Scratch:     filepath.Join(t.TempDir(), "must-not-be-read"),
		ArtifactDir: filepath.Join(t.TempDir(), "must-not-be-read-artifacts"),
	}
	report := codeExecReport{MaxWorkspaceBytes: 1024, MaxArtifactBytes: 1024}
	if err := finalizeCodeExecReportWithObserver(context.Background(), run, false, &report, func() {
		readCalls++
	}); err != nil {
		t.Fatalf("finalize artifacts=false report: %v", err)
	}
	if readCalls != 0 {
		t.Fatalf("artifacts=false performed %d traversal reads, want 0", readCalls)
	}
	if report.WorkspaceBytes != 0 || len(report.Artifacts) != 0 {
		t.Fatalf("artifacts=false report contains traversal-derived data: %#v", report)
	}
}

func TestBuildCodeExecCapabilitiesDoesNotAssumeProcessContainment(t *testing.T) {
	capabilities := buildCodeExecCapabilities(sandbox.LimitReport{})
	if got, ok := capabilities["process_containment"].(bool); !ok || got {
		t.Fatalf("process_containment = %#v, want false without an enforced backend capability", capabilities["process_containment"])
	}
}

func TestCodeExecProjectWrappedCommandIsRejectedBeforeStaging(t *testing.T) {
	t.Setenv("GOWORK", "off")
	project := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(project, "go.mod"), "module example.com/app\n\ngo 1.24\n")

	tests := []struct {
		name    string
		command any
	}{
		{name: "Bash options", command: []any{"bash", "--norc", "-c", "go test ./..."}},
		{name: "environment options", command: []any{"env", "-u", "GOWORK", "go", "test", "./..."}},
		{name: "command prompt", command: []any{"cmd.exe", "/D", "/S", "/C", "go test ./..."}},
		{name: "PowerShell", command: []any{"pwsh", "-NoProfile", "-Command", "& { go test ./... }"}},
		{name: "quoted command text", command: "printf '%s\\n' 'literal; go test is data only'"},
		{name: "Make", command: []any{"make", "test"}},
		{name: "Just", command: []any{"just", "test"}},
		{name: "Task", command: []any{"task", "test"}},
		{name: "project script", command: []any{"./scripts/test", "./..."}},
		{name: "unknown runner", command: []any{"custom-runner", "test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finalSandboxCalls := 0
			stageCalls := 0
			workspace := t.TempDir()
			s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
				Workspace: workspace,
				Timeout:   30,
				Network:   false,
			})
			s.projectStager = func(
				context.Context,
				string,
				string,
				codeExecExecutionPlan,
				*FileAccessBroker,
				sandbox.Config,
			) (string, string, bool, error) {
				stageCalls++
				return "", "", false, fmt.Errorf("unexpected project staging")
			}
			s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
				finalSandboxCalls++
				return &mockSandbox{}, nil
			}
			_, err := s.Execute(context.Background(), map[string]any{
				"mode":         "project",
				"project_root": project,
				"command":      tt.command,
			})
			if err == nil || !strings.Contains(err.Error(), "go execution") &&
				!strings.Contains(err.Error(), "structured command argv") {
				t.Fatalf("wrapped Go project command error = %v, want direct-only contract rejection", err)
			}
			if finalSandboxCalls != 0 {
				t.Fatalf("wrapped command started final sandbox %d times, want 0", finalSandboxCalls)
			}
			if stageCalls != 0 {
				t.Fatalf("wrapped command started staging/helper %d times, want 0", stageCalls)
			}
			entries, readErr := os.ReadDir(workspace)
			if readErr != nil {
				t.Fatalf("read rejected-run workspace: %v", readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("metadata preflight created %d staging entries before rejection, want 0", len(entries))
			}
		})
	}
}

func TestCodeExecProjectWrappedTextWithoutGoMetadataIsRejected(t *testing.T) {
	project := t.TempDir()
	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      "printf '%s\\n' 'literal; go test is data only'",
	})
	if err == nil || !strings.Contains(err.Error(), "structured command argv") {
		t.Fatalf("wrapped non-Go text error = %v, want structured argv rejection", err)
	}
	if finalSandboxCalls != 0 {
		t.Fatalf("wrapped non-Go command started final sandbox %d times, want 0", finalSandboxCalls)
	}
}

func TestBUG20260727001_CodeExecProjectBuildIgnoredImportDoesNotRequireLocalModule(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	dependencyDir := filepath.Join(hostWorkspace, "dependency")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse (\n\t./app\n\t./dependency\n)\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "go.mod"), "module example.com/dependency\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "dependency.go"), "package dependency\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

replace example.com/dependency => ../dependency
`)
	writeCodeExecTestFile(t, filepath.Join(appDir, "app.go"), "package app\n\nconst Ready = true\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "ignored.go"), `//go:build ignore

package app

import _ "example.com/dependency"
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": appDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("build-ignored import must not require a local module: %v", err)
	}
	if finalSandboxCalls != 0 || !strings.Contains(result.Content, "[no test files]") {
		t.Fatalf("final sandbox calls = %d, content = %q; want no final run and a no-test result", finalSandboxCalls, result.Content)
	}
}

func TestBUG20260727001_CodeExecProjectStandardLibraryImportDoesNotMatchLocalModuleName(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	netModuleDir := filepath.Join(hostWorkspace, "net-module")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse (\n\t./app\n\t./net-module\n)\n")
	writeCodeExecTestFile(t, filepath.Join(netModuleDir, "go.mod"), "module net\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "app.go"), `package app

import "net/http"

const MethodGet = http.MethodGet
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": appDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("standard library import must not match a local module name: %v", err)
	}
	if finalSandboxCalls != 0 || !strings.Contains(result.Content, "[no test files]") {
		t.Fatalf("final sandbox calls = %d, content = %q; want no final run and a no-test result", finalSandboxCalls, result.Content)
	}
}

func TestBUG20260727001_CodeExecProjectNestedModuleStaysOutsideParentValidation(t *testing.T) {
	t.Setenv("GOWORK", "off")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	dependencyDir := filepath.Join(hostWorkspace, "dependency")
	nestedDir := filepath.Join(appDir, "nested")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "go.mod"), "module example.com/dependency\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "dependency.go"), "package dependency\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

replace example.com/dependency => ../dependency
`)
	writeCodeExecTestFile(t, filepath.Join(appDir, "app.go"), "package app\n\nconst Ready = true\n")
	writeCodeExecTestFile(t, filepath.Join(nestedDir, "go.mod"), "module example.com/nested\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(nestedDir, "nested.go"), `package nested

import _ "example.com/dependency"
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": appDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("nested module must stay outside parent validation: %v", err)
	}
	if finalSandboxCalls != 0 || !strings.Contains(result.Content, "[no test files]") {
		t.Fatalf("final sandbox calls = %d, content = %q; want no final run and a no-test result", finalSandboxCalls, result.Content)
	}
}

func TestCodeExecProjectUnrelatedAncestorGoWorkDoesNotRequireLanguage(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	goAppDir := filepath.Join(hostWorkspace, "go-app")
	nonGoDir := filepath.Join(hostWorkspace, "desktop")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse ./go-app\n")
	writeCodeExecTestFile(t, filepath.Join(goAppDir, "go.mod"), "module example.com/go-app\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(nonGoDir, "package.json"), "{\"private\":true}\n")

	finalSandboxCalls := 0
	stageCalls := 0
	goHelperStageCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		// 本用例验证 replace 闭包与模块身份；私有缓存种子、go list 和 go test -c
		// 的冷启动时间不属于被测不变量，因此沿用 Go 项目测试的 180 秒预算。
		Timeout: 180,
		Network: false,
	})
	s.SetFileAccess(NewFileAccessBroker([]string{nonGoDir}))
	s.projectStager = func(
		ctx context.Context,
		hostProjectRoot string,
		stageRoot string,
		plan codeExecExecutionPlan,
		broker *FileAccessBroker,
		cfg sandbox.Config,
	) (string, string, bool, error) {
		stageCalls++
		if plan.GoRuntime {
			goHelperStageCalls++
		}
		return stageCodeExecProject(ctx, hostProjectRoot, stageRoot, plan, broker, cfg)
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": nonGoDir,
		"command":      []any{"sh", "-c", "printf unrelated-workspace"},
	})
	if err != nil {
		t.Fatalf("unrelated ancestor go.work must not require Go language metadata: %v", err)
	}
	if finalSandboxCalls != 1 {
		t.Fatalf("final sandbox calls = %d, want 1", finalSandboxCalls)
	}
	if stageCalls != 1 || goHelperStageCalls != 0 {
		t.Fatalf("staging calls = %d, Go helper staging calls = %d; want 1 and 0", stageCalls, goHelperStageCalls)
	}
}

func TestBUG20260727001_CodeExecProjectDirectGoIgnoresUnauthorizedUnrelatedAncestorGoWork(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	otherDir := filepath.Join(hostWorkspace, "other")
	projectDir := filepath.Join(hostWorkspace, "project")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse ./other\n")
	writeCodeExecTestFile(t, filepath.Join(otherDir, "go.mod"), "module example.com/other\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(otherDir, "other.go"), "package other\n")
	writeCodeExecTestFile(t, filepath.Join(projectDir, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(projectDir, "project.go"), "package project\n")

	finalSandboxCalls := 0
	stageCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	s.SetFileAccess(NewFileAccessBroker([]string{projectDir}))
	s.projectStager = func(
		ctx context.Context,
		hostProjectRoot string,
		stageRoot string,
		plan codeExecExecutionPlan,
		broker *FileAccessBroker,
		cfg sandbox.Config,
	) (string, string, bool, error) {
		stageCalls++
		if !plan.GoRuntime {
			t.Fatal("direct go command must select the Go staged closure")
		}
		return stageCodeExecProject(ctx, hostProjectRoot, stageRoot, plan, broker, cfg)
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": projectDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("unrelated ancestor go.work must be ignored without widening broker access: %v", err)
	}
	if stageCalls != 1 || finalSandboxCalls != 0 || !strings.Contains(result.Content, "[no test files]") {
		t.Fatalf("staging/final calls = %d/%d, content = %q; want 1/0 and a no-test result", stageCalls, finalSandboxCalls, result.Content)
	}
}

func TestBUG20260727001_CodeExecProjectUnusedReplaceOnlyModuleCannotOverwriteActivePackage(t *testing.T) {
	t.Setenv("GOWORK", "off")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	activeDir := filepath.Join(hostWorkspace, "active")
	shadowDir := filepath.Join(hostWorkspace, "shadow")
	writeCodeExecTestFile(t, filepath.Join(activeDir, "go.mod"), "module example.com/shared\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(activeDir, "pkg", "marker.go"), "package pkg\n\nconst Marker = \"active\"\n")
	writeCodeExecTestFile(t, filepath.Join(shadowDir, "go.mod"), "module example.com/shared/pkg\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(shadowDir, "marker.go"), "package pkg\n\nconst Marker = \"unused-shadow\"\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

require example.com/shared v0.0.0

replace example.com/shared => ../active
replace example.com/shared/pkg => ../shadow
`)
	writeCodeExecTestFile(t, filepath.Join(appDir, "app_test.go"), `package app

import (
	"testing"

	"example.com/shared/pkg"
)

func TestActivePackage(t *testing.T) {
	if pkg.Marker != "active" {
		t.Fatal(pkg.Marker)
	}
}
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		// 本用例验证 replace 闭包与模块身份；私有缓存种子、go list 和 go test -c
		// 的冷启动时间不属于被测不变量，因此沿用 Go 项目测试的 180 秒预算。
		Timeout: 180,
		Network: false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": appDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("unused replace-only module must not overwrite the active package graph: %v", err)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok || report.Status != "success" {
		t.Fatalf("unused replace-only module report = %#v; content = %q", result.Data, result.Content)
	}
	if finalSandboxCalls != 1 {
		t.Fatalf("final sandbox calls = %d, want 1; content = %q", finalSandboxCalls, result.Content)
	}
}

func TestCodeExecProjectAncestorGoWorkMemberRequiresExplicitLanguage(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	moduleDir := filepath.Join(hostWorkspace, "app")
	projectDir := filepath.Join(moduleDir, "cmd", "worker")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse ./app\n")
	writeCodeExecTestFile(t, filepath.Join(moduleDir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(projectDir, "main.go"), "package main\n\nfunc main() {}\n")

	finalSandboxCalls := 0
	stageCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
		Network:   false,
	})
	s.projectStager = func(
		context.Context,
		string,
		string,
		codeExecExecutionPlan,
		*FileAccessBroker,
		sandbox.Config,
	) (string, string, bool, error) {
		stageCalls++
		return "", "", false, fmt.Errorf("unexpected project staging")
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": projectDir,
		"command":      []any{"sh", "-c", "go test ./..."},
	})
	if err == nil || !strings.Contains(err.Error(), "go execution") {
		t.Fatalf("workspace member wrapper error = %v, want direct-only contract rejection", err)
	}
	if finalSandboxCalls != 0 {
		t.Fatalf("workspace member wrapper started final sandbox %d times, want 0", finalSandboxCalls)
	}
	if stageCalls != 0 {
		t.Fatalf("workspace member wrapper started staging/helper %d times, want 0", stageCalls)
	}
}

func TestCodeExecProjectRootGoWorkRequiresExplicitLanguageWithoutHelper(t *testing.T) {
	t.Setenv("GOWORK", "off")
	project := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(project, "go.work"), "go 1.24\n")

	finalSandboxCalls := 0
	stageCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
		Network:   false,
	})
	s.projectStager = func(
		context.Context,
		string,
		string,
		codeExecExecutionPlan,
		*FileAccessBroker,
		sandbox.Config,
	) (string, string, bool, error) {
		stageCalls++
		return "", "", false, fmt.Errorf("unexpected project staging")
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}

	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []any{"sh", "-c", "printf workspace"},
	})
	if err == nil || !strings.Contains(err.Error(), "structured direct go argv") {
		t.Fatalf("project-root go.work wrapper error = %v, want direct-only contract rejection", err)
	}
	if stageCalls != 0 || finalSandboxCalls != 0 {
		t.Fatalf("preflight calls: staging/helper=%d final=%d; want 0 and 0", stageCalls, finalSandboxCalls)
	}
}

func TestBUG20260727001_CodeExecProjectStagesLocalUseAndReplaceClosure(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	toolkitDir := filepath.Join(hostWorkspace, "toolkit")
	schemaDir := filepath.Join(hostWorkspace, "schema")
	for _, dir := range []string{appDir, toolkitDir, schemaDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse (\n\t./app\n\t./toolkit\n)\n")
	write(filepath.Join(schemaDir, "go.mod"), "module example.com/schema\n\ngo 1.24\n")
	write(filepath.Join(schemaDir, "schema.go"), `package schema

type Node struct {
	AnyOf []string
	OneOf []string
	AllOf []string
	Not   *Node
}
`)
	write(filepath.Join(toolkitDir, "go.mod"), "module example.com/toolkit\n\ngo 1.24\n")
	write(filepath.Join(toolkitDir, "toolkit.go"), "package toolkit\n\nfunc Marker() string { return \"STAGED_TOOLKIT_OK\" }\n")
	write(filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

require (
	example.com/schema v0.0.0
	example.com/toolkit v0.0.0
)

replace example.com/schema => ../schema
replace example.com/toolkit => ../toolkit
`)
	write(filepath.Join(appDir, "app_test.go"), `package app

import (
	"testing"

	"example.com/schema"
	"example.com/toolkit"
)

func TestClosure(t *testing.T) {
	node := schema.Node{
		AnyOf: []string{"a"},
		OneOf: []string{"b"},
		AllOf: []string{"c"},
		Not:   &schema.Node{},
	}
	if len(node.AnyOf) != 1 || len(node.OneOf) != 1 || len(node.AllOf) != 1 || node.Not == nil {
		t.Fatal("schema composition fields were downgraded")
	}
	if toolkit.Marker() != "STAGED_TOOLKIT_OK" {
		t.Fatal(toolkit.Marker())
	}
}
`)

	// 本用例验证依赖闭包暂存，不验证运行超时策略。嵌套项目使用每次执行独立的
	// 冷 GOCACHE，并发运行包测试时可能超过通用测试辅助流程的 30 秒预算。
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
		Network:   false,
	})
	var trustedStageRoot, trustedProject, trustedGoWork string
	var trustedVendored bool
	s.projectStager = func(
		ctx context.Context,
		hostProjectRoot string,
		stageRoot string,
		plan codeExecExecutionPlan,
		broker *FileAccessBroker,
		cfg sandbox.Config,
	) (string, string, bool, error) {
		if plan.Toolchain == nil {
			t.Fatal("project staging started before the Go toolchain was resolved")
		}
		project, goWork, vendored, err := stageCodeExecProject(
			ctx,
			hostProjectRoot,
			stageRoot,
			plan,
			broker,
			cfg,
		)
		if err == nil {
			trustedStageRoot = stageRoot
			trustedProject = project
			trustedGoWork = goWork
			trustedVendored = vendored
		}
		return project, goWork, vendored, err
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": appDir,
		"command":      []any{"go", "test", "./...", "-count=1"},
	})
	if err != nil {
		t.Fatalf("execute staged project: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") {
		t.Fatalf("local use/replace project did not pass:\n%s", result.Content)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data = %T, want codeExecReport", result.Data)
	}
	if report.Status != "success" {
		t.Fatalf("result status = %q, want success", report.Status)
	}
	stagedWorkspace := report.Paths["workspace"]
	stagedProject := report.Paths["project_root"]
	if !pathWithinResolved(stagedWorkspace, stagedProject) {
		t.Fatalf("project_root %q is outside staged workspace %q", stagedProject, stagedWorkspace)
	}
	hostWorkspace = resolveRealPath(hostWorkspace)
	for _, key := range []string{"cwd", "project_root"} {
		if strings.Contains(report.Paths[key], hostWorkspace) {
			t.Fatalf("%s leaked host path: %s", key, report.Paths[key])
		}
	}
	if trustedStageRoot == "" || trustedProject == "" || trustedGoWork == "" || !trustedVendored {
		t.Fatalf(
			"trusted staging facts are incomplete: root=%q project=%q go.work=%q vendored=%v",
			trustedStageRoot,
			trustedProject,
			trustedGoWork,
			trustedVendored,
		)
	}
	for _, metadata := range []string{
		trustedGoWork,
		filepath.Join(trustedProject, "go.mod"),
		filepath.Join(trustedStageRoot, "vendor", "modules.txt"),
	} {
		data, err := os.ReadFile(metadata)
		if err != nil {
			t.Fatalf("read staged metadata %s: %v", metadata, err)
		}
		if strings.Contains(string(data), hostWorkspace) {
			t.Fatalf("staged metadata retained host path %q:\n%s", hostWorkspace, data)
		}
	}
}

func TestBUG20260727001_CodeExecProjectMissingLocalReplaceFailsBeforeCommand(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(`module example.com/app

go 1.24

require example.com/missing v0.0.0

replace example.com/missing => ../missing
`), 0644); err != nil {
		t.Fatal(err)
	}

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
		Network:   false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []any{"go", "test", "./...", "-count=1"},
	})
	if err == nil || !strings.Contains(err.Error(), "project dependency closure") {
		t.Fatalf("missing local replace must fail with dependency closure error, got %v", err)
	}
	if finalSandboxCalls != 0 {
		t.Fatalf("project command sandbox started %d times, want 0", finalSandboxCalls)
	}
}

func TestBUG20260727001_CodeExecProjectMissingDirectRequireFailsBeforeCommand(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	dependencyDir := filepath.Join(hostWorkspace, "dependency")
	writeCodeExecTestFile(t, filepath.Join(hostWorkspace, "go.work"), "go 1.24\n\nuse (\n\t./app\n\t./dependency\n)\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "go.mod"), "module example.com/dependency\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "dependency.go"), "package dependency\n\nfunc Marker() string { return \"DIRECT_REQUIRE_OK\" }\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

replace example.com/dependency => ../dependency
`)
	writeCodeExecTestFile(t, filepath.Join(appDir, "app_test.go"), `package app

import (
	"testing"

	"example.com/dependency"
)

func TestClosure(t *testing.T) {
	if dependency.Marker() != "DIRECT_REQUIRE_OK" {
		t.Fatal(dependency.Marker())
	}
}
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
		Network:   false,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "go",
		"project_root": appDir,
		"command":      []any{"go", "test", "./...", "-count=1"},
	})
	if err == nil || !strings.Contains(err.Error(), "project dependency closure") {
		t.Fatalf("missing direct require must fail with dependency closure error, got %v", err)
	}
	if strings.Contains(err.Error(), resolveRealPath(hostWorkspace)) {
		t.Fatalf("missing direct require error leaked host path: %v", err)
	}
	if finalSandboxCalls != 0 {
		t.Fatalf("missing direct require started final sandbox %d times, want 0", finalSandboxCalls)
	}
}

func TestBUG20260727001_CodeExecProjectUnauthorizedLocalTargetFailsBeforeCommand(t *testing.T) {
	t.Setenv("GOWORK", "")
	hostWorkspace := t.TempDir()
	appDir := filepath.Join(hostWorkspace, "app")
	dependencyDir := filepath.Join(hostWorkspace, "dependency")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.work"), "go 1.24\n\nuse (\n\t.\n\t../dependency\n)\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "go.mod"), "module example.com/dependency\n\ngo 1.24\n")
	writeCodeExecTestFile(t, filepath.Join(dependencyDir, "dependency.go"), "package dependency\n")
	writeCodeExecTestFile(t, filepath.Join(appDir, "go.mod"), `module example.com/app

go 1.24

require example.com/dependency v0.0.0

replace example.com/dependency => ../dependency
`)

	finalSandboxCalls := 0
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
		Network:   false,
	})
	s.SetFileAccess(NewFileAccessBroker([]string{appDir}))
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		finalSandboxCalls++
		return &mockSandbox{}, nil
	}
	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "go",
		"project_root": appDir,
		"command":      []any{"go", "test", "./..."},
	})
	if err == nil || !strings.Contains(err.Error(), "project dependency closure") || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("unauthorized local target must fail with dependency closure authorization error, got %v", err)
	}
	if strings.Contains(err.Error(), resolveRealPath(hostWorkspace)) {
		t.Fatalf("authorization error leaked host path: %v", err)
	}
	if finalSandboxCalls != 0 {
		t.Fatalf("unauthorized local target started final sandbox %d times, want 0", finalSandboxCalls)
	}
}

func TestCodeExecSkill_Execute_OutputTruncation(t *testing.T) {
	requireCodeExecSandbox(t)
	sb := &mockSandbox{}
	// 直接字段赋值：sandbox.Config 的限额字段由 go.work 链接的 toolkit 保证存在，
	// 不再走反射设置（旧版曾用反射 + 版本跳过机制兜底）。
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30, MaxOutputBytes: 32, MaxStderrBytes: 32}
	s := newConfiguredTestCodeExecSkill(t, sb, cfg)
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
	s := newConfiguredTestCodeExecSkill(t, sb, cfg)
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
	var mu sync.Mutex
	var networks []bool
	var factoryConfigs []sandbox.Config
	var scripts []string

	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: sandbox.NetworkDisabled})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		mu.Lock()
		networks = append(networks, cfg.Network == sandbox.NetworkHost)
		factoryConfigs = append(factoryConfigs, cfg)
		mu.Unlock()
		return &mockSandbox{execFn: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			mu.Lock()
			scripts = append(scripts, strings.Join(command.Env, "\n"))
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
	policy := s.SandboxPolicy()
	commitCodeExecPolicyForTest(t, s, policy)
	if _, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('still offline')",
	}); err != nil {
		t.Fatalf("second offline execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(networks) < 3 {
		t.Fatalf("expected factory calls for offline execute, rebuild, and online execute; got %v", networks)
	}
	if networks[0] {
		t.Fatalf("offline execute used Network=true: %v", networks)
	}
	if networks[len(networks)-1] {
		t.Fatalf("policy refresh used Network=true: %v", networks)
	}
	for index, cfg := range factoryConfigs {
		wantCapabilities := sandbox.UntrustedCodeIsolationCapabilities
		if cfg.MaxMemoryBytes > 0 {
			wantCapabilities |= sandbox.CapabilityMemory
		}
		if cfg.MaxProcesses > 0 {
			wantCapabilities |= sandbox.CapabilityProcesses
		}
		if cfg.MaxWorkspaceBytes > 0 || cfg.MaxArtifactBytes > 0 {
			wantCapabilities |= sandbox.CapabilityStorage
		}
		if cfg.RequiredCapabilities != wantCapabilities {
			t.Fatalf("sandbox factory %d RequiredCapabilities = %s, want %s", index, cfg.RequiredCapabilities, wantCapabilities)
		}
	}
	if len(scripts) < 2 {
		t.Fatalf("expected two structured environments, got %d", len(scripts))
	}
	for index, script := range scripts {
		if strings.Contains(script, "GOMODCACHE") {
			t.Fatalf("non-Go execution %d exported GOMODCACHE:\n%s", index, script)
		}
	}
}

func TestCodeExecGoBuildEnvironmentUsesPrivateOfflineModuleCache(t *testing.T) {
	hostModCache := filepath.Join(t.TempDir(), "gomodcache")
	if err := os.MkdirAll(hostModCache, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", hostModCache)
	run := newCodeExecGoExecutionTestRun(t)
	environment, err := codeExecGoBuildEnvironment(run)
	if err != nil {
		t.Fatalf("derive trusted Go build environment: %v", err)
	}
	wantModuleCache := filepath.Join(run.CacheDir, "gomod")
	if environment["GOMODCACHE"] != wantModuleCache {
		t.Fatalf("GOMODCACHE = %q, want %q", environment["GOMODCACHE"], wantModuleCache)
	}
	if strings.Contains(environment["GOMODCACHE"], resolveRealPath(hostModCache)) {
		t.Fatalf("trusted build environment exposed host module cache: %q", environment["GOMODCACHE"])
	}
	for key, want := range map[string]string{
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "off",
	} {
		if environment[key] != want {
			t.Fatalf("%s = %q, want %q", key, environment[key], want)
		}
	}
}

func TestCodeExecGoHelperUsesHardSandboxBoundary(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	requested := sandbox.Config{
		Workspace:         workspace,
		Timeout:           int((24 * time.Hour) / time.Second),
		Network:           true,
		MaxOutputBytes:    1 << 30,
		MaxStderrBytes:    1 << 30,
		MaxWorkspaceBytes: 1 << 40,
		MaxMemoryBytes:    1 << 40,
		MaxProcesses:      1 << 20,
	}
	var captured sandbox.Config
	var remaining time.Duration
	helper := codeExecGoHelper{
		Factory: func(cfg sandbox.Config) (sandbox.Sandbox, error) {
			captured = cfg
			return &mockSandbox{execFn: func(ctx context.Context, _ sandbox.Command) (*sandbox.ExecResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("Go helper context has no hard deadline")
				}
				remaining = time.Until(deadline)
				return &sandbox.ExecResult{Stdout: "darwin\namd64\n", ExitCode: 0}, nil
			}}, nil
		},
	}

	result, err := helper.Run(
		context.Background(),
		requested,
		workspace,
		workspace,
		nil,
		goBinary,
		[]string{"env", "GOOS", "GOARCH"},
		map[string]string{"HOME": workspace},
	)
	if err != nil {
		t.Fatalf("run sandboxed Go helper: %v", err)
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("Go helper result = %#v", result)
	}
	if captured.Network {
		t.Fatal("Go helper enabled network access")
	}
	canonicalWorkspace, err := resolveCodeExecBoundaryPath(workspace)
	if err != nil {
		t.Fatalf("canonicalize helper workspace: %v", err)
	}
	if captured.Workspace != canonicalWorkspace {
		t.Fatalf("Go helper workspace = %q, want %q", captured.Workspace, canonicalWorkspace)
	}
	if captured.Timeout != int(codeExecGoHelperHardTimeout/time.Second) {
		t.Fatalf("Go helper timeout = %d, want %d", captured.Timeout, int(codeExecGoHelperHardTimeout/time.Second))
	}
	if remaining <= 0 || remaining > codeExecGoHelperHardTimeout {
		t.Fatalf("Go helper context remaining = %s, want (0, %s]", remaining, codeExecGoHelperHardTimeout)
	}
	if captured.MaxOutputBytes != codeExecGoHelperMaxOutputBytes ||
		captured.MaxStderrBytes != codeExecGoHelperMaxStderrBytes ||
		captured.MaxWorkspaceBytes != codeExecGoHelperMaxWorkspaceBytes ||
		captured.MaxMemoryBytes != codeExecGoHelperMaxMemoryBytes ||
		captured.MaxProcesses != 0 {
		t.Fatalf("Go helper resource boundary = %#v", captured)
	}
	wantCapabilities := sandbox.TrustedBuildIsolationCapabilities |
		sandbox.CapabilityMemory |
		sandbox.CapabilityStorage
	if captured.RequiredCapabilities != wantCapabilities {
		t.Fatalf("Go helper RequiredCapabilities = %s, want %s", captured.RequiredCapabilities, wantCapabilities)
	}
}

func TestCodeExecTrustedCacheIsDeniedBeforeProjectStaging(t *testing.T) {
	cacheBase := t.TempDir()
	projectRoot := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(projectRoot, "go.mod"), "module example.com/project\n\ngo 1.24\n")

	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
	})
	s.goBuildCacheBase = cacheBase
	deniedBeforeStaging := false
	s.projectStager = func(
		_ context.Context,
		_ string,
		stageRoot string,
		_ codeExecExecutionPlan,
		_ *FileAccessBroker,
		cfg sandbox.Config,
	) (string, string, bool, error) {
		deniedBeforeStaging = slices.ContainsFunc(cfg.DeniedPaths, func(path string) bool {
			return resolveRealPath(path) == resolveRealPath(cacheBase)
		})
		stagedProject := filepath.Join(stageRoot, "project")
		writeCodeExecTestFile(t, filepath.Join(stagedProject, "go.mod"), "module example.com/project\n\ngo 1.24\n")
		return stagedProject, "", false, nil
	}
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{Stdout: "ok", ExitCode: 0}, nil
		}}, nil
	}

	if _, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "go",
		"project_root": projectRoot,
		"command":      []any{"go", "env", "GOOS"},
	}); err == nil || !strings.Contains(err.Error(), "accepts only run or test") {
		t.Fatalf("Go policy rejection = %v, want run/test-only error", err)
	}
	if !deniedBeforeStaging {
		t.Fatal("trusted Go cache deny was not present before project staging")
	}
}

func TestCodeExecTrustedCacheBoundaryRejectsWorkspaceOverlap(t *testing.T) {
	tests := []struct {
		name      string
		workspace func(string) string
		cacheBase func(string) string
	}{
		{
			name:      "cache inside workspace",
			workspace: func(root string) string { return filepath.Join(root, "workspace") },
			cacheBase: func(root string) string { return filepath.Join(root, "workspace", "trusted-cache") },
		},
		{
			name:      "workspace inside cache",
			workspace: func(root string) string { return filepath.Join(root, "trusted-cache", "workspace") },
			cacheBase: func(root string) string { return filepath.Join(root, "trusted-cache") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := tt.workspace(root)
			cacheBase := tt.cacheBase(root)
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: workspace, Timeout: 30})
			s.goBuildCacheBase = cacheBase
			_, err := s.Execute(context.Background(), map[string]any{
				"mode":     "module",
				"language": "go",
				"command":  []any{"go", "env", "GOOS"},
				"files": []any{
					map[string]any{"path": "go.mod", "content": "module example.com/overlap\n\ngo 1.24\n"},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "must not overlap") {
				t.Fatalf("workspace/cache overlap error = %v", err)
			}
		})
	}
}

func TestCodeExecProjectRejectsLocalDependencyInsideTrustedCache(t *testing.T) {
	tests := []struct {
		name       string
		projectMod string
		workFile   string
	}{
		{
			name: "go.mod replace",
			projectMod: `module example.com/project

go 1.24

replace example.com/cache => ../trusted-cache
`,
		},
		{
			name:       "go.work use",
			projectMod: "module example.com/project\n\ngo 1.24\n",
			workFile:   "go 1.24\n\nuse (\n\t./project\n\t./trusted-cache\n)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			projectRoot := filepath.Join(root, "project")
			cacheBase := filepath.Join(root, "trusted-cache")
			writeCodeExecTestFile(t, filepath.Join(projectRoot, "go.mod"), tt.projectMod)
			writeCodeExecTestFile(t, filepath.Join(cacheBase, "go.mod"), "module example.com/cache\n\ngo 1.24\n")
			writeCodeExecTestFile(t, filepath.Join(cacheBase, "cache.go"), "package cache\n")
			if tt.workFile != "" {
				workPath := filepath.Join(root, "go.work")
				writeCodeExecTestFile(t, workPath, tt.workFile)
				t.Setenv("GOWORK", workPath)
			}

			s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
			s.goBuildCacheBase = cacheBase
			_, err := s.Execute(context.Background(), map[string]any{
				"mode":         "project",
				"language":     "go",
				"project_root": projectRoot,
				"command":      []any{"go", "env", "GOOS"},
			})
			if err == nil || !strings.Contains(err.Error(), "local module is denied") {
				t.Fatalf("trusted-cache dependency error = %v", err)
			}
		})
	}
}

func TestCodeExecGoWorkDiscoveryIgnoresHostEnvironment(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	writeCodeExecTestFile(t, filepath.Join(projectRoot, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	controlledGoWork := filepath.Join(root, "go.work")
	writeCodeExecTestFile(t, controlledGoWork, "go 1.24\n\nuse ./project\n")
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing", "host.go.work"))

	got, err := discoverCodeExecHostGoWork(projectRoot)
	if err != nil {
		t.Fatalf("discover controlled project go.work: %v", err)
	}
	if resolveRealPath(got) != resolveRealPath(controlledGoWork) {
		t.Fatalf("discovered go.work = %q, want %q", got, controlledGoWork)
	}

	standalone := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(standalone, "go.mod"), "module example.com/standalone\n\ngo 1.24\n")
	got, err = discoverCodeExecHostGoWork(standalone)
	if err != nil {
		t.Fatalf("ignore unusable host GOWORK: %v", err)
	}
	if got != "" {
		t.Fatalf("standalone project inherited host GOWORK %q", got)
	}
}

func TestCodeExecGoCacheCleanupRunsAfterPolicyRejection(t *testing.T) {
	tests := []struct {
		name    string
		command any
		wantErr string
	}{
		{name: "go global C flag", command: []any{"go", "-C", ".", "test", "./..."}, wantErr: "does not accept global flags"},
		{name: "unsupported subcommand", command: []any{"env", "LANG=C", "go", "env", "GOOS"}, wantErr: "accepts only run or test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
			cleanupCalls := 0
			s.goBuildCacheCleaner = func(codeExecRun) error {
				cleanupCalls++
				return nil
			}
			s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
				return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
					return &sandbox.ExecResult{Stdout: "ok", ExitCode: 0}, nil
				}}, nil
			}
			if _, err := s.Execute(context.Background(), map[string]any{
				"mode":     "module",
				"language": "go",
				"command":  tt.command,
				"files": []any{
					map[string]any{"path": "go.mod", "content": "module example.com/cleanupfact\n\ngo 1.24\n"},
				},
			}); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Go policy rejection = %v, want %q", err, tt.wantErr)
			}
			if cleanupCalls != 1 {
				t.Fatalf("run-local Go cache cleanup calls = %d, want 1", cleanupCalls)
			}
		})
	}
}

func TestCodeExecGoToolchainDescriptorPinsBinaryAndEnvironment(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	binaryContent := []byte("fake-go-one")
	writeCodeExecTestFile(t, goBinary, string(binaryContent))
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	initialTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(goBinary, initialTime, initialTime); err != nil {
		t.Fatal(err)
	}

	var capturedConfigs []sandbox.Config
	newHelper := func() codeExecGoHelper {
		calls := 0
		return codeExecGoHelper{Factory: func(cfg sandbox.Config) (sandbox.Sandbox, error) {
			capturedConfigs = append(capturedConfigs, cfg)
			return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
				calls++
				if calls%2 == 1 {
					return &sandbox.ExecResult{Stdout: `{"GOOS":"darwin","GOARCH":"arm64","GOVERSION":"go1.26.5","GOROOT":"/opt/go","CGO_ENABLED":"1","GOEXPERIMENT":"arenas","GOAMD64":"v3","GOARM64":"v8.0"}`, ExitCode: 0}, nil
				}
				return &sandbox.ExecResult{Stdout: "compile version go1.26.5", ExitCode: 0}, nil
			}}, nil
		}}
	}

	descriptor, err := inspectCodeExecGoToolchainDescriptor(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 3600},
		goBinary,
		newHelper(),
	)
	if err != nil {
		t.Fatalf("inspect Go toolchain descriptor: %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(binaryContent))
	if descriptor.Binary != resolveRealPath(goBinary) || descriptor.BinarySHA256 != wantHash {
		t.Fatalf("Go binary descriptor = %#v", descriptor)
	}
	if descriptor.CompileVersion != "compile version go1.26.5" ||
		descriptor.GOROOT != "/opt/go" || descriptor.GOOS != "darwin" || descriptor.GOARCH != "arm64" ||
		descriptor.GOAMD64 != "v3" || descriptor.GOARM64 != "v8.0" || descriptor.GOEXPERIMENT != "arenas" {
		t.Fatalf("Go environment descriptor = %#v", descriptor)
	}
	if len(descriptor.Identity) != 64 {
		t.Fatalf("Go toolchain identity = %q", descriptor.Identity)
	}
	if len(capturedConfigs) < 2 {
		t.Fatalf("Go helper sandbox configs = %d, want at least 2", len(capturedConfigs))
	}
	selectedInstallRoot := resolveRealPath(filepath.Dir(filepath.Dir(goBinary)))
	testGOROOT := codeExecTestGOROOT(t)
	if !slices.ContainsFunc(capturedConfigs[0].ReadablePaths, func(path string) bool {
		return resolveRealPath(path) == selectedInstallRoot
	}) {
		t.Fatalf("initial helper readable paths do not contain selected Go install root %q: %v", selectedInstallRoot, capturedConfigs[0].ReadablePaths)
	}
	if resolveRealPath(testGOROOT) != selectedInstallRoot && slices.ContainsFunc(capturedConfigs[0].ReadablePaths, func(path string) bool {
		return resolveRealPath(path) == resolveRealPath(testGOROOT)
	}) {
		t.Fatalf("initial helper exposed unrelated runtime GOROOT %q: %v", testGOROOT, capturedConfigs[0].ReadablePaths)
	}
	if !slices.ContainsFunc(capturedConfigs[1].ReadablePaths, func(path string) bool {
		return resolveRealPath(path) == resolveRealPath("/opt/go")
	}) {
		t.Fatalf("compiler helper readable paths do not contain selected GOROOT: %v", capturedConfigs[1].ReadablePaths)
	}

	changedMetadataTime := initialTime.Add(24 * time.Hour)
	if chtimesErr := os.Chtimes(goBinary, changedMetadataTime, changedMetadataTime); chtimesErr != nil {
		t.Fatal(chtimesErr)
	}
	afterMetadataChange, err := inspectCodeExecGoToolchainDescriptor(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 3600},
		goBinary,
		newHelper(),
	)
	if err != nil {
		t.Fatalf("reinspect Go toolchain descriptor: %v", err)
	}
	if afterMetadataChange.Identity != descriptor.Identity {
		t.Fatalf("metadata-only change altered identity: %q != %q", afterMetadataChange.Identity, descriptor.Identity)
	}

	replacement := []byte("fake-go-two")
	if len(replacement) != len(binaryContent) {
		t.Fatal("test fixture replacement must preserve binary size")
	}
	if writeErr := os.WriteFile(goBinary, replacement, 0700); writeErr != nil {
		t.Fatal(writeErr)
	}
	if chtimesErr := os.Chtimes(goBinary, initialTime, initialTime); chtimesErr != nil {
		t.Fatal(chtimesErr)
	}
	afterContentChange, err := inspectCodeExecGoToolchainDescriptor(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 3600},
		goBinary,
		newHelper(),
	)
	if err != nil {
		t.Fatalf("inspect replaced Go binary: %v", err)
	}
	if afterContentChange.Identity == descriptor.Identity {
		t.Fatal("same-size Go binary replacement retained the trusted identity")
	}
}

func TestCodeExecGoHelperResultErrorDoesNotLeakHostPaths(t *testing.T) {
	hostMarker := filepath.Join(t.TempDir(), "private-host-marker")
	err := codeExecGoHelperResultError("inspect Go compiler identity", &sandbox.ExecResult{
		ExitCode: 2,
		Stdout:   "go: loading " + hostMarker,
		Stderr:   "cache failed at " + hostMarker,
	})
	if err == nil {
		t.Fatal("Go helper failure returned nil")
	}
	if strings.Contains(err.Error(), hostMarker) || strings.Contains(err.Error(), "cache failed") {
		t.Fatalf("Go helper error leaked host diagnostic: %q", err)
	}
	if err.Error() != "inspect Go compiler identity: exit status 2" {
		t.Fatalf("Go helper error = %q", err)
	}
}

func TestCodeExecGoDescriptorHelperFailureDoesNotLeakFactoryError(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	hostMarker := filepath.Join(t.TempDir(), "private-factory-marker")
	_, err := inspectCodeExecGoToolchainDescriptor(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 30},
		goBinary,
		codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
			return nil, errors.New("factory secret " + hostMarker)
		}},
	)
	if err == nil {
		t.Fatal("failing descriptor helper returned nil")
	}
	if strings.Contains(err.Error(), hostMarker) || strings.Contains(err.Error(), "factory secret") {
		t.Fatalf("descriptor helper error leaked factory diagnostics: %q", err)
	}
	if err.Error() != "inspect Go environment: sandbox execution failed" {
		t.Fatalf("descriptor helper error = %q", err)
	}
}

func TestCodeExecExecutionPlanUsesPinnedGoToolchain(t *testing.T) {
	descriptor := &codeExecGoToolchainDescriptor{
		Binary:   filepath.Join(t.TempDir(), "go"),
		GOROOT:   filepath.Join(t.TempDir(), "goroot"),
		Identity: strings.Repeat("a", 64),
	}
	tests := []struct {
		name       string
		command    []string
		wantEnv    map[string]string
		wantGoTest bool
	}{
		{name: "direct", command: []string{"go", "test", "./..."}, wantGoTest: true},
		{name: "safe environment", command: []string{"env", "LANG=C", "go", "test", "./..."}, wantEnv: map[string]string{"LANG": "C"}, wantGoTest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := bindCodeExecExecutionPlanCommand(codeExecExecutionPlan{
				GoRuntime: true,
				Toolchain: descriptor,
			}, tt.command)
			if err != nil {
				t.Fatalf("build execution plan: %v", err)
			}
			if plan.Command[0] != descriptor.Binary {
				t.Fatalf("pinned command = %v, want Go binary %q", plan.Command, descriptor.Binary)
			}
			if !maps.Equal(plan.Environment, tt.wantEnv) {
				t.Fatalf("pinned environment = %v, want %v", plan.Environment, tt.wantEnv)
			}
			if plan.GoTest != tt.wantGoTest || !plan.GoRuntime || plan.Toolchain != descriptor {
				t.Fatalf("execution plan = %#v", plan)
			}
		})
	}

	run := codeExecRun{
		Scratch:  t.TempDir(),
		CacheDir: t.TempDir(),
		Plan: codeExecExecutionPlan{
			GoRuntime: true,
			Toolchain: descriptor,
		},
		Config: sandbox.Config{Network: false},
	}
	exports, err := codeExecGoBuildEnvironment(run)
	if err != nil {
		t.Fatalf("derive trusted Go build environment: %v", err)
	}
	if exports["GOROOT"] != descriptor.GOROOT {
		t.Fatalf("trusted build GOROOT = %q, want %q", exports["GOROOT"], descriptor.GOROOT)
	}
	pathEntries := filepath.SplitList(exports["PATH"])
	if len(pathEntries) == 0 || resolveRealPath(pathEntries[0]) != resolveRealPath(filepath.Dir(descriptor.Binary)) {
		t.Fatalf("trusted build PATH does not pin Go binary first: %q", exports["PATH"])
	}
}

func TestCodeExecExecutionPlanPinsTrustedStagingGoToolchain(t *testing.T) {
	workspace := t.TempDir()
	tempDir := filepath.Join(workspace, "stage-tmp")
	goBinary := filepath.Join(t.TempDir(), "selected-go", "bin", "go")
	writeCodeExecTestFile(t, goBinary, "selected-go-binary")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	selectedGOROOT := filepath.Join(t.TempDir(), "selected-goroot")
	if err := os.MkdirAll(selectedGOROOT, 0700); err != nil {
		t.Fatal(err)
	}

	var captured sandbox.Config
	var executed string
	helper := codeExecGoHelper{Factory: func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return &mockSandbox{execFn: func(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("Go staging helper context has no deadline")
			}
			executed = command.Path + " " + strings.Join(command.Args, " ")
			return &sandbox.ExecResult{Stdout: "{}", ExitCode: 0}, nil
		}}, nil
	}}
	plan := codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: newBoundCodeExecTestToolchain(
			t, goBinary, selectedGOROOT, strings.Repeat("d", 64),
		),
		Helper: helper,
	}
	runner, err := newCodeExecGoStageRunner(
		context.Background(),
		sandbox.Config{
			Workspace:      workspace,
			Timeout:        3600,
			MaxOutputBytes: 1024,
			MaxStderrBytes: 512,
		},
		workspace,
		"",
		tempDir,
		plan,
	)
	if err != nil {
		t.Fatalf("create Go staging runner: %v", err)
	}
	canonicalWorkspace, err := resolveCodeExecBoundaryPath(workspace)
	if err != nil {
		t.Fatalf("canonicalize staging workspace: %v", err)
	}
	canonicalTempDir, err := resolveCodeExecBoundaryPath(tempDir)
	if err != nil {
		t.Fatalf("canonicalize staging temp directory: %v", err)
	}
	canonicalGoBinary := resolveRealPath(goBinary)
	canonicalGOROOT := resolveRealPath(selectedGOROOT)
	if runner.Workspace != canonicalWorkspace || runner.Config.Workspace != canonicalWorkspace ||
		runner.TempDir != canonicalTempDir {
		t.Fatalf("Go staging paths are not canonical: %#v", runner)
	}
	if runner.Plan.Toolchain.Binary != canonicalGoBinary || runner.Plan.Toolchain.GOROOT != canonicalGOROOT {
		t.Fatalf("Go staging toolchain paths are not canonical: %#v", runner.Plan.Toolchain)
	}
	if _, runErr := runner.Run(workspace, "off", "env", "GOROOT"); runErr != nil {
		t.Fatalf("run Go staging helper: %v", runErr)
	}
	if !strings.Contains(executed, canonicalGoBinary) {
		t.Fatalf("Go staging command did not use canonical selected binary %q: %s", canonicalGoBinary, executed)
	}
	if captured.Workspace != canonicalWorkspace || !slices.Contains(captured.ReadablePaths, canonicalGOROOT) {
		t.Fatalf("Go staging sandbox paths are not canonical: %#v", captured)
	}
	if captured.MaxOutputBytes != 1024 || captured.MaxStderrBytes != 512 {
		t.Fatalf("Go staging raised request resource limits: %#v", captured)
	}
	if captured.Timeout != int(codeExecGoStageHardTimeout/time.Second) || captured.Network {
		t.Fatalf("Go staging helper boundary = %#v", captured)
	}

	defaultPlan := plan
	defaultPlan.stageDefaultOutput = true
	defaultPlan.stageDefaultStderr = true
	defaultRunner, err := newCodeExecGoStageRunner(
		context.Background(),
		ensureCodeExecConfigDefaults(sandbox.Config{Workspace: workspace, Timeout: 3600}),
		workspace,
		"",
		tempDir,
		defaultPlan,
	)
	if err != nil {
		t.Fatalf("create default Go staging runner: %v", err)
	}
	if _, err := defaultRunner.Run(workspace, "off", "env", "GOROOT"); err != nil {
		t.Fatalf("run default Go staging helper: %v", err)
	}
	if captured.MaxOutputBytes != codeExecGoHelperMaxOutputBytes ||
		captured.MaxStderrBytes != codeExecGoHelperMaxStderrBytes {
		t.Fatalf("Go staging internal defaults = %#v", captured)
	}
}

func TestCodeExecGoStageErrorDoesNotLeakHelperDiagnostics(t *testing.T) {
	hostMarker := filepath.Join(t.TempDir(), "private-stage-marker")
	tests := []struct {
		name   string
		result *sandbox.ExecResult
		cause  error
		want   string
	}{
		{
			name: "exit",
			result: &sandbox.ExecResult{
				ExitCode: 17,
				Stdout:   "workspace " + hostMarker,
				Stderr:   "secret " + hostMarker,
			},
			cause: errors.New("private cause " + hostMarker),
			want:  "project dependency closure: go mod edit: exit status 17",
		},
		{
			name:  "sandbox failure",
			cause: errors.New("private cause " + hostMarker),
			want:  "project dependency closure: go mod edit: sandbox execution failed",
		},
		{
			name: "storage after successful exit",
			result: &sandbox.ExecResult{
				ExitCode: 0,
				Stdout:   "workspace " + hostMarker,
				Stderr:   "secret " + hostMarker,
			},
			cause: errors.Join(sandbox.ErrStorageLimitExceeded, errors.New("private cause "+hostMarker)),
			want:  "project dependency closure: go mod edit: storage limit exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := codeExecGoStageError([]string{"mod", "edit"}, tt.result, tt.cause)
			if err == nil {
				t.Fatal("Go staging failure returned nil")
			}
			if strings.Contains(err.Error(), hostMarker) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private cause") {
				t.Fatalf("Go staging error leaked helper diagnostics: %q", err)
			}
			if err.Error() != tt.want {
				t.Fatalf("Go staging error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestCodeExecGoStageRunnerAcceptsCompletedResultWithWrapperError(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	plan := codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: newBoundCodeExecTestToolchain(
			t, goBinary, t.TempDir(), strings.Repeat("f", 64),
		),
		Helper: codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
			return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
				return &sandbox.ExecResult{Stdout: "completed", ExitCode: 0}, errors.New("post-run wrapper error")
			}}, nil
		}},
	}
	runner, err := newCodeExecGoStageRunner(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 30},
		workspace,
		"",
		filepath.Join(workspace, "tmp"),
		plan,
	)
	if err != nil {
		t.Fatalf("create Go staging runner: %v", err)
	}
	out, err := runner.Run(workspace, "off", "env", "GOROOT")
	if err != nil {
		t.Fatalf("completed Go staging result was rejected: %v", err)
	}
	if string(out) != "completed" {
		t.Fatalf("completed Go staging output = %q", out)
	}
}

func TestCodeExecGoStageVendorPrunesForeignPlatformsBeforeAcceptingStoragePostcondition(t *testing.T) {
	workspace := t.TempDir()
	vendorRoot := filepath.Join(workspace, "vendor")
	packageDir := filepath.Join(vendorRoot, "example.com", "dependency")
	eligibleName := "generated_" + runtime.GOOS + "_" + runtime.GOARCH + ".go"
	foreignOS, foreignArch := "windows", "386"
	if runtime.GOOS == foreignOS && runtime.GOARCH == foreignArch {
		foreignOS, foreignArch = "linux", "amd64"
	}
	foreignName := "generated_" + foreignOS + "_" + foreignArch + ".go"
	writeCodeExecTestFile(t, filepath.Join(packageDir, eligibleName), "package dependency\n")
	writeCodeExecTestFile(t, filepath.Join(packageDir, foreignName), "package dependency\n")

	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	plan := codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: newBoundCodeExecTestToolchain(
			t, goBinary, t.TempDir(), strings.Repeat("e", 64),
		),
		Helper: codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
			return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
				return &sandbox.ExecResult{Stdout: "vendored", ExitCode: 0}, sandbox.ErrStorageLimitExceeded
			}}, nil
		}},
	}
	runner, err := newCodeExecGoStageRunner(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 30, MaxWorkspaceBytes: 1024},
		workspace,
		"",
		filepath.Join(workspace, "tmp"),
		plan,
	)
	if err != nil {
		t.Fatalf("create Go staging runner: %v", err)
	}
	if _, runErr := runner.Run(workspace, "off", "work", "vendor"); runErr == nil ||
		!strings.Contains(runErr.Error(), "storage limit exceeded") {
		t.Fatalf("generic helper call accepted storage postcondition: %v", runErr)
	}
	out, err := runner.RunVendor(workspace, "off", vendorRoot, "work", "vendor")
	if err != nil {
		t.Fatalf("recover completed vendor operation: %v", err)
	}
	if string(out) != "vendored" {
		t.Fatalf("vendor output = %q", out)
	}
	if !fileExists(filepath.Join(packageDir, eligibleName)) {
		t.Fatal("eligible vendor source was removed")
	}
	if fileExists(filepath.Join(packageDir, foreignName)) {
		t.Fatal("foreign-platform vendor source was retained")
	}

	writeCodeExecTestFile(t, filepath.Join(packageDir, foreignName), "package dependency\n")
	runner.Config.MaxWorkspaceBytes = 1
	if _, err := runner.RunVendor(workspace, "off", vendorRoot, "work", "vendor"); err == nil ||
		!strings.Contains(err.Error(), "storage limit exceeded") {
		t.Fatalf("vendor operation exceeded the persistent hard limit without rejection: %v", err)
	}
}

func TestCodeExecForeignPlatformGoFile(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		targetOS   string
		targetArch string
		want       bool
	}{
		{name: "generic", file: "driver.go", targetOS: "linux", targetArch: "amd64"},
		{name: "custom tag filename", file: "driver_enterprise.go", targetOS: "linux", targetArch: "amd64"},
		{name: "matching os", file: "driver_linux.go", targetOS: "linux", targetArch: "amd64"},
		{name: "foreign os", file: "driver_windows.go", targetOS: "linux", targetArch: "amd64", want: true},
		{name: "matching pair test", file: "driver_linux_amd64_test.go", targetOS: "linux", targetArch: "amd64"},
		{name: "foreign arch", file: "driver_linux_arm64.go", targetOS: "linux", targetArch: "amd64", want: true},
		{name: "foreign pair", file: "driver_darwin_amd64.go", targetOS: "linux", targetArch: "amd64", want: true},
		{name: "android linux alias", file: "driver_linux.go", targetOS: "android", targetArch: "arm64"},
		{name: "non go", file: "driver_windows.c", targetOS: "linux", targetArch: "amd64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeExecForeignPlatformGoFile(tt.file, tt.targetOS, tt.targetArch); got != tt.want {
				t.Fatalf("foreign platform classification for %q = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func TestEnsureCodeExecGoBuildCacheSeedUsesPlanHelperWithoutDiagnosticsLeak(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	cacheBase := filepath.Join(codeExecTestGoBuildCacheBase, "plan-helper-"+newCodeExecRunID())
	t.Cleanup(func() { _ = os.RemoveAll(cacheBase) })
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	hostMarker := filepath.Join(t.TempDir(), "private-seed-marker")
	helperCalls := 0
	plan := codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: newBoundCodeExecTestToolchain(
			t, goBinary, t.TempDir(), strings.Repeat("e", 64),
		),
		Helper: codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
			helperCalls++
			return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
				return &sandbox.ExecResult{
					ExitCode: 23,
					Stdout:   "workspace " + hostMarker,
					Stderr:   "secret " + hostMarker,
				}, nil
			}}, nil
		}},
	}
	run := codeExecRun{
		ID:        "seed-helper-leak",
		Base:      base,
		Workspace: workspace,
		Scratch:   workspace,
		CacheDir:  filepath.Join(workspace, "cache"),
		Plan:      plan,
		Config:    ensureCodeExecConfigDefaults(sandbox.Config{Workspace: workspace, Timeout: 30}),
	}
	_, _, err := ensureCodeExecGoBuildCacheSeed(context.Background(), run, cacheBase)
	if err == nil {
		t.Fatal("failing seed helper returned nil")
	}
	if helperCalls != 1 {
		t.Fatalf("execution-plan helper calls = %d, want 1; error = %v", helperCalls, err)
	}
	if strings.Contains(err.Error(), hostMarker) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Go seed helper error leaked diagnostics: %q", err)
	}
	if err.Error() != "prepare trusted Go build cache: exit status 23" {
		t.Fatalf("Go seed helper error = %q", err)
	}
}

func TestEnsureCodeExecGoBuildCacheSeedRecoversDamagedGenerations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, seedRoot, identity string)
	}{
		{
			name: "incomplete generation",
			setup: func(t *testing.T, seedRoot, _ string) {
				writeCodeExecTestFile(t, filepath.Join(seedRoot, "go-build", "stale"), "stale")
			},
		},
		{
			name: "corrupted generation",
			setup: func(t *testing.T, seedRoot, identity string) {
				writeCodeExecTestFile(t, filepath.Join(seedRoot, ".complete"), identity)
				entry := filepath.Join(seedRoot, "go-build", "entry")
				writeCodeExecTestFile(t, entry, "trusted")
				if err := writeCodeExecGoBuildCacheManifest(seedRoot, identity); err != nil {
					t.Fatal(err)
				}
				writeCodeExecTestFile(t, entry, "changed")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := fmt.Sprintf("%064x", sha256.Sum256([]byte(t.Name())))
			cacheBase := filepath.Join(codeExecTestGoBuildCacheBase, "recover-"+newCodeExecRunID())
			t.Cleanup(func() { _ = os.RemoveAll(cacheBase) })
			seedRoot := filepath.Join(cacheBase, identity)
			tt.setup(t, seedRoot, identity)
			staleBuild := filepath.Join(cacheBase, ".building-"+identity[:12]+"-stale")
			writeCodeExecTestFile(t, filepath.Join(staleBuild, "partial"), "stale")

			base := t.TempDir()
			workspace := filepath.Join(base, "workspace")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			goBinary := filepath.Join(t.TempDir(), "go")
			writeCodeExecTestFile(t, goBinary, "fake-go")
			if err := os.Chmod(goBinary, 0700); err != nil {
				t.Fatal(err)
			}
			helperCalls := 0
			run := codeExecRun{
				ID:        "recover-seed",
				Base:      base,
				Workspace: workspace,
				Scratch:   workspace,
				CacheDir:  filepath.Join(workspace, "cache"),
				Plan: codeExecExecutionPlan{
					GoRuntime: true,
					Toolchain: newBoundCodeExecTestToolchain(
						t, goBinary, t.TempDir(), identity,
					),
					Helper: codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
						helperCalls++
						return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
							return &sandbox.ExecResult{ExitCode: 0}, nil
						}}, nil
					}},
				},
				Config: ensureCodeExecConfigDefaults(sandbox.Config{Workspace: workspace, Timeout: 30}),
			}

			seed, available, err := ensureCodeExecGoBuildCacheSeed(context.Background(), run, cacheBase)
			if err != nil || !available {
				t.Fatalf("recover damaged seed: available=%v error=%v", available, err)
			}
			if helperCalls != 1 {
				t.Fatalf("seed rebuild helper calls = %d, want 1", helperCalls)
			}
			if _, valid, err := codeExecValidGoBuildCacheSeed(filepath.Dir(seed), identity); err != nil || !valid {
				t.Fatalf("rebuilt seed is invalid: valid=%v error=%v", valid, err)
			}
			if _, err := os.Lstat(filepath.Join(seed, "stale")); !os.IsNotExist(err) {
				t.Fatalf("stale generation content survived rebuild: %v", err)
			}
			if _, err := os.Lstat(staleBuild); !os.IsNotExist(err) {
				t.Fatalf("stale build generation survived recovery: %v", err)
			}
		})
	}
}

func TestCodeExecStagingFailureCleansOnlyOwnedRunDirectories(t *testing.T) {
	projectRoot := t.TempDir()
	sentinel := filepath.Join(projectRoot, "sentinel")
	writeCodeExecTestFile(t, sentinel, "preserve")
	workspace := t.TempDir()
	scratchBase := t.TempDir()
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: workspace, Timeout: 30})
	s.scratchBase = scratchBase
	s.projectStager = func(
		_ context.Context,
		_ string,
		stageRoot string,
		_ codeExecExecutionPlan,
		_ *FileAccessBroker,
		_ sandbox.Config,
	) (string, string, bool, error) {
		writeCodeExecTestFile(t, filepath.Join(stageRoot, "partial"), "partial")
		return "", "", false, errors.New("forced staging failure")
	}

	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "python",
		"project_root": projectRoot,
		"command":      []any{"python3", "main.py"},
	})
	if err == nil || !strings.Contains(err.Error(), "forced staging failure") {
		t.Fatalf("staging failure = %v", err)
	}
	if data, readErr := os.ReadFile(sentinel); readErr != nil || string(data) != "preserve" {
		t.Fatalf("cleanup changed user project sentinel: %q, %v", data, readErr)
	}
	for _, parent := range []string{filepath.Join(workspace, "runs"), filepath.Join(scratchBase, "hexclaw-sandbox-runs")} {
		entries, readErr := os.ReadDir(parent)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("staging failure left owned run directories under %q: %v", parent, entries)
		}
	}
}

func TestCodeExecStageTreeExcludesGeneratedNativeArtifacts(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "staged")
	writeCodeExecTestFile(t, filepath.Join(source, "main.go"), "package main\n")
	writeCodeExecTestFile(t, filepath.Join(source, "script.sh"), "#!/bin/sh\nexit 0\n")
	writeCodeExecTestFile(t, filepath.Join(source, "app"), "\xcf\xfa\xed\xfecompiled")
	writeCodeExecTestFile(t, filepath.Join(source, "package.test"), "\xcf\xfa\xed\xfecompiled")
	writeCodeExecTestFile(t, filepath.Join(source, "bin", "tool"), "\xcf\xfa\xed\xfecompiled")
	for _, path := range []string{filepath.Join(source, "script.sh"), filepath.Join(source, "app")} {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}

	err := copyCodeExecStageTree(
		context.Background(),
		source,
		destination,
		nil,
		&codeExecStageCopyBudget{Max: 1024 * 1024},
	)
	if err != nil {
		t.Fatalf("stage source tree: %v", err)
	}
	for _, path := range []string{"main.go", "script.sh"} {
		if _, err := os.Stat(filepath.Join(destination, path)); err != nil {
			t.Fatalf("required source %q was not staged: %v", path, err)
		}
	}
	for _, path := range []string{"app", "package.test", filepath.Join("bin", "tool")} {
		if _, err := os.Lstat(filepath.Join(destination, path)); !os.IsNotExist(err) {
			t.Fatalf("generated native artifact %q was staged: %v", path, err)
		}
	}
}

func TestCodeExecStageTreeHonorsCanceledContext(t *testing.T) {
	source := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(source, "source.txt"), "source")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := stageCodeExecProject(
		ctx,
		source,
		filepath.Join(t.TempDir(), "stage"),
		codeExecExecutionPlan{},
		nil,
		ensureCodeExecConfigDefaults(sandbox.Config{Workspace: t.TempDir()}),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stage cancellation error = %v, want context canceled", err)
	}
}

func TestCodeExecStageTraversalBudgetRejectsZeroByteFileFlood(t *testing.T) {
	source := t.TempDir()
	for index := 0; index < 5; index++ {
		writeCodeExecTestFile(t, filepath.Join(source, fmt.Sprintf("empty-%02d", index)), "")
	}
	readCalls := 0
	traversal := newCodeExecTraversalBudget(1024, func() { readCalls++ })
	traversal.Limits.MaxFiles = 3
	err := copyCodeExecStageTree(
		context.Background(),
		source,
		filepath.Join(t.TempDir(), "stage"),
		nil,
		&codeExecStageCopyBudget{Max: 1024, Traversal: traversal},
	)
	if err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("zero-byte stage flood error = %v, want file limit rejection", err)
	}
	if readCalls != 1 {
		t.Fatalf("zero-byte stage flood used %d ReadDir batches, want one bounded batch", readCalls)
	}
}

func TestCodeExecStageTraversalBudgetRejectsDeepTree(t *testing.T) {
	source := t.TempDir()
	deep := source
	for _, name := range []string{"one", "two", "three", "four"} {
		deep = filepath.Join(deep, name)
		if err := os.MkdirAll(deep, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeCodeExecTestFile(t, filepath.Join(deep, "payload.txt"), "payload")
	traversal := newCodeExecTraversalBudget(1024, nil)
	traversal.Limits.MaxDepth = 2
	err := copyCodeExecStageTree(
		context.Background(),
		source,
		filepath.Join(t.TempDir(), "stage"),
		nil,
		&codeExecStageCopyBudget{Max: 1024, Traversal: traversal},
	)
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("deep stage tree error = %v, want depth limit rejection", err)
	}
}

func TestCodeExecArtifactTraversalUsesBoundedReadDirBatches(t *testing.T) {
	scratch := t.TempDir()
	artifactDir := filepath.Join(scratch, "artifacts")
	if err := os.MkdirAll(artifactDir, 0700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < codeExecTraversalReadBatch+1; index++ {
		writeCodeExecTestFile(t, filepath.Join(artifactDir, fmt.Sprintf("empty-%03d", index)), "")
	}
	readCalls := 0
	budget := newCodeExecTraversalBudget(1024, func() { readCalls++ })
	budget.Limits.MaxFiles = codeExecTraversalReadBatch + 2
	budget.Limits.MaxEntries = codeExecTraversalReadBatch + 2
	artifacts, err := collectCodeExecArtifactsWithTraversal(
		context.Background(),
		scratch,
		artifactDir,
		1024,
		budget,
	)
	if err != nil {
		t.Fatalf("collect batched artifacts: %v", err)
	}
	if len(artifacts) != codeExecTraversalReadBatch+1 || readCalls < 2 {
		t.Fatalf("batched artifact traversal: artifacts=%d reads=%d", len(artifacts), readCalls)
	}
}

func TestCodeExecArtifactTraversalBudgetRejectsTotalBytes(t *testing.T) {
	scratch := t.TempDir()
	artifactDir := filepath.Join(scratch, "artifacts")
	writeCodeExecTestFile(t, filepath.Join(artifactDir, "first.txt"), "123456")
	writeCodeExecTestFile(t, filepath.Join(artifactDir, "second.txt"), "abcdef")
	budget := newCodeExecTraversalBudget(10, nil)
	_, err := collectCodeExecArtifactsWithTraversal(
		context.Background(),
		scratch,
		artifactDir,
		8,
		budget,
	)
	if err == nil || !strings.Contains(err.Error(), "total byte limit") {
		t.Fatalf("artifact total byte error = %v, want traversal budget rejection", err)
	}
}

func TestCodeExecArtifactTraversalChecksContextBeforeReadDir(t *testing.T) {
	scratch := t.TempDir()
	artifactDir := filepath.Join(scratch, "artifacts")
	if err := os.MkdirAll(artifactDir, 0700); err != nil {
		t.Fatal(err)
	}
	readCalls := 0
	budget := newCodeExecTraversalBudget(1024, func() { readCalls++ })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := collectCodeExecArtifactsWithTraversal(ctx, scratch, artifactDir, 1024, budget)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled artifact traversal error = %v, want context canceled", err)
	}
	if readCalls != 0 {
		t.Fatalf("canceled artifact traversal performed %d ReadDir calls, want 0", readCalls)
	}
}

func TestCodeExecStageTreeRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links may require elevated Windows privileges")
	}
	source := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(source, "target.txt"), "target")
	if err := os.Symlink("target.txt", filepath.Join(source, "link.txt")); err != nil {
		t.Fatalf("create stage symlink: %v", err)
	}
	err := copyCodeExecStageTree(
		context.Background(),
		source,
		filepath.Join(t.TempDir(), "stage"),
		nil,
		&codeExecStageCopyBudget{Max: 1024},
	)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("stage symlink error = %v, want fail-closed rejection", err)
	}
}

func TestCodeExecStageTreeRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	outside := filepath.Join(root, "outside.txt")
	writeCodeExecTestFile(t, outside, "outside")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(source, "linked.txt")); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	err := copyCodeExecStageTree(
		context.Background(),
		source,
		filepath.Join(root, "stage"),
		nil,
		&codeExecStageCopyBudget{Max: 1024},
	)
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("stage hard-link error = %v, want fail-closed rejection", err)
	}
}

func TestCodeExecStageRegularFileRejectsGrowthBeforeOpen(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(sourceDir, "source.txt"), "before")
	sourceRoot, _, err := openCodeExecRootNoFollow(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	destinationRoot, _, err := openCodeExecRootNoFollow(destinationDir)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationRoot.Close()
	before, err := sourceRoot.Lstat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	file, err := sourceRoot.OpenFile("source.txt", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := file.WriteString("-after"); writeErr != nil {
		_ = file.Close()
		t.Fatal(writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	err = copyCodeExecStageRegularFile(
		context.Background(),
		sourceRoot,
		"source.txt",
		before,
		destinationRoot,
		"source.txt",
		&codeExecStageCopyBudget{Max: 1024},
	)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("stage growth error = %v, want snapshot drift rejection", err)
	}
}

func TestCodeExecStageRegularFileRejectsIdentityReplacement(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(sourceDir, "source.txt"), "original")
	sourceRoot, _, err := openCodeExecRootNoFollow(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	destinationRoot, _, err := openCodeExecRootNoFollow(destinationDir)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationRoot.Close()
	before, err := sourceRoot.Lstat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if renameErr := sourceRoot.Rename("source.txt", "original.txt"); renameErr != nil {
		t.Fatal(renameErr)
	}
	if writeErr := sourceRoot.WriteFile("source.txt", []byte("replacement"), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}

	err = copyCodeExecStageRegularFile(
		context.Background(),
		sourceRoot,
		"source.txt",
		before,
		destinationRoot,
		"source.txt",
		&codeExecStageCopyBudget{Max: 1024},
	)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("stage replacement error = %v, want identity drift rejection", err)
	}
}

func TestCodeExecArtifactRejectsGrowthBeforeOpen(t *testing.T) {
	artifactDir := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(artifactDir, "artifact.txt"), "before")
	root, _, err := openCodeExecRootNoFollow(artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Lstat("artifact.txt")
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile("artifact.txt", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := file.WriteString("-after"); writeErr != nil {
		_ = file.Close()
		t.Fatal(writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	_, _, err = hashCodeExecArtifact(context.Background(), root, "artifact.txt", before, 1024)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("artifact growth error = %v, want snapshot drift rejection", err)
	}
}

func TestCodeExecRequestTimeoutCannotExpandConfiguredLimit(t *testing.T) {
	configured := 5
	run, err := prepareCodeExecRun(
		context.Background(),
		sandbox.Config{Workspace: t.TempDir(), Timeout: configured},
		codeExecRequest{Mode: "module", Timeout: configured + 60},
		nil,
		"",
		nil,
		codeExecExecutionPlan{},
	)
	if err != nil {
		t.Fatalf("prepare timeout fixture: %v", err)
	}
	if run.Config.Timeout != configured {
		t.Fatalf("run timeout = %d, want configured maximum %d", run.Config.Timeout, configured)
	}
}

func TestCodeExecRunCreationFailureCleansPreviouslyOwnedRoot(t *testing.T) {
	projectRoot := t.TempDir()
	workspace := t.TempDir()
	blockedScratchParent := filepath.Join(t.TempDir(), "blocked")
	writeCodeExecTestFile(t, blockedScratchParent, "not-a-directory")
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: workspace, Timeout: 30})
	s.scratchBase = filepath.Join(blockedScratchParent, "nested")

	_, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"language":     "python",
		"project_root": projectRoot,
		"command":      []any{"python3", "main.py"},
	})
	if err == nil {
		t.Fatal("blocked scratch parent did not fail run creation")
	}
	entries, readErr := os.ReadDir(filepath.Join(workspace, "runs"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial run creation left owned root: %v", entries)
	}
}

func TestCodeExecGoToolchainDescriptorRejectsBinaryReplacementBeforeExecution(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go-one")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	helper := codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			calls++
			if calls == 1 {
				return &sandbox.ExecResult{Stdout: fmt.Sprintf(
					`{"GOOS":%q,"GOARCH":%q,"GOVERSION":"go-test","GOROOT":%q,"CGO_ENABLED":"0"}`,
					runtime.GOOS,
					runtime.GOARCH,
					filepath.Dir(filepath.Dir(goBinary)),
				), ExitCode: 0}, nil
			}
			return &sandbox.ExecResult{Stdout: "compile version go-test", ExitCode: 0}, nil
		}}, nil
	}}
	descriptor, err := inspectCodeExecGoToolchainDescriptor(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 30},
		goBinary,
		helper,
	)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(goBinary), "replacement")
	writeCodeExecTestFile(t, replacement, "fake-go-two")
	if chmodErr := os.Chmod(replacement, 0700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if runtime.GOOS == "windows" {
		if removeErr := os.Remove(goBinary); removeErr != nil {
			t.Fatal(removeErr)
		}
	}
	if renameErr := os.Rename(replacement, goBinary); renameErr != nil {
		t.Fatal(renameErr)
	}
	if verifyErr := verifyCodeExecGoToolchainDescriptor(descriptor); verifyErr == nil {
		t.Fatal("replaced Go binary retained execution-plan binding")
	}
	execCalls := 0
	_, err = runCodeExecPlannedCommand(
		context.Background(),
		&mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			execCalls++
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}},
		codeExecRun{Plan: codeExecExecutionPlan{GoRuntime: true, Toolchain: &descriptor}},
		[]string{goBinary, "env", "GOROOT"},
	)
	if err == nil || execCalls != 0 {
		t.Fatalf("replaced Go binary reached final sandbox: calls=%d error=%v", execCalls, err)
	}
}

func TestHashCodeExecRegularFileNoFollowRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	linked := filepath.Join(root, "linked")
	writeCodeExecTestFile(t, source, "trusted")
	if err := os.Link(source, linked); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	if _, err := hashCodeExecRegularFileNoFollow(linked); err == nil {
		t.Fatal("hard-linked executable passed no-follow hashing")
	}
}

func TestCodeExecCanonicalRuntimeExecutableReturnsBoundRealPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link creation may require elevated Windows privileges")
	}
	root := t.TempDir()
	target := filepath.Join(root, "real-go")
	link := filepath.Join(root, "go")
	writeCodeExecTestFile(t, target, "fake-go")
	if err := os.Chmod(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := codeExecCanonicalRuntimeExecutable(link)
	if err != nil {
		t.Fatalf("resolve runtime executable: %v", err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical runtime executable = %q, want %q", got, want)
	}
}

func TestCodeExecValidGoBuildCacheSeedRequiresManifest(t *testing.T) {
	seedRoot := t.TempDir()
	identity := strings.Repeat("b", 64)
	writeCodeExecTestFile(t, filepath.Join(seedRoot, ".complete"), identity)
	writeCodeExecTestFile(t, filepath.Join(seedRoot, "go-build", "ab", "abcdef-a"), "trusted")

	_, valid, err := codeExecValidGoBuildCacheSeed(seedRoot, identity)
	if err != nil {
		t.Fatalf("validate incomplete seed: %v", err)
	}
	if valid {
		t.Fatal("trusted Go cache seed without a manifest was accepted")
	}
}

func TestCodeExecGoBuildCacheManifestRejectsTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "same-size content replacement",
			mutate: func(t *testing.T, seedRoot string) {
				t.Helper()
				writeCodeExecTestFile(t, filepath.Join(seedRoot, "go-build", "ab", "abcdef-a"), "untrust")
			},
		},
		{
			name: "permission replacement",
			mutate: func(t *testing.T, seedRoot string) {
				t.Helper()
				if err := os.Chmod(filepath.Join(seedRoot, "go-build", "ab", "abcdef-a"), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "undeclared file",
			mutate: func(t *testing.T, seedRoot string) {
				t.Helper()
				writeCodeExecTestFile(t, filepath.Join(seedRoot, "go-build", "ab", "extra-a"), "extra")
			},
		},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name   string
			mutate func(*testing.T, string)
		}{
			name: "symlink replacement",
			mutate: func(t *testing.T, seedRoot string) {
				t.Helper()
				path := filepath.Join(seedRoot, "go-build", "ab", "abcdef-a")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
					t.Fatal(err)
				}
			},
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seedRoot, identity := newCodeExecManifestSeed(t)
			tt.mutate(t, seedRoot)
			_, valid, err := codeExecValidGoBuildCacheSeed(seedRoot, identity)
			if err == nil && valid {
				t.Fatal("tampered trusted Go cache seed was accepted")
			}
		})
	}
}

func TestCodeExecGoBuildCacheMetadataRejectsLinkedFiles(t *testing.T) {
	tests := []struct {
		name   string
		target string
		link   func(oldname, newname string) error
	}{
		{name: "hard-linked marker", target: ".complete", link: os.Link},
		{name: "hard-linked manifest", target: codeExecGoBuildCacheManifestName, link: os.Link},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests,
			struct {
				name   string
				target string
				link   func(oldname, newname string) error
			}{name: "symbolic-linked marker", target: ".complete", link: os.Symlink},
			struct {
				name   string
				target string
				link   func(oldname, newname string) error
			}{name: "symbolic-linked manifest", target: codeExecGoBuildCacheManifestName, link: os.Symlink},
		)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seedRoot, identity := newCodeExecManifestSeed(t)
			target := filepath.Join(seedRoot, tt.target)
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), filepath.Base(target))
			writeCodeExecTestFile(t, external, string(data))
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := tt.link(external, target); err != nil {
				t.Skipf("linked metadata fixture is unavailable: %v", err)
			}
			if _, valid, err := codeExecValidGoBuildCacheSeed(seedRoot, identity); err == nil || valid {
				t.Fatalf("linked seed metadata passed validation: valid=%v error=%v", valid, err)
			}
		})
	}
}

func TestCodeExecGoBuildCacheManifestRecordsIntegrityMetadata(t *testing.T) {
	seedRoot, _ := newCodeExecManifestSeed(t)
	data, err := os.ReadFile(filepath.Join(seedRoot, codeExecGoBuildCacheManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
			Mode   uint32 `json:"mode"`
			Owner  string `json:"owner"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("manifest file count = %d, want 1", len(manifest.Files))
	}
	entry := manifest.Files[0]
	if entry.Path != filepath.ToSlash(filepath.Join("ab", "abcdef-a")) || len(entry.SHA256) != 64 ||
		entry.Size != int64(len("trusted")) || entry.Mode == 0 || entry.Owner == "" {
		t.Fatalf("manifest entry = %#v", entry)
	}
}

func TestCodeExecGoBuildCacheCopyRevalidatesManifestAtUse(t *testing.T) {
	seedRoot, _ := newCodeExecManifestSeed(t)
	source := filepath.Join(seedRoot, "go-build")
	writeCodeExecTestFile(t, filepath.Join(source, "ab", "abcdef-a"), "untrust")
	destination := filepath.Join(t.TempDir(), "go-build")

	err := copyCodeExecGoBuildCacheSeed(context.Background(), source, destination, 1024)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("tampered seed copy error = %v", err)
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("tampered seed copy left destination behind: %v", statErr)
	}
}

func newCodeExecManifestSeed(t *testing.T) (string, string) {
	t.Helper()
	seedRoot, _, _, identity := newCodeExecManifestSeedWithContent(t, "trusted", 0600)
	return seedRoot, identity
}

func newCodeExecManifestSeedWithContent(
	t *testing.T,
	content string,
	mode os.FileMode,
) (string, string, string, string) {
	t.Helper()
	seedRoot := t.TempDir()
	identity := strings.Repeat("c", 64)
	writeCodeExecTestFile(t, filepath.Join(seedRoot, ".complete"), identity)
	entry := filepath.Join(seedRoot, "go-build", "ab", "abcdef-a")
	writeCodeExecTestFile(t, entry, content)
	if err := os.Chmod(entry, mode); err != nil {
		t.Fatal(err)
	}
	if err := writeCodeExecGoBuildCacheManifest(seedRoot, identity); err != nil {
		t.Fatalf("write trusted Go cache manifest: %v", err)
	}
	return seedRoot, filepath.Join(seedRoot, "go-build"), entry, identity
}

func TestCodeExecGoBuildCacheSeedCreatesIndependentWritableCopy(t *testing.T) {
	_, seed, seedEntry, _ := newCodeExecManifestSeedWithContent(t, "trusted", 0400)
	destination := filepath.Join(t.TempDir(), "go-build")
	if err := copyCodeExecGoBuildCacheSeed(context.Background(), seed, destination, 1024); err != nil {
		t.Fatalf("copy trusted Go build cache seed: %v", err)
	}
	copiedEntry := filepath.Join(destination, "ab", "abcdef-a")
	if err := os.WriteFile(copiedEntry, []byte("private"), 0600); err != nil {
		t.Fatalf("run-local cache copy must be writable: %v", err)
	}
	seedContent, err := os.ReadFile(seedEntry)
	if err != nil {
		t.Fatal(err)
	}
	if string(seedContent) != "trusted" {
		t.Fatalf("run-local cache write changed trusted seed: %q", seedContent)
	}
}

func TestCodeExecGoBuildCacheSeedRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated Windows privileges")
	}
	_, seed, seedEntry, _ := newCodeExecManifestSeedWithContent(t, "secret", 0600)
	target := filepath.Join(t.TempDir(), "target")
	writeCodeExecTestFile(t, target, "secret")
	if err := os.Remove(seedEntry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, seedEntry); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "go-build")
	err := copyCodeExecGoBuildCacheSeed(context.Background(), seed, destination, 1024)
	if err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlink seed entry must fail closed, got %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("failed seed copy left destination behind: %v", statErr)
	}
}

func TestCodeExecGoBuildCacheSeedEnforcesWorkspaceBudget(t *testing.T) {
	_, seed, _, _ := newCodeExecManifestSeedWithContent(t, "12345", 0600)
	destination := filepath.Join(t.TempDir(), "go-build")
	err := copyCodeExecGoBuildCacheSeed(context.Background(), seed, destination, 4)
	if err == nil || !strings.Contains(err.Error(), "workspace limit") {
		t.Fatalf("oversized cache seed must fail before copy, got %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("oversized seed left destination behind: %v", statErr)
	}
}

func TestCodeExecGoBuildCacheContextUsesConfigurationAndParentDeadline(t *testing.T) {
	started := time.Now()
	configured, cancelConfigured := codeExecGoBuildCacheContext(context.Background(), 2)
	defer cancelConfigured()
	configuredDeadline, ok := configured.Deadline()
	if !ok {
		t.Fatal("configured cache context has no deadline")
	}
	if remaining := configuredDeadline.Sub(started); remaining < time.Second || remaining > 3*time.Second {
		t.Fatalf("configured cache deadline remaining = %s, want approximately 2s", remaining)
	}

	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	parentDeadline, _ := parent.Deadline()
	child, cancelChild := codeExecGoBuildCacheContext(parent, 30)
	defer cancelChild()
	childDeadline, ok := child.Deadline()
	if !ok || childDeadline.After(parentDeadline) {
		t.Fatalf("cache context deadline = %v, must not exceed parent %v", childDeadline, parentDeadline)
	}
}

func TestCodeExecGoBuildCacheSeedLockHonorsContext(t *testing.T) {
	key := filepath.Join(t.TempDir(), "seed")
	release, err := acquireCodeExecGoBuildCacheSeedLock(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = acquireCodeExecGoBuildCacheSeedLock(ctx, key)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended seed lock error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("contended seed lock ignored context deadline: %s", elapsed)
	}
}

func TestCodeExecGoBuildCacheSeedLockCoordinatesProcesses(t *testing.T) {
	const helperEnv = "HEXCLAW_CODE_EXEC_SEED_LOCK_HELPER"
	if os.Getenv(helperEnv) == "1" {
		key := os.Getenv("HEXCLAW_CODE_EXEC_SEED_LOCK_KEY")
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		release, err := acquireCodeExecGoBuildCacheSeedLock(ctx, key)
		if release != nil {
			release()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cross-process lock error = %v, want context deadline exceeded", err)
		}
		return
	}

	key := filepath.Join(t.TempDir(), "seed")
	release, err := acquireCodeExecGoBuildCacheSeedLock(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lockPath := key + ".lock"
	if info, err := os.Lstat(lockPath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("cross-process lock directory = %v, %v", info, err)
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCodeExecGoBuildCacheSeedLockCoordinatesProcesses$")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "HEXCLAW_CODE_EXEC_SEED_LOCK_KEY="+key)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock helper: %v\n%s", err, output)
	}
}

func TestCodeExecGoBuildCacheSeedLockRecoversStaleLease(t *testing.T) {
	key := filepath.Join(t.TempDir(), "seed")
	lockPath := key + ".lock"
	if err := os.Mkdir(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	writeCodeExecTestFile(t, filepath.Join(lockPath, "owner.json"), `{"pid":1,"nonce":"stale"}`)
	staleTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := acquireCodeExecGoBuildCacheSeedLock(ctx, key)
	if err != nil {
		t.Fatalf("recover stale seed lease: %v", err)
	}
	if info, err := os.Stat(lockPath); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("recovered seed lease = %v, %v", info, err)
	}
	release()
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("released seed lease remains: %v", err)
	}
}

type codeExecConditionalCancelContext struct {
	context.Context
	shouldCancel func() bool
	done         chan struct{}
	once         sync.Once
}

func (c *codeExecConditionalCancelContext) Done() <-chan struct{} {
	return c.done
}

func (c *codeExecConditionalCancelContext) Err() error {
	if c.shouldCancel() {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return c.Context.Err()
}

func TestCodeExecGoBuildCacheSeedCopyCancellationRemovesDestination(t *testing.T) {
	_, seed, _, _ := newCodeExecManifestSeedWithContent(t, strings.Repeat("x", 256*1024), 0600)
	destination := filepath.Join(t.TempDir(), "go-build")
	ctx := &codeExecConditionalCancelContext{
		Context: context.Background(),
		shouldCancel: func() bool {
			_, err := os.Lstat(destination)
			return err == nil
		},
		done: make(chan struct{}),
	}
	err := copyCodeExecGoBuildCacheSeed(ctx, seed, destination, 1024*1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache copy error = %v, want context canceled", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("canceled cache copy left destination behind: %v", err)
	}
}

func TestCodeExecGoBuildCacheWorkspaceMeasurementHonorsContext(t *testing.T) {
	workspace := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(workspace, "first"), "first")
	writeCodeExecTestFile(t, filepath.Join(workspace, "second"), "second")
	checks := 0
	ctx := &codeExecConditionalCancelContext{
		Context: context.Background(),
		shouldCancel: func() bool {
			checks++
			return checks >= 3
		},
		done: make(chan struct{}),
	}
	started := time.Now()
	_, err := codeExecDirSizeContext(ctx, workspace)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled workspace measurement error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("workspace measurement ignored context cancellation: %s", elapsed)
	}
}

func TestCodeExecGoBuildCacheDoesNotPolluteHostCache(t *testing.T) {
	hostCache := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(hostCache, "sentinel"), "unchanged")
	t.Setenv("GOCACHE", hostCache)
	run := newCodeExecGoBuildCacheTestRun(t)
	if _, err := prepareCodeExecGoBuildCache(context.Background(), run, codeExecTestGoBuildCacheBase); err != nil {
		t.Fatalf("prepare private Go build cache: %v", err)
	}
	entries, err := os.ReadDir(hostCache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("host GOCACHE was modified: %v", entries)
	}
}

func TestCodeExecGoBuildCacheFitsDefaultWorkspaceBudget(t *testing.T) {
	run := newCodeExecGoBuildCacheTestRun(t)
	if _, err := prepareCodeExecGoBuildCache(context.Background(), run, codeExecTestGoBuildCacheBase); err != nil {
		t.Fatalf("prepare private Go build cache: %v", err)
	}
	size, err := codeExecDirSizeContext(context.Background(), filepath.Join(run.CacheDir, "go-build"))
	if err != nil {
		t.Fatal(err)
	}
	budget := run.applicationBudget()
	if size >= budget.MaxWorkspaceBytes {
		t.Fatalf("trusted Go build cache seed = %d, default workspace budget = %d", size, budget.MaxWorkspaceBytes)
	}
	t.Logf(
		"trusted_go_build_cache_seed_bytes=%d default_max_workspace_bytes=%d",
		size,
		budget.MaxWorkspaceBytes,
	)
}

func TestCodeExecGoBuildCacheCleanupPreservesTrustedSeedAndRunOutputs(t *testing.T) {
	_, seed, seedEntry, _ := newCodeExecManifestSeedWithContent(t, "trusted", 0600)
	run := newCodeExecGoBuildCacheTestRun(t)
	run.ArtifactDir = filepath.Join(run.Workspace, "artifacts")
	run.ManifestPath = filepath.Join(run.Base, "manifest.json")
	if err := copyCodeExecGoBuildCacheSeed(
		context.Background(),
		seed,
		filepath.Join(run.CacheDir, "go-build"),
		run.applicationBudget().MaxWorkspaceBytes,
	); err != nil {
		t.Fatal(err)
	}
	writeCodeExecTestFile(t, filepath.Join(run.ArtifactDir, "result.txt"), "artifact")
	writeCodeExecTestFile(t, run.ManifestPath, "manifest")
	if err := cleanupCodeExecGoBuildCache(run); err != nil {
		t.Fatalf("clean run-local Go build cache: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(run.CacheDir, "go-build")); !os.IsNotExist(err) {
		t.Fatalf("run-local Go build cache still exists: %v", err)
	}
	seedContent, err := os.ReadFile(seedEntry)
	if err != nil {
		t.Fatal(err)
	}
	if string(seedContent) != "trusted" {
		t.Fatalf("cache cleanup changed trusted seed: %q", seedContent)
	}
	for _, path := range []string{filepath.Join(run.ArtifactDir, "result.txt"), run.ManifestPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cache cleanup removed run output %s: %v", path, err)
		}
	}
}

func TestCodeExecGoBuildCacheCleanupFailureEntersReport(t *testing.T) {
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   180,
	})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{Stdout: "PASS", ExitCode: 0}, nil
		}}, nil
	}
	s.goBuildCacheCleaner = func(codeExecRun) error {
		return errors.New("forced cleanup failure")
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "go",
		"files": []any{
			map[string]any{"path": "go.mod", "content": "module example.com/cleanup\n\ngo 1.24\n"},
			map[string]any{"path": "main_test.go", "content": "package cleanup\n"},
		},
	})
	if err != nil {
		t.Fatalf("execute cleanup failure probe: %v", err)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data = %T, want codeExecReport", result.Data)
	}
	if report.Status != "failed" || !strings.Contains(report.Error, "remove run-local Go build cache") ||
		!strings.Contains(report.Error, "forced cleanup failure") {
		t.Fatalf("cleanup failure missing from report: %#v", report)
	}
}

func TestCodeExecNonGoRunDoesNotResolveGoBuildCacheBase(t *testing.T) {
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: t.TempDir(),
		Timeout:   30,
	})
	s.goBuildCacheBase = "relative-path-must-not-be-resolved"
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{Stdout: "NON_GO_OK", ExitCode: 0}, nil
		}}, nil
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "snippet",
		"language": "python",
		"code":     "print('NON_GO_OK')",
	})
	if err != nil {
		t.Fatalf("non-Go execution resolved Go cache base: %v", err)
	}
	if !strings.Contains(result.Content, "NON_GO_OK") {
		t.Fatalf("non-Go execution failed: %s", result.Content)
	}
}

func TestCodeExecGoBuildCacheBaseRejectsSymlinkComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated Windows privileges")
	}
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "cache-link")
	if symlinkErr := os.Symlink(target, link); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}
	run := newCodeExecGoBuildCacheTestRun(t)
	_, _, err = ensureCodeExecGoBuildCacheSeed(
		context.Background(),
		run,
		filepath.Join(link, "nested"),
	)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink cache component must fail closed, got %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "nested")); !os.IsNotExist(statErr) {
		t.Fatalf("symlink target was created or modified: %v", statErr)
	}
}

func newCodeExecGoBuildCacheTestRun(t *testing.T) codeExecRun {
	t.Helper()
	base := t.TempDir()
	workspace := filepath.Join(base, "runs", "cache-test", "work")
	cacheDir := filepath.Join(workspace, "cache")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := ensureCodeExecConfigDefaults(sandbox.Config{
		Workspace: base,
		Timeout:   30,
	})
	plan, err := newCodeExecExecutionPlan(context.Background(), cfg, true, codeExecGoHelper{})
	if err != nil {
		t.Fatalf("create Go cache test execution plan: %v", err)
	}
	if plan.Toolchain == nil {
		t.Fatal("Go cache test requires a Go toolchain")
	}
	return codeExecRun{
		ID:        "cache-test",
		Base:      base,
		Workspace: workspace,
		Scratch:   workspace,
		CacheDir:  cacheDir,
		Plan:      plan,
		Config:    cfg,
	}
}

func TestCodeExecSkill_Execute_TrustedGoBuildCacheIsDeniedFromRun(t *testing.T) {
	requireCodeExecSandbox(t)
	marker := filepath.Join(codeExecTestGoBuildCacheBase, "private-marker")
	writeCodeExecTestFile(t, marker, "trusted")
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace:     t.TempDir(),
		Timeout:       180,
		ReadablePaths: []string{codeExecTestGoBuildCacheBase},
	})
	result, err := s.Execute(context.Background(), map[string]any{
		"mode":     "module",
		"language": "go",
		"files": []any{
			map[string]any{"path": "go.mod", "content": "module example.com/cacheisolation\n\ngo 1.24\n"},
			map[string]any{"path": "cache_test.go", "content": fmt.Sprintf(`package cacheisolation

import (
	"os"
	"testing"
)

func TestTrustedCacheIsDenied(t *testing.T) {
	if _, err := os.Stat(%q); err == nil {
		t.Fatal("trusted cache is readable")
	}
	if err := os.WriteFile(%q, []byte("poisoned"), 0600); err == nil {
		t.Fatal("trusted cache is writable")
	}
}
`, marker, marker)},
		},
	})
	if err != nil {
		t.Fatalf("execute cache isolation probe: %v", err)
	}
	if !strings.Contains(result.Content, "PASS") &&
		!strings.Contains(result.Content, "ok  \texample.com/cacheisolation") {
		t.Fatalf("sandbox run accessed trusted Go build cache:\n%s", result.Content)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "trusted" {
		t.Fatalf("sandbox run changed trusted Go build cache marker: %q", content)
	}
}

func TestCodeExecSkill_Execute_OfflinePolicyDoesNotInstallDependencies(t *testing.T) {
	run := func(t *testing.T) ([]string, string) {
		t.Helper()
		var mu sync.Mutex
		var scripts []string
		calls := 0
		s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{Workspace: t.TempDir(), Timeout: 30, Network: sandbox.NetworkDisabled})
		s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
			return &mockSandbox{execFn: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				commandText := command.Path + " " + strings.Join(command.Args, " ")
				scripts = append(scripts, commandText)
				switch {
				case calls == 1:
					return &sandbox.ExecResult{Stderr: "ModuleNotFoundError: No module named 'pandas'", ExitCode: 1}, nil
				case calls == 2 && strings.Contains(commandText, "-m pip install pandas"):
					return &sandbox.ExecResult{Stdout: "installed", ExitCode: 0}, nil
				default:
					return &sandbox.ExecResult{Stdout: "P0_DEP_OK", ExitCode: 0}, nil
				}
			}}, nil
		}
		result, err := s.Execute(context.Background(), map[string]any{
			"language":  "python",
			"code":      "import pandas\nprint('P0_DEP_OK')",
			"artifacts": false,
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), scripts...), result.Content
	}

	offlineScripts, offlineOut := run(t)
	if len(offlineScripts) != 1 {
		t.Fatalf("network=false should not attempt dependency install; scripts=%d\n%s", len(offlineScripts), strings.Join(offlineScripts, "\n---\n"))
	}
	if !strings.Contains(offlineOut, `"dependency_missing":["pandas"]`) {
		t.Fatalf("network=false should still report dependency_missing:\n%s", offlineOut)
	}
	if strings.Contains(strings.Join(offlineScripts, "\n"), "pip install") {
		t.Fatalf("network=false attempted pip install:\n%s", strings.Join(offlineScripts, "\n---\n"))
	}
}

func TestCodeExecSkill_SandboxPolicyReportsNetwork(t *testing.T) {
	s := newTestCodeExecSkill(t)
	if s.SandboxPolicy().NetworkEnabled {
		t.Error("should be false initially")
	}
}

func TestCodeExecSkill_PrepareSandboxPolicy_NoChange(t *testing.T) {
	s := newTestCodeExecSkill(t)
	commitCodeExecPolicyForTest(t, s, s.SandboxPolicy())
	if s.SandboxPolicy().NetworkEnabled {
		t.Error("should still be false")
	}
}

func TestCodeExecSkill_PrepareSandboxPolicy_Toggle(t *testing.T) {
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{
		Workspace:            ws,
		Timeout:              30,
		Network:              sandbox.NetworkDisabled,
		RequiredCapabilities: sandbox.UntrustedCodeIsolationCapabilities,
	}
	s := newConfiguredTestCodeExecSkill(t, sb, cfg)
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }

	if s.SandboxPolicy().NetworkEnabled {
		t.Fatal("should start with network disabled")
	}

	commitCodeExecPolicyForTest(t, s, SandboxPolicy{NetworkEnabled: false})
	if s.SandboxPolicy().NetworkEnabled {
		t.Error("should be disabled after update")
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{NetworkEnabled: true})
	if candidate != nil || !errors.Is(err, errCodeExecHostNetworkUnsupported) {
		t.Fatalf("host network update = (%v, %v), want fail-closed rejection", candidate, err)
	}
}

func TestCodeExecSkill_PrepareSandboxPolicy_FailureKeepsPreviousState(t *testing.T) {
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: ws, Timeout: 30, Network: sandbox.NetworkDisabled}
	s := newConfiguredTestCodeExecSkill(t, sb, cfg)

	mutateCodeExecConfigForTest(s, func(cfg *sandbox.Config) { cfg.Workspace = "" })

	_, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{NetworkEnabled: false})
	if err == nil {
		t.Fatal("expected update error when rebuild sandbox fails")
	}
	if s.SandboxPolicy().NetworkEnabled {
		t.Fatal("network state changed on failed rebuild")
	}
}

func TestCodeExecSkill_PrepareSandboxPolicyDerivesCapabilitiesFromSameConfig(t *testing.T) {
	s := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace:      t.TempDir(),
		Network:        sandbox.NetworkDisabled,
		MaxMemoryBytes: 1,
	})
	var captured sandbox.Config
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return &mockSandbox{}, nil
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy: %v", err)
	}
	candidate.Commit()
	want := sandbox.UntrustedCodeIsolationCapabilities | sandbox.CapabilityMemory
	if captured.RequiredCapabilities != want {
		t.Fatalf("RequiredCapabilities = %s, want %s", captured.RequiredCapabilities, want)
	}
}

func TestCodeExecSkill_ConcurrentSafety(t *testing.T) {
	ws := t.TempDir()
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: ws, Timeout: 30, Network: sandbox.NetworkDisabled}
	s := newConfiguredTestCodeExecSkill(t, sb, cfg)
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
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

	// 同时 5 个并发完整策略切换。
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			policy := s.SandboxPolicy()
			candidate, err := s.PrepareSandboxPolicy(context.Background(), policy)
			if err != nil {
				errs <- fmt.Errorf("update: %w", err)
				return
			}
			candidate.Commit()
		}(i)
	}

	// 同时 5 个并发完整策略快照读取。
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.SandboxPolicy() // 不应 panic
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
		want []string
	}{
		{"python", []string{"pandas", "numpy"}, []string{"python3", "-m", "pip", "install", "pandas", "numpy"}},
		{"javascript", []string{"lodash"}, []string{"npm", "install", "--no-save", "lodash"}},
		{"go", []string{"pkg"}, nil},
	}
	for _, tt := range tests {
		got := buildInstallCommand(tt.lang, tt.pkgs)
		if !slices.Equal(got, tt.want) {
			t.Errorf("buildInstallCommand(%q, %v) = %v, want %v", tt.lang, tt.pkgs, got, tt.want)
		}
	}
}
