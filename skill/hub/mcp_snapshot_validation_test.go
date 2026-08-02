package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

func TestHubRefreshRejectsUnpinnedMCPBeforeFreshSeedAndGetFallsBackToEmbedded(t *testing.T) {
	cacheDir := t.TempDir()
	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "v0.0.7"}, "")
	h.SetCacheDir(cacheDir)
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "index.json":
			_, _ = w.Write([]byte(`{"version":"2.0.0","updated_at":"2031-01-01T00:00:00Z","skills":[{"name":"remote-only-skill"}]}`))
		case "mcp-registry.json":
			_, _ = w.Write([]byte(`{"version":"1.3.1","updated_at":"2026-06-23","servers":[{"name":"filesystem","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	err := h.Refresh(context.Background())
	var degraded *RefreshDegradedError
	if !errors.As(err, &degraded) || degraded.Source != "mcp-registry" {
		t.Fatalf("fresh unpinned registry error = %T %v, want mcp-registry degraded rejection", err, err)
	}
	if got := h.GetCatalog(); got != nil {
		t.Fatalf("rejected fresh candidate was published before seed: %+v", got)
	}
	if !h.lastSync.IsZero() {
		t.Fatalf("rejected fresh candidate changed lastSync: %v", h.lastSync)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "hub-catalog.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected fresh candidate wrote cache: %v", statErr)
	}

	entry, getErr := (&McpHub{inner: h}).Get("filesystem")
	if getErr != nil {
		t.Fatalf("Get must fall back to embedded pinned MCP after rejected refresh: %v", getErr)
	}
	if entry.Status != "pinned" || entry.Artifact == nil {
		t.Fatalf("fallback MCP is not the embedded pinned entry: %+v", entry)
	}

	deadline := time.Now().Add(5 * time.Second)
	for h.refreshing.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.refreshing.Load() {
		t.Fatal("background retry did not finish")
	}
	if !h.lastSync.IsZero() {
		t.Fatalf("rejected background retry changed lastSync: %v", h.lastSync)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "hub-catalog.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected background retry wrote cache: %v", statErr)
	}
}

func TestHubSeedRejectsInvalidMCPFromNewerDiskCache(t *testing.T) {
	cacheDir := t.TempDir()
	cache := []byte(`{
		"version":"future-invalid",
		"updated_at":"2099-01-01T00:00:00Z",
		"mcp_registry":{"version":"1.3.1","updated_at":"2026-06-23","sha256":"untrusted"},
		"skills":[{"name":"cache-only-unpinned","type":"mcp","command":"npx","args":["-y","example"]}]
	}`)
	if err := os.WriteFile(filepath.Join(cacheDir, "hub-catalog.json"), cache, 0o600); err != nil {
		t.Fatalf("write invalid cache: %v", err)
	}

	h := New(HubConfig{Enabled: true, RepoURL: "https://hub.test/repo", Branch: "main"}, "")
	h.SetCacheDir(cacheDir)
	h.lastAttempt = time.Now() // Keep this test on the deterministic disk→embedded seed boundary.
	h.EnsureCatalog()

	entry, err := (&McpHub{inner: h}).Get("filesystem")
	if err != nil || entry.Status != "pinned" || entry.Artifact == nil {
		t.Fatalf("invalid newer cache covered embedded pinned MCP: entry=%+v err=%v", entry, err)
	}
	for _, candidate := range h.GetCatalog().Skills {
		if candidate.Name == "cache-only-unpinned" {
			t.Fatalf("invalid cache MCP was published: %+v", candidate)
		}
	}
	if !h.lastSync.IsZero() {
		t.Fatalf("disk seed must not count as a network refresh: %v", h.lastSync)
	}
}

func TestMergeMCPRegistryRejectsInvalidExecutableMetadata(t *testing.T) {
	zeroSRI := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	tests := []struct {
		name   string
		server string
	}{
		{
			name:   "missing pinned status and artifact",
			server: `{"name":"legacy","command":"npx","args":["-y","legacy"]}`,
		},
		{
			name:   "pinned artifact does not bind exact argv",
			server: fmt.Sprintf(`{"name":"drift","status":"pinned","command":"npx","args":["-y","drift"],"artifact":{"ecosystem":"npm","package":"drift","version":"1.2.3","integrity":%q,"source_registry":"https://registry.npmjs.org"}}`, zeroSRI),
		},
		{
			name:   "quarantined entry misses reason",
			server: `{"name":"quarantined","status":"quarantined"}`,
		},
		{
			name:   "quarantined entry retains artifact",
			server: fmt.Sprintf(`{"name":"quarantined","status":"quarantined","quarantine_reason":"removed","artifact":{"ecosystem":"npm","package":"bad","version":"1.2.3","integrity":%q,"source_registry":"https://registry.npmjs.org"}}`, zeroSRI),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := Catalog{}
			err := mergeMcpRegistry(&catalog, []byte(`{"version":"2.0.0","updated_at":"2026-08-02","servers":[`+tt.server+`]}`))
			if err == nil {
				t.Fatalf("invalid MCP metadata was merged: %+v", catalog.Skills)
			}
			if len(catalog.Skills) != 0 {
				t.Fatalf("failed merge partially mutated catalog: %+v", catalog.Skills)
			}
		})
	}
}

func TestMergeMCPRegistryPreservesIdentityWithoutChangingCatalogOrderingTime(t *testing.T) {
	zeroSRI := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	body := []byte(fmt.Sprintf(`{
		"version":"2.0.0",
		"updated_at":"2099-12-31",
		"servers":[{
			"name":"filesystem",
			"status":"pinned",
			"command":"npx",
			"args":["-y","filesystem@1.2.3"],
			"artifact":{"ecosystem":"npm","package":"filesystem","version":"1.2.3","integrity":%q,"source_registry":"https://registry.npmjs.org"}
		}]
	}`, zeroSRI))
	indexUpdatedAt := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	catalog := Catalog{Version: "index-v1", UpdatedAt: indexUpdatedAt}
	if err := mergeMcpRegistry(&catalog, body); err != nil {
		t.Fatalf("merge valid MCP registry: %v", err)
	}
	if !catalog.UpdatedAt.Equal(indexUpdatedAt) {
		t.Fatalf("registry updated_at was incorrectly used as catalog ordering time: got %v want %v", catalog.UpdatedAt, indexUpdatedAt)
	}

	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal merged catalog: %v", err)
	}
	var snapshot struct {
		MCPRegistry *struct {
			Version   string `json:"version"`
			UpdatedAt string `json:"updated_at"`
			SHA256    string `json:"sha256"`
		} `json:"mcp_registry"`
	}
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatalf("decode catalog identity: %v", err)
	}
	if snapshot.MCPRegistry == nil {
		t.Fatalf("registry identity was silently dropped from catalog: %s", encoded)
	}
	wantDigest := sha256.Sum256(body)
	if snapshot.MCPRegistry.Version != "2.0.0" || snapshot.MCPRegistry.UpdatedAt != "2099-12-31" || snapshot.MCPRegistry.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("registry identity mismatch: %+v", snapshot.MCPRegistry)
	}
	if strings.Contains(snapshot.MCPRegistry.UpdatedAt, "T00:00:00") {
		t.Fatalf("registry identity was normalized into a synthetic sortable time: %+v", snapshot.MCPRegistry)
	}
}
