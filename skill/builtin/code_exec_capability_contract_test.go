package builtin

import (
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestRegisterAdvancedCodeExecDeclaresRequiredCapabilities(t *testing.T) {
	registry := skill.NewRegistry()
	deps := &SkillDeps{Workspace: t.TempDir()}

	RegisterAdvanced(registry, config.BuiltinConfig{CodeExec: true}, deps)

	if deps.CodeExecSkill == nil {
		t.Fatal("RegisterAdvanced did not construct CodeExecSkill with a valid capability contract")
	}
}

func TestCodeExecRequiredCapabilitiesFollowFinalResourceLimits(t *testing.T) {
	base := sandbox.UntrustedCodeIsolationCapabilities
	tests := []struct {
		name string
		cfg  sandbox.Config
		want sandbox.CapabilitySet
	}{
		{
			name: "drop stale resource capabilities",
			cfg: sandbox.Config{
				ExecutionProfile: sandbox.ExecutionProfileTrustedBuild,
				RequiredCapabilities: sandbox.CapabilityMemory |
					sandbox.CapabilityProcesses |
					sandbox.CapabilityStorage,
			},
			want: base,
		},
		{
			name: "rebuild stale resource capabilities from final limits",
			cfg: sandbox.Config{
				RequiredCapabilities: sandbox.CapabilityMemory |
					sandbox.CapabilityProcesses |
					sandbox.CapabilityStorage,
				MaxMemoryBytes: 1,
			},
			want: base | sandbox.CapabilityMemory,
		},
		{
			name: "preserve explicit non-resource capability",
			cfg:  sandbox.Config{RequiredCapabilities: sandbox.CapabilityProcessCreation},
			want: base | sandbox.CapabilityProcessCreation,
		},
		{name: "memory", cfg: sandbox.Config{MaxMemoryBytes: 1}, want: base | sandbox.CapabilityMemory},
		{name: "processes", cfg: sandbox.Config{MaxProcesses: 1}, want: base | sandbox.CapabilityProcesses},
		{name: "workspace storage", cfg: sandbox.Config{MaxWorkspaceBytes: 1}, want: base | sandbox.CapabilityStorage},
		{name: "artifact storage", cfg: sandbox.Config{MaxArtifactBytes: 1}, want: base | sandbox.CapabilityStorage},
		{
			name: "all resource limits",
			cfg: sandbox.Config{
				MaxMemoryBytes:    1,
				MaxProcesses:      1,
				MaxWorkspaceBytes: 1,
				MaxArtifactBytes:  1,
			},
			want: base | sandbox.CapabilityMemory | sandbox.CapabilityProcesses | sandbox.CapabilityStorage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := withCodeExecRequiredCapabilities(test.cfg)
			if got.ExecutionProfile != sandbox.ExecutionProfileUntrusted {
				t.Fatalf("ExecutionProfile = %s, want untrusted", got.ExecutionProfile)
			}
			if got.RequiredCapabilities != test.want {
				t.Fatalf("RequiredCapabilities = %s, want %s", got.RequiredCapabilities, test.want)
			}
		})
	}
}

func TestCodeExecTrustedBuildCapabilitiesFollowFinalResourceLimits(t *testing.T) {
	base := sandbox.TrustedBuildIsolationCapabilities
	tests := []struct {
		name string
		cfg  sandbox.Config
		want sandbox.CapabilitySet
	}{
		{
			name: "drop stale resource capabilities",
			cfg: sandbox.Config{RequiredCapabilities: sandbox.CapabilityMemory |
				sandbox.CapabilityProcesses |
				sandbox.CapabilityStorage},
			want: base,
		},
		{
			name: "memory",
			cfg:  sandbox.Config{MaxMemoryBytes: 1},
			want: base | sandbox.CapabilityMemory,
		},
		{
			name: "workspace storage",
			cfg:  sandbox.Config{MaxWorkspaceBytes: 1},
			want: base | sandbox.CapabilityStorage,
		},
		{
			name: "artifact storage",
			cfg:  sandbox.Config{MaxArtifactBytes: 1},
			want: base | sandbox.CapabilityStorage,
		},
		{
			name: "memory and storage without processes",
			cfg: sandbox.Config{
				MaxMemoryBytes:    1,
				MaxProcesses:      1,
				MaxWorkspaceBytes: 1,
				MaxArtifactBytes:  1,
			},
			want: base | sandbox.CapabilityMemory | sandbox.CapabilityStorage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := withCodeExecTrustedBuildCapabilities(test.cfg)
			if got.ExecutionProfile != sandbox.ExecutionProfileTrustedBuild {
				t.Fatalf("ExecutionProfile = %s, want trusted-build", got.ExecutionProfile)
			}
			if got.RequiredCapabilities != test.want {
				t.Fatalf("RequiredCapabilities = %s, want %s", got.RequiredCapabilities, test.want)
			}
			if got.MaxProcesses != 0 {
				t.Fatalf("MaxProcesses = %d, want 0", got.MaxProcesses)
			}
		})
	}
}

func TestCodeExecCapabilityContractAllowsSandboxConstruction(t *testing.T) {
	cfg := withCodeExecRequiredCapabilities(sandbox.Config{
		Workspace: t.TempDir(),
	})

	instance, err := sandbox.New(cfg)
	if err != nil {
		t.Fatalf("sandbox.New rejected the derived capability contract: %v", err)
	}
	t.Cleanup(func() { _ = instance.Close() })
}

func TestEnsureCodeExecConfigDefaultsDerivesCapabilitiesAfterLimits(t *testing.T) {
	cfg := ensureCodeExecConfigDefaults(sandbox.Config{Workspace: t.TempDir()})
	want := sandbox.UntrustedCodeIsolationCapabilities
	if cfg.RequiredCapabilities != want {
		t.Fatalf("RequiredCapabilities = %s, want %s", cfg.RequiredCapabilities, want)
	}
	if cfg.MaxMemoryBytes != 0 || cfg.MaxProcesses != 0 || cfg.MaxWorkspaceBytes != 0 || cfg.MaxArtifactBytes != 0 {
		t.Fatalf("default hard resource limits must remain zero: %#v", cfg)
	}
}

func TestNewCodeExecSkillDerivesCapabilitiesFromItsConfig(t *testing.T) {
	skill := NewCodeExecSkill(&mockSandbox{}, sandbox.Config{
		Workspace:      t.TempDir(),
		MaxMemoryBytes: 1,
	})
	want := sandbox.UntrustedCodeIsolationCapabilities | sandbox.CapabilityMemory
	if got := codeExecConfigForTest(skill).RequiredCapabilities; got != want {
		t.Fatalf("RequiredCapabilities = %s, want %s", got, want)
	}
}
