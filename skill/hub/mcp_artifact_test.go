package hub

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidatePinnedMCPServer(t *testing.T) {
	npmIntegrity := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	valid := McpServerMeta{
		Name:    "filesystem",
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-filesystem@2026.7.10", "/tmp"},
		Status:  "pinned",
		Artifact: &MCPArtifact{
			Ecosystem:      "npm",
			Package:        "@modelcontextprotocol/server-filesystem",
			Version:        "2026.7.10",
			Integrity:      npmIntegrity,
			SourceRegistry: "https://registry.npmjs.org",
		},
	}

	tests := []struct {
		name    string
		mutate  func(*McpServerMeta)
		wantErr string
	}{
		{name: "valid exact npm artifact"},
		{name: "missing status", mutate: func(m *McpServerMeta) { m.Status = "" }, wantErr: "pinned artifact"},
		{name: "quarantined", mutate: func(m *McpServerMeta) { m.Status = "quarantined"; m.QuarantineReason = "package removed" }, wantErr: "已隔离"},
		{name: "argument does not bind version", mutate: func(m *McpServerMeta) { m.Args[1] = "@modelcontextprotocol/server-filesystem" }, wantErr: "未绑定 artifact"},
		{name: "extra npm package", mutate: func(m *McpServerMeta) { m.Args = append(m.Args, "--package=attacker@1.0.0") }, wantErr: "额外 package"},
		{name: "invalid integrity", mutate: func(m *McpServerMeta) { m.Artifact.Integrity = "sha512-not-base64" }, wantErr: "sha512 SRI"},
		{name: "alternate registry", mutate: func(m *McpServerMeta) { m.Artifact.SourceRegistry = "https://packages.example" }, wantErr: "source registry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := valid
			meta.Args = append([]string(nil), valid.Args...)
			artifact := *valid.Artifact
			meta.Artifact = &artifact
			if tt.mutate != nil {
				tt.mutate(&meta)
			}
			got, err := ValidatePinnedMCPServer(meta)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Name() != meta.Name || strings.Join(got.Args(), "\x00") != strings.Join(meta.Args, "\x00") {
					t.Fatalf("validated projection drift: name=%q args=%v", got.Name(), got.Args())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}
