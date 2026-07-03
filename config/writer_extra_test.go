package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWriter_AppendMCPServer verifies a new MCP server is appended and persisted,
// defaulting Enabled to true.
func TestWriter_AppendMCPServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)

	if err := w.AppendMCPServer("fs", "stdio", "npx", []string{"-y", "server"}, nil, ""); err != nil {
		t.Fatalf("AppendMCPServer failed: %v", err)
	}

	// readConfig path: file now exists and should round-trip the server.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config invalid: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("expected 1 MCP server, got %d", len(cfg.MCP.Servers))
	}
	s := cfg.MCP.Servers[0]
	if s.Name != "fs" || s.Transport != "stdio" || s.Command != "npx" {
		t.Errorf("server fields mismatch: %+v", s)
	}
	if !s.Enabled {
		t.Error("appended server should default to Enabled=true")
	}
}

// TestWriter_AppendMCPServer_DuplicateRejected verifies appending a duplicate name
// returns an error and does not add a second entry.
func TestWriter_AppendMCPServer_DuplicateRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)

	if err := w.AppendMCPServer("dup", "sse", "", nil, nil, "https://example.com"); err != nil {
		t.Fatalf("first append failed: %v", err)
	}
	err := w.AppendMCPServer("dup", "sse", "", nil, nil, "https://example.com")
	if err == nil {
		t.Fatal("duplicate name should be rejected")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists': %v", err)
	}
}

// TestWriter_RemoveMCPServer verifies an existing server is removed and persisted.
func TestWriter_RemoveMCPServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)

	_ = w.AppendMCPServer("a", "stdio", "cmd", nil, nil, "")
	_ = w.AppendMCPServer("b", "stdio", "cmd", nil, nil, "")

	if err := w.RemoveMCPServer("a"); err != nil {
		t.Fatalf("RemoveMCPServer failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	var cfg Config
	_ = yaml.Unmarshal(data, &cfg)
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != "b" {
		t.Errorf("expected only server 'b' to remain, got %+v", cfg.MCP.Servers)
	}
}

// TestWriter_RemoveMCPServer_NotFound verifies removing an unknown server errors.
func TestWriter_RemoveMCPServer_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)

	err := w.RemoveMCPServer("ghost")
	if err == nil {
		t.Fatal("removing nonexistent server should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

// TestWriter_AppendStartsFromDefaultsWhenFileMissing verifies readConfig falls back
// to DefaultConfig() when the target file does not yet exist.
func TestWriter_AppendStartsFromDefaultsWhenFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	w := NewWriter(path)

	if err := w.AppendMCPServer("x", "stdio", "cmd", nil, nil, ""); err != nil {
		t.Fatalf("append onto missing file should succeed via defaults: %v", err)
	}
	data, _ := os.ReadFile(path)
	var cfg Config
	_ = yaml.Unmarshal(data, &cfg)
	// Default storage driver should be present, proving we began from DefaultConfig.
	if cfg.Storage.Driver != "sqlite" {
		t.Errorf("expected defaults baseline (driver=sqlite), got %q", cfg.Storage.Driver)
	}
}

// TestWriter_ReadConfig_InvalidYAML verifies a corrupt config file surfaces a parse
// error rather than silently resetting.
func TestWriter_ReadConfig_InvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("server:\n\t bad: : :"), 0600); err != nil {
		t.Fatal(err)
	}
	w := NewWriter(path)

	if err := w.RemoveMCPServer("anything"); err == nil {
		t.Fatal("expected parse error on corrupt config")
	}
}

// TestAtomicWriteFile verifies content and permissions are written atomically.
func TestAtomicWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.txt")
	want := []byte("hexclaw-atomic-content")

	if err := atomicWriteFile(path, want, 0640); err != nil {
		t.Fatalf("atomicWriteFile failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", got, want)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0640 {
		t.Errorf("perm = %o, want 0640", perm)
	}
}

// TestAtomicWriteFile_OverwritesExisting verifies the rename replaces prior content.
func TestAtomicWriteFile_OverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("new"), 0600); err != nil {
		t.Fatalf("atomicWriteFile failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("content = %q, want 'new'", got)
	}
}

// TestCodeExecPolicy_NilDefaultsAreFunctional verifies the nil-pointer policy
// accessors default to function-first values documented on the type.
func TestCodeExecPolicy_NilDefaultsAreFunctional(t *testing.T) {
	var p CodeExecPolicyConfig // both pointers nil
	if p.CodeExecRequiresApproval() {
		t.Error("nil RequireApproval should default to false (function-first)")
	}
	if !p.CodeExecNetworkAllowed() {
		t.Error("nil Network should default to true (network allowed)")
	}
}

// TestCodeExecPolicy_ExplicitValuesRespected verifies explicit pointers override defaults.
func TestCodeExecPolicy_ExplicitValuesRespected(t *testing.T) {
	tests := []struct {
		name          string
		approval, net bool
	}{
		{"approval_false_net_false", false, false},
		{"approval_true_net_false", true, false},
		{"approval_false_net_true", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			approval, net := tt.approval, tt.net
			p := CodeExecPolicyConfig{RequireApproval: &approval, Network: &net}
			if got := p.CodeExecRequiresApproval(); got != tt.approval {
				t.Errorf("RequiresApproval = %v, want %v", got, tt.approval)
			}
			if got := p.CodeExecNetworkAllowed(); got != tt.net {
				t.Errorf("NetworkAllowed = %v, want %v", got, tt.net)
			}
		})
	}
}

// TestPlatformsConfig_UnmarshalSingleObjectBackCompat verifies the legacy single-object
// platform format (feishu: {...}) is wrapped into a one-element slice.
func TestPlatformsConfig_UnmarshalSingleObjectBackCompat(t *testing.T) {
	yamlSrc := `
feishu:
  name: "legacy"
  enabled: true
  app_id: "cli_xxx"
web:
  enabled: true
`
	var p PlatformsConfig
	if err := yaml.Unmarshal([]byte(yamlSrc), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(p.Feishu) != 1 {
		t.Fatalf("expected 1 feishu bot from single-object form, got %d", len(p.Feishu))
	}
	if p.Feishu[0].Name != "legacy" || !p.Feishu[0].Enabled {
		t.Errorf("feishu fields mismatch: %+v", p.Feishu[0])
	}
	if !p.Web.Enabled {
		t.Error("web.enabled should decode to true")
	}
}

// TestPlatformsConfig_UnmarshalSliceForm verifies the new array form decodes into
// multiple bot instances across several platforms.
func TestPlatformsConfig_UnmarshalSliceForm(t *testing.T) {
	yamlSrc := `
telegram:
  - name: "bot1"
    enabled: true
    token: "t1"
  - name: "bot2"
    enabled: false
    token: "t2"
slack:
  - name: "s1"
    token: "x"
    signing_secret: "y"
`
	var p PlatformsConfig
	if err := yaml.Unmarshal([]byte(yamlSrc), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(p.Telegram) != 2 {
		t.Fatalf("expected 2 telegram bots, got %d", len(p.Telegram))
	}
	if p.Telegram[0].Token != "t1" || p.Telegram[1].Name != "bot2" {
		t.Errorf("telegram decode mismatch: %+v", p.Telegram)
	}
	if len(p.Slack) != 1 || p.Slack[0].SigningSecret != "y" {
		t.Errorf("slack decode mismatch: %+v", p.Slack)
	}
}

// TestPlatformsConfig_UnmarshalNonMappingIgnored verifies a non-mapping node is a no-op.
func TestPlatformsConfig_UnmarshalNonMappingIgnored(t *testing.T) {
	var p PlatformsConfig
	// A scalar at the platforms position must not error nor populate anything.
	if err := yaml.Unmarshal([]byte(`"just-a-string"`), &p); err != nil {
		t.Fatalf("non-mapping node should be ignored, got error: %v", err)
	}
	if len(p.Feishu) != 0 || len(p.Telegram) != 0 {
		t.Error("non-mapping node should not populate any platform")
	}
}

// TestPlatformsConfig_MarshalJSON verifies the custom marshaller emits valid JSON
// with the expected keys.
func TestPlatformsConfig_MarshalJSON(t *testing.T) {
	p := PlatformsConfig{
		Web:      WebConfig{Enabled: true},
		Telegram: []TelegramConfig{{Name: "b", Enabled: true, Token: "t"}},
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("output should be valid JSON: %v", err)
	}
	if _, ok := round["Web"]; !ok {
		t.Errorf("expected Web key in JSON, got: %s", data)
	}
}
