package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill/hub"
)

type staticMCPInstallerCatalog struct {
	entry hub.McpServerMeta
}

func (c staticMCPInstallerCatalog) Search(string) []hub.McpServerMeta {
	return []hub.McpServerMeta{c.entry}
}

func (c staticMCPInstallerCatalog) Get(name string) (*hub.McpServerMeta, error) {
	if name != c.entry.Name {
		return nil, fmt.Errorf("MCP server %q not found", name)
	}
	entry := c.entry
	return &entry, nil
}

func TestMCPInstallerRejectsHubEntryWithoutPinnedArtifact(t *testing.T) {
	h := staticMCPInstallerCatalog{entry: hub.McpServerMeta{
		Name:    "untrusted-test",
		Command: "missing-mcp-test-binary",
	}}
	mgr := mcp.NewManager()
	t.Cleanup(mgr.Close)
	installer := newMcpInstallerSkill(h, mgr, nil)

	_, err := installer.Execute(context.Background(), map[string]any{"action": "install", "keyword": "untrusted-test"})
	if err == nil || !strings.Contains(err.Error(), "pinned artifact") {
		t.Fatalf("expected fail-closed pinned artifact error, got %v", err)
	}
	if got := mgr.ConfiguredServerNames(); len(got) != 0 {
		t.Fatalf("unpinned entry reached MCP manager: %v", got)
	}
}
