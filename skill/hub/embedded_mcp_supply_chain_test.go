package hub

import "testing"

func TestEmbeddedMCPRegistryContainsOnlyPinnedOrQuarantinedEntries(t *testing.T) {
	catalog := embeddedCatalog()
	seen := 0
	for _, entry := range catalog.Skills {
		if entry.Type != "mcp" {
			continue
		}
		seen++
		if entry.Status == "quarantined" {
			if entry.QuarantineReason == "" || entry.Artifact != nil {
				t.Fatalf("quarantined entry %q has invalid provenance state", entry.Name)
			}
			continue
		}
		if _, err := ValidatePinnedMCPServer(MCPServerMetaFromSkill(entry)); err != nil {
			t.Fatalf("embedded MCP entry %q is executable without valid pin: %v", entry.Name, err)
		}
	}
	if seen == 0 {
		t.Fatal("embedded MCP registry is empty")
	}
}
