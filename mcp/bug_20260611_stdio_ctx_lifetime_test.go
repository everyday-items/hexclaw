package mcp

// BUG-20260611 (review finding #1): REFUTED by this behavioral test, kept as a
// regression guard.
//
// A static-analysis review flagged this as Critical: hexagon
// ConnectStdioServerV2 builds the command with exec.CommandContext(ctx, ...),
// and connectServer was handed a short timeout ctx the caller cancelled right
// after connect — so the inference was "cancel SIGKILLs the subprocess".
//
// Empirically it does NOT: the MCP go-sdk CommandTransport.Connect (cmd.go:29)
// ignores its ctx argument and tears the process down only via Close() (closing
// stdin), so the subprocess outlives caller ctx cancellation. This test locks
// that behavior so a future SDK/hexagon change that re-couples process lifetime
// to the connect ctx fails here loudly.

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestBug20260611_StdioServerSurvivesCallerCtxCancel(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available, skipping real stdio MCP test")
	}

	mgr := NewManager()
	defer mgr.Close()

	// Mirror AddServer/reconnect: a short-lived ctx the caller cancels right
	// after connect returns.
	ctx, cancel := context.WithCancel(context.Background())
	count, err := mgr.Connect(ctx, []ServerConfig{
		{
			Name:      "memory",
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-memory"},
			Enabled:   true,
		},
	})
	if err != nil || count == 0 {
		t.Skipf("connect failed (MCP server not reachable): %v (tools=%d)", err, count)
	}

	// The bug: this cancel kills the stdio subprocess.
	cancel()
	time.Sleep(500 * time.Millisecond)

	// After fix: the server is still connected and its tools callable.
	tools := mgr.ListToolInfos()
	if len(tools) == 0 {
		t.Fatalf("[BUG-20260611] stdio server died after caller ctx cancel — no tools remain")
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	// Any tool call proves the subprocess is alive; an unknown-arg error is
	// still a live response (process answered), whereas a killed process
	// yields a transport/EOF error.
	_, callErr := mgr.CallTool(callCtx, tools[0].Name, map[string]any{})
	if callErr != nil && isTransportDead(callErr) {
		t.Fatalf("[BUG-20260611] stdio subprocess was killed by caller ctx cancel: %v", callErr)
	}
}

// isTransportDead reports whether the error indicates the subprocess/transport
// is gone (as opposed to a normal tool-level error like bad arguments).
func isTransportDead(err error) bool {
	msg := err.Error()
	for _, needle := range []string{"EOF", "broken pipe", "closed", "killed", "signal", "process", "connection"} {
		if containsFold(msg, needle) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
