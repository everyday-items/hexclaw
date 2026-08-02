package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill/hub"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
)

func TestHubMCPInstallDoesNotExposeCatalogEntryWithoutPinnedArtifact(t *testing.T) {
	repoDir := t.TempDir()
	mp := marketplace.NewMarketplace(t.TempDir())
	writeLocalHubCatalog(
		t,
		repoDir,
		`{"version":"1.0.0","updated_at":"2026-08-02T00:00:00Z","skills":[]}`,
		`{"version":"2.0.0","servers":[{"name":"untrusted-test","type":"mcp","command":"missing-mcp-test-binary","args":[]}]}`,
		nil,
	)

	h := hub.New(hub.HubConfig{Enabled: true, RepoURL: "file://" + repoDir, Branch: "main"}, mp.Dir())
	err := h.Refresh(context.Background())
	var degraded *hub.RefreshDegradedError
	if !errors.As(err, &degraded) || degraded.Source != "mcp-registry" {
		t.Fatalf("unpinned registry refresh error = %T %v, want mcp-registry degraded rejection", err, err)
	}
	mgr := hexmcp.NewManager()
	t.Cleanup(mgr.Close)

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.mp = mp
	srv.skillHub = h
	srv.mcpMgr = mgr

	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/install", strings.NewReader(`{"source":"clawhub://untrusted-test"}`))
	w := httptest.NewRecorder()
	srv.handleInstallSkill(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("rejected Hub MCP entry must not be exposed to install routing: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := mgr.ConfiguredServerNames(); len(got) != 0 {
		t.Fatalf("unpinned Hub MCP entry reached process manager: %v", got)
	}
}
