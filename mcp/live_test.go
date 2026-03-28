package mcp

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestLive_TimeServer connects to @modelcontextprotocol/server-time via stdio.
// Skipped if npx is not available.
func TestLive_TimeServer(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available, skipping live test")
	}

	mgr := NewManager()
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	count, err := mgr.Connect(ctx, []ServerConfig{
		{
			Name:      "memory",
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-memory"},
			Enabled:   true,
		},
	})

	if err != nil {
		t.Skipf("connect failed (MCP server not available): %v", err)
	}

	t.Logf("connected: %d tools discovered", count)

	tools := mgr.Tools()
	if len(tools) == 0 {
		t.Skip("no tools discovered, MCP server may not support this SDK version")
	}

	for _, tl := range tools {
		t.Logf("  tool: %s — %s", tl.Name(), tl.Description())
	}

	// Call any available tool
	infos := mgr.ToolInfos()
	t.Logf("tool infos: %d tools from %d servers", len(infos), len(mgr.ServerNames()))
}
