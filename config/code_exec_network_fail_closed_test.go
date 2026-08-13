package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsCodeExecHostNetwork(t *testing.T) {
	cfg := DefaultConfig()
	enabled := true
	cfg.Skill.Builtin.CodeExecPolicy.Network = &enabled

	err := cfg.Validate()
	if err == nil {
		t.Fatal("code_exec host-network configuration must be rejected")
	}
	for _, want := range []string{
		"skill.builtin.code_exec_policy.network",
		"host network",
		"destination filtering",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate error = %q, want %q", err, want)
		}
	}
}
