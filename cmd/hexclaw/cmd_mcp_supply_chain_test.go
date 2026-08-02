package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill/hub"
)

func TestValidateMCPInstallEntryRejectsUnpinnedCatalogEntry(t *testing.T) {
	entry := &hub.McpServerMeta{
		Name:    "untrusted",
		Command: "npx",
		Args:    []string{"-y", "example@1.2.3"},
	}

	_, err := validateMCPInstallEntry(entry)
	if err == nil || !strings.Contains(err.Error(), "pinned artifact") {
		t.Fatalf("CLI install must fail closed before persistence, got %v", err)
	}
}

func TestValidateMCPInstallEntryReturnsDetachedValidatedProjection(t *testing.T) {
	entry := &hub.McpServerMeta{
		Name:    "example",
		Command: "npx",
		Args:    []string{"-y", "example@1.2.3"},
		Status:  "pinned",
		Artifact: &hub.MCPArtifact{
			Ecosystem:      "npm",
			Package:        "example",
			Version:        "1.2.3",
			Integrity:      "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
			SourceRegistry: "https://registry.npmjs.org",
		},
	}

	validated, err := validateMCPInstallEntry(entry)
	if err != nil {
		t.Fatalf("validate pinned entry: %v", err)
	}
	entry.Args[1] = "attacker@9.9.9"
	if got := validated.Args()[1]; got != "example@1.2.3" {
		t.Fatalf("validated argv aliased mutable catalog input: %q", got)
	}
}
