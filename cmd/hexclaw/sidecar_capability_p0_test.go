package main

import "testing"

func TestSidecarCapabilityP0_EnvironmentContract(t *testing.T) {
	t.Run("missing is compatibility mode", func(t *testing.T) {
		t.Setenv("HEXCLAW_SIDECAR_CAPABILITY_TOKEN", "")
		token, err := sidecarCapabilityTokenFromEnv()
		if err != nil || token != "" {
			t.Fatalf("missing token=(%q,%v), want empty compatibility mode", token, err)
		}
	})

	t.Run("short token fails closed", func(t *testing.T) {
		t.Setenv("HEXCLAW_SIDECAR_CAPABILITY_TOKEN", "predictable")
		if _, err := sidecarCapabilityTokenFromEnv(); err == nil {
			t.Fatal("short sidecar capability was accepted")
		}
	})

	t.Run("high entropy token is trimmed", func(t *testing.T) {
		const want = "R6P_w3j9hQ2fJ7vNs8kM4xZc1dT5yUa0B"
		t.Setenv("HEXCLAW_SIDECAR_CAPABILITY_TOKEN", "  "+want+"\n")
		token, err := sidecarCapabilityTokenFromEnv()
		if err != nil || token != want {
			t.Fatalf("token=(%q,%v), want validated token", token, err)
		}
	})
}
