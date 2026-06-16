package api

// BUG-20260616: validateExternalProviderBaseURL returned nil early for
// type=ollama, skipping all SSRF validation. An attacker could point an
// "ollama" provider at an internal/metadata URL and the server would request
// it. Ollama is local, so its base_url must be loopback — not an arbitrary
// internal address.

import "testing"

func TestBug20260616_OllamaBaseURLLocalOnly(t *testing.T) {
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://192.168.0.5:11434",                // internal LAN
		"http://10.1.2.3:11434",
	} {
		if err := validateExternalProviderBaseURL("ollama", u); err == nil {
			t.Errorf("ollama base_url %q (non-loopback) must be rejected as SSRF", u)
		}
	}
	for _, u := range []string{"http://localhost:11434", "http://127.0.0.1:11434", ""} {
		if err := validateExternalProviderBaseURL("ollama", u); err != nil {
			t.Errorf("legit local ollama %q must pass, got %v", u, err)
		}
	}
}
