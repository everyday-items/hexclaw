package builtin

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

func TestCodeExecCommandCapabilitiesRejectMacOSNodeProjectScripts(t *testing.T) {
	tests := []struct {
		name    string
		command []string
	}{
		{name: "npm", command: []string{"npm", "test"}},
		{name: "absolute npm", command: []string{filepath.Join(string(filepath.Separator), "opt", "bin", "npm"), "test"}},
		{name: "pnpm", command: []string{"pnpm", "test"}},
		{name: "yarn", command: []string{"yarn", "test"}},
		{name: "npx", command: []string{"npx", "vitest"}},
		{name: "corepack", command: []string{"corepack", "pnpm", "test"}},
		{name: "env", command: []string{"env", "NODE_ENV=test", "npm", "test"}},
		{name: "node test runner", command: []string{"node", "--test", "test.js"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			required, err := codeExecCommandRequiredCapabilities("darwin", test.command)
			if required != 0 {
				t.Fatalf("required capabilities = %s, want none after rejection", required)
			}
			if !errors.Is(err, sandbox.ErrRequiredCapabilitiesUnavailable) {
				t.Fatalf("error = %v, want ErrRequiredCapabilitiesUnavailable", err)
			}
			if !strings.Contains(err.Error(), "macOS Node project commands are unsupported in the untrusted sandbox because process creation is unavailable") {
				t.Fatalf("error = %q, want explicit macOS unsupported message", err)
			}
		})
	}
}

func TestCodeExecCommandCapabilitiesAllowMacOSDirectNodeFile(t *testing.T) {
	required, err := codeExecCommandRequiredCapabilities("darwin", []string{"node", "index.js"})
	if err != nil {
		t.Fatalf("direct node command returned error: %v", err)
	}
	if required != 0 {
		t.Fatalf("required capabilities = %s, want none", required)
	}
}

func TestCodeExecCommandCapabilitiesRequireProcessCreationOutsideMacOS(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			required, err := codeExecCommandRequiredCapabilities(goos, []string{"pnpm", "test"})
			if err != nil {
				t.Fatalf("command requirements returned error: %v", err)
			}
			if required != sandbox.CapabilityProcessCreation {
				t.Fatalf("required capabilities = %s, want process creation", required)
			}
		})
	}
}

func TestCodeExecCommandCapabilitiesAreAppliedBeforeSandboxCreation(t *testing.T) {
	cfg, err := withCodeExecCommandRequiredCapabilities(
		sandbox.Config{RequiredCapabilities: sandbox.CapabilityMemory},
		"linux",
		[]string{"npm", "test"},
	)
	if err != nil {
		t.Fatalf("apply command requirements returned error: %v", err)
	}
	want := sandbox.UntrustedCodeIsolationCapabilities | sandbox.CapabilityProcessCreation
	if cfg.RequiredCapabilities != want {
		t.Fatalf("required capabilities = %s, want %s", cfg.RequiredCapabilities, want)
	}
}

func TestPrepareCodeExecCommandWithCapabilitiesRejectsInferredMacOSNodeProjectTest(t *testing.T) {
	run := codeExecRun{
		Workspace: t.TempDir(),
		Config:    sandbox.Config{},
	}
	req := codeExecRequest{
		Mode:     "module",
		Language: "javascript",
		Files: []codeExecInputFile{
			{Path: "package.json", Content: `{"scripts":{"test":"node test.js"}}`},
			{Path: "test.js", Content: "console.log('test')\n"},
		},
	}
	_, _, err := prepareCodeExecCommandWithCapabilities(context.Background(), req, run, "darwin")
	if !errors.Is(err, sandbox.ErrRequiredCapabilitiesUnavailable) {
		t.Fatalf("error = %v, want inferred Node project test rejection", err)
	}
}

func TestPrepareCodeExecCommandWithCapabilitiesKeepsMacOSDirectNodeFile(t *testing.T) {
	run := codeExecRun{
		Workspace: t.TempDir(),
		Config:    sandbox.Config{},
	}
	req := codeExecRequest{
		Mode:     "module",
		Language: "javascript",
		Files: []codeExecInputFile{
			{Path: "index.js", Content: "console.log('direct')\n"},
		},
	}
	command, cfg, err := prepareCodeExecCommandWithCapabilities(context.Background(), req, run, "darwin")
	if err != nil {
		t.Fatalf("direct node preparation returned error: %v", err)
	}
	if !slices.Equal(command, []string{"node", "index.js"}) {
		t.Fatalf("command = %v, want direct node file", command)
	}
	if cfg.RequiredCapabilities != sandbox.UntrustedCodeIsolationCapabilities {
		t.Fatalf("required capabilities = %s, want untrusted isolation", cfg.RequiredCapabilities)
	}
}

func TestCodeExecMacOSDynamicNodeInstallFailsClosed(t *testing.T) {
	command := []string{"npm", "install", "--no-save", "example"}
	if err := validateCodeExecDynamicCommandCapabilities("darwin", command); !errors.Is(err, sandbox.ErrRequiredCapabilitiesUnavailable) ||
		!strings.Contains(err.Error(), "macOS Node project commands are unsupported") {
		t.Fatalf("macOS dynamic install error = %v", err)
	}
}
