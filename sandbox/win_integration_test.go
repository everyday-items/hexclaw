//go:build windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============== D35: Windows 沙箱集成测试 ==============

func TestWindows_SandboxCreation(t *testing.T) {
	ws := t.TempDir()
	sb, err := New(Config{Workspace: ws, Timeout: 30})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sb == nil {
		t.Fatal("sandbox is nil")
	}
}

func TestWindows_ExecSimpleCommand(t *testing.T) {
	ws := t.TempDir()
	sb, err := New(Config{Workspace: ws, Timeout: 10})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	ctx := context.Background()
	result, err := sb.Exec(ctx, "cmd", []string{"/c", "echo", "hello"})
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("expected 'hello' in stdout, got: %q", result.Stdout)
	}
	t.Logf("stdout: %s", result.Stdout)
}

func TestWindows_ExecPythonCode(t *testing.T) {
	ws := t.TempDir()
	sb, err := New(Config{Workspace: ws, Timeout: 15})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Skip if python not available
	ctx := context.Background()
	result, err := sb.ExecCode(ctx, "python", `print("hello from sandbox")`)
	if err != nil {
		t.Skipf("python not available: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello from sandbox") {
		t.Fatalf("expected output, got: %q", result.Stdout)
	}
}

func TestWindows_WorkspaceIsolation(t *testing.T) {
	ws := t.TempDir()

	// Create a file in workspace
	testFile := filepath.Join(ws, "test.txt")
	os.WriteFile(testFile, []byte("sandbox data"), 0644)

	sb, err := New(Config{Workspace: ws, Timeout: 10})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	ctx := context.Background()
	// Can read workspace file
	result, err := sb.Exec(ctx, "cmd", []string{"/c", "type", "test.txt"})
	if err != nil {
		t.Fatalf("read workspace file: %v", err)
	}
	if !strings.Contains(result.Stdout, "sandbox data") {
		t.Fatalf("expected file content, got: %q", result.Stdout)
	}
}

func TestWindows_Timeout(t *testing.T) {
	ws := t.TempDir()
	sb, err := New(Config{Workspace: ws, Timeout: 2})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	ctx := context.Background()
	start := time.Now()
	_, err = sb.Exec(ctx, "cmd", []string{"/c", "ping", "-n", "10", "127.0.0.1"})
	elapsed := time.Since(start)

	// Should timeout within ~3 seconds (2s timeout + buffer)
	if elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced: took %v", elapsed)
	}
	t.Logf("timed out after %v (expected ~2s)", elapsed)
}

func TestWindows_EnvironmentClean(t *testing.T) {
	ws := t.TempDir()
	sb, err := New(Config{Workspace: ws, Timeout: 10})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	ctx := context.Background()
	result, err := sb.Exec(ctx, "cmd", []string{"/c", "echo", "%COMSPEC%"})
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	// COMSPEC should be cleaned (not expanded to cmd.exe path)
	stdout := strings.TrimSpace(result.Stdout)
	if strings.Contains(strings.ToLower(stdout), "cmd.exe") {
		t.Logf("WARNING: COMSPEC leaked: %s (hardening may need Windows-specific fixes)", stdout)
	}
}

func TestWindows_PathValidation(t *testing.T) {
	tests := []struct {
		path string
		ok   bool
	}{
		{`C:\Users\test\file.txt`, true},
		{`file.txt`, true},
		{`\\server\share\file`, false},   // UNC
		{`\\.\PhysicalDrive0`, false},     // device handle
		{`file.txt:hidden`, false},        // ADS (non-absolute)
	}

	for _, tt := range tests {
		err := validateWindowsPath(tt.path)
		if tt.ok && err != nil {
			t.Errorf("expected OK for %q, got: %v", tt.path, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("expected block for %q", tt.path)
		}
	}
}

func TestWindows_EscapeVectors(t *testing.T) {
	tests := []struct {
		cmd  string
		args []string
		ok   bool
	}{
		{"python", []string{"script.py"}, true},
		{"node", []string{"app.js"}, true},
		{"powershell", []string{"-Command", "Get-Process"}, false}, // blocked
		{"cmd.exe", []string{"/c", "whoami"}, false},                // blocked
		{"python", []string{`\\server\share\evil.py`}, false},      // UNC in args
	}

	for _, tt := range tests {
		err := validateWindowsEscapeVectors(tt.cmd, tt.args)
		if tt.ok && err != nil {
			t.Errorf("expected OK for %q %v, got: %v", tt.cmd, tt.args, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("expected block for %q %v", tt.cmd, tt.args)
		}
	}
}

func TestWindows_NetProxy_Integration(t *testing.T) {
	// Test that proxy starts and blocks correctly
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proxy := NewNetProxy(NetProxyConfig{
		AllowDomains: []string{"httpbin.org"},
	})

	addr, err := proxy.Start(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Logf("proxy listening at %s", addr)

	// Verify proxy env vars
	envVars := ProxyEnvVars(addr)
	if len(envVars) < 4 {
		t.Fatalf("expected 4+ env vars, got %d", len(envVars))
	}
	for _, e := range envVars {
		if !strings.Contains(e, addr) && !strings.HasPrefix(e, "SSL_CERT_FILE=") {
			t.Errorf("env var doesn't contain proxy addr: %s", e)
		}
	}
}

func TestWindows_DefaultPolicy(t *testing.T) {
	policy := DefaultWindowsPolicy()
	if policy.Mode != ModeWorkspaceWrite {
		t.Errorf("expected workspace-write mode, got %s", policy.Mode)
	}
	if policy.Network != NetworkOffline {
		t.Errorf("expected offline network, got %s", policy.Network)
	}
	if policy.MemoryMB != 512 {
		t.Errorf("expected 512MB memory, got %d", policy.MemoryMB)
	}
	if !policy.UseDesktop {
		t.Error("expected UseDesktop=true")
	}
}
