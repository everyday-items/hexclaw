package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestRegisterAdvancedRejectsCodeExecHostNetwork(t *testing.T) {
	allowed := true
	registry := skill.NewRegistry()
	deps := &SkillDeps{Workspace: t.TempDir()}

	RegisterAdvanced(registry, config.BuiltinConfig{
		CodeExec: true,
		CodeExecPolicy: config.CodeExecPolicyConfig{
			Network: &allowed,
		},
	}, deps)

	if deps.CodeExecSkill != nil {
		t.Fatal("code_exec must not start when the requested network policy exposes the host network")
	}
	if _, exists := registry.Get("code_exec"); exists {
		t.Fatal("code_exec must not be registered when the requested network policy exposes the host network")
	}
}

func TestPrepareSandboxPolicyRejectsHostNetworkBeforeConstruction(t *testing.T) {
	s := NewCodeExecSkill(nil, sandbox.Config{
		Workspace: t.TempDir(),
		Network:   sandbox.NetworkDisabled,
	})
	factoryCalled := false
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		factoryCalled = true
		return &mockSandbox{}, nil
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{NetworkEnabled: true})
	if candidate != nil {
		t.Fatal("host-network policy must not produce a candidate")
	}
	if err == nil || !strings.Contains(err.Error(), "host network") {
		t.Fatalf("PrepareSandboxPolicy error = %v, want host-network rejection", err)
	}
	if factoryCalled {
		t.Fatal("host-network policy reached sandbox construction")
	}
	if s.SandboxPolicy().NetworkEnabled {
		t.Fatal("rejected host-network policy changed the active policy")
	}
}

func TestCodeExecExecuteRejectsInjectedHostNetworkBeforeConstruction(t *testing.T) {
	s := NewCodeExecSkill(nil, sandbox.Config{
		Workspace: t.TempDir(),
		Network:   sandbox.NetworkHost,
	})
	factoryCalled := false
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		factoryCalled = true
		return &mockSandbox{}, nil
	}

	result, err := s.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('unreachable')",
	})
	if result != nil {
		t.Fatalf("Execute result = %#v, want nil", result)
	}
	if err == nil || !strings.Contains(err.Error(), "host network") {
		t.Fatalf("Execute error = %v, want host-network rejection", err)
	}
	if factoryCalled {
		t.Fatal("injected host-network policy reached sandbox construction")
	}
}

func TestPrepareSandboxPolicyKeepsDefaultOfflineBehavior(t *testing.T) {
	s := NewCodeExecSkill(nil, sandbox.Config{
		Workspace: t.TempDir(),
		Network:   sandbox.NetworkDisabled,
	})
	var constructed sandbox.NetworkMode
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		constructed = cfg.Network
		return &mockSandbox{}, nil
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy offline error = %v", err)
	}
	candidate.Commit()
	if constructed != sandbox.NetworkDisabled || s.SandboxPolicy().NetworkEnabled {
		t.Fatalf("offline policy constructed=%s active=%+v", constructed, s.SandboxPolicy())
	}
}
