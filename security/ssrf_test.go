package security

import (
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
		errMsg  string
	}{
		{"public URL", "https://api.github.com/repos", false, ""},
		{"localhost blocked", "http://localhost:8080/api", true, "SSRF blocked"},
		{"127.0.0.1 blocked", "http://127.0.0.1/secret", true, "SSRF blocked"},
		{"10.x blocked", "http://10.0.0.1/internal", true, "SSRF blocked"},
		{"172.16.x blocked", "http://172.16.0.1/admin", true, "SSRF blocked"},
		{"192.168.x blocked", "http://192.168.1.1/config", true, "SSRF blocked"},
		{"169.254 blocked", "http://169.254.169.254/latest/meta-data", true, "SSRF blocked"},
		{"AWS metadata blocked", "http://169.254.169.254/", true, "SSRF blocked"},
		{"GCP metadata blocked", "http://metadata.google.internal/", true, "SSRF blocked"},
		{"empty URL", "", true, "missing host"},
		{"no host", "http://", true, "missing host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateURL(tt.url)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %q", tt.url)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tt.url, err)
			}
			if tt.wantErr && err != nil && tt.errMsg != "" {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}
