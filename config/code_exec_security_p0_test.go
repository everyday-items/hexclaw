package config

import "testing"

func TestCodeExecSecurityP0_NetworkIsDenyByDefault(t *testing.T) {
	var zero CodeExecPolicyConfig
	if zero.CodeExecNetworkAllowed() {
		t.Fatal("zero-value code_exec policy must deny network access")
	}

	if DefaultConfig().Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed() {
		t.Fatal("default config must deny code_exec network access")
	}

	allowed := true
	if !(CodeExecPolicyConfig{Network: &allowed}).CodeExecNetworkAllowed() {
		t.Fatal("an explicit network=true grant must remain supported")
	}
}
