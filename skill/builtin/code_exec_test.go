package builtin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

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

func newTestCodeExecSkill() *CodeExecSkill {
	sb := &mockSandbox{}
	cfg := sandbox.Config{Workspace: "/tmp/test-ws", Timeout: 30, Network: true}
	return NewCodeExecSkill(sb, cfg)
}

func TestCodeExecSkill_Meta(t *testing.T) {
	s := newTestCodeExecSkill()
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

func TestCodeExecSkill_Execute_Success(t *testing.T) {
	s := newTestCodeExecSkill()
	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('hello')",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "mock output" {
		t.Errorf("got %q, want %q", result.Content, "mock output")
	}
}

func TestCodeExecSkill_Execute_MissingArgs(t *testing.T) {
	s := newTestCodeExecSkill()
	_, err := s.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
	if !strings.Contains(err.Error(), "language and code are required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCodeExecSkill_Execute_NonZeroExit(t *testing.T) {
	sb := &mockSandbox{
		execCodeFn: func(_ context.Context, _, _ string) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{Stdout: "", Stderr: "syntax error", ExitCode: 1}, nil
		},
	}
	s := NewCodeExecSkill(sb, sandbox.Config{Workspace: "/tmp/ws", Timeout: 30})
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
	if !strings.Contains(result.Content, "syntax error") {
		t.Errorf("should contain stderr, got: %s", result.Content)
	}
}

func TestCodeExecSkill_NetworkEnabled(t *testing.T) {
	s := newTestCodeExecSkill() // Network: true
	if !s.NetworkEnabled() {
		t.Error("should be true initially")
	}
}

func TestCodeExecSkill_UpdateNetwork_NoChange(t *testing.T) {
	s := newTestCodeExecSkill() // Network: true
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
		{"python", []string{"pandas", "numpy"}, "pip install pandas numpy"},
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
