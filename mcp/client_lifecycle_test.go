package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/testing/mock"
)

// stubStdioConnect temporarily replaces hexagon.ConnectMCPStdio so AddServer /
// tryReconnect success paths run without spawning a real subprocess. It returns
// a cleanup that restores the original and a pointer to the live call counter.
func stubStdioConnect(t *testing.T, tools []hexagon.Tool) (cleanupCalls *int) {
	t.Helper()
	orig := hexagon.ConnectMCPStdio
	calls := 0
	hexagon.ConnectMCPStdio = func(ctx context.Context, command string, args ...string) ([]hexagon.Tool, func(), error) {
		return tools, func() { calls++ }, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdio = orig })
	return &calls
}

func fakeTool(name string) hexagon.Tool {
	return mock.NewTool(name, mock.WithToolDescription("desc-"+name))
}

// TestAddServer_RejectsEmptyName asserts the guard before any connection.
func TestAddServer_RejectsEmptyName(t *testing.T) {
	m := NewManager()
	if err := m.AddServer(context.Background(), ServerConfig{Transport: "stdio", Command: "x"}); err == nil {
		t.Error("empty name must error")
	}
}

// TestAddServer_Success drives the full success path: connect, insert into the
// servers map, and append to configs for reconnection.
func TestAddServer_Success(t *testing.T) {
	stubStdioConnect(t, []hexagon.Tool{fakeTool("read"), fakeTool("write")})
	m := NewManager()

	cfg := ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"}
	if err := m.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	names := m.ServerNames()
	if len(names) != 1 || names[0] != "fs" {
		t.Fatalf("server not registered: %v", names)
	}
	if got := len(m.Tools()); got != 2 {
		t.Errorf("want 2 tools, got %d", got)
	}
	// Config must be persisted so reconnect can find it.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.configs) != 1 || m.configs[0].Name != "fs" {
		t.Errorf("config not persisted: %v", m.configs)
	}
	if !m.configs[0].Enabled {
		t.Error("AddServer should force Enabled=true")
	}
}

// TestAddServer_ReplacesExisting verifies a same-name AddServer closes the old
// connection (cleanup invoked) and swaps in the new one without duplicate config.
func TestAddServer_ReplacesExisting(t *testing.T) {
	calls := stubStdioConnect(t, []hexagon.Tool{fakeTool("v1")})
	m := NewManager()
	cfg := ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"}

	if err := m.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("first AddServer: %v", err)
	}
	if err := m.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("second AddServer: %v", err)
	}

	if *calls != 1 {
		t.Errorf("old connection cleanup should fire exactly once, got %d", *calls)
	}
	if len(m.ServerNames()) != 1 {
		t.Errorf("replace must not duplicate server: %v", m.ServerNames())
	}
	m.mu.RLock()
	cfgCount := len(m.configs)
	m.mu.RUnlock()
	if cfgCount != 1 {
		t.Errorf("replace must not duplicate config, got %d", cfgCount)
	}
}

// TestAddServer_ConnectError surfaces connectServer failures (bad transport).
func TestAddServer_ConnectError(t *testing.T) {
	m := NewManager()
	err := m.AddServer(context.Background(), ServerConfig{Name: "bad", Transport: "grpc"})
	if err == nil {
		t.Error("unsupported transport must error")
	}
	if len(m.ServerNames()) != 0 {
		t.Error("failed AddServer must not register the server")
	}
}

// TestAddServer_AfterClose rejects additions once the manager is closed.
func TestAddServer_AfterClose(t *testing.T) {
	stubStdioConnect(t, nil)
	m := NewManager()
	m.Close()
	err := m.AddServer(context.Background(), ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"})
	if err == nil {
		t.Error("AddServer after Close must error")
	}
}

// TestRemoveServer_Success removes a connected server, invokes its cleanup, and
// prunes its config.
func TestRemoveServer_Success(t *testing.T) {
	calls := stubStdioConnect(t, []hexagon.Tool{fakeTool("t")})
	m := NewManager()
	cfg := ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"}
	if err := m.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	if err := m.RemoveServer("fs"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if *calls != 1 {
		t.Errorf("cleanup should fire on removal, got %d", *calls)
	}
	if len(m.ServerNames()) != 0 {
		t.Errorf("server should be gone: %v", m.ServerNames())
	}
	m.mu.RLock()
	cfgCount := len(m.configs)
	m.mu.RUnlock()
	if cfgCount != 0 {
		t.Errorf("config should be pruned, got %d", cfgCount)
	}
}

// TestRemoveServer_NotFound errors for unknown names.
func TestRemoveServer_NotFound(t *testing.T) {
	m := NewManager()
	if err := m.RemoveServer("nope"); err == nil {
		t.Error("removing unknown server must error")
	}
}

// TestRemoveServer_AfterClose rejects removals once closed.
func TestRemoveServer_AfterClose(t *testing.T) {
	m := NewManager()
	m.Close()
	if err := m.RemoveServer("any"); err == nil {
		t.Error("RemoveServer after Close must error")
	}
}

// TestServerStatuses reports connected flag and tool counts.
func TestServerStatuses(t *testing.T) {
	m := NewManager()
	m.mu.Lock()
	m.servers["a"] = &connectedServer{name: "a", connected: true, tools: []hexagon.Tool{fakeTool("x"), fakeTool("y")}}
	m.servers["b"] = &connectedServer{name: "b", connected: false}
	m.mu.Unlock()

	statuses := m.ServerStatuses()
	if len(statuses) != 2 {
		t.Fatalf("want 2 statuses, got %d", len(statuses))
	}
	byName := map[string]ServerStatus{}
	for _, s := range statuses {
		byName[s.Name] = s
	}
	if !byName["a"].Connected || byName["a"].ToolCount != 2 {
		t.Errorf("status a wrong: %+v", byName["a"])
	}
	if byName["b"].Connected || byName["b"].ToolCount != 0 {
		t.Errorf("status b wrong: %+v", byName["b"])
	}
}

// TestListToolDefinitions only includes tools from connected servers.
func TestListToolDefinitions(t *testing.T) {
	m := NewManager()
	m.mu.Lock()
	m.servers["live"] = &connectedServer{name: "live", connected: true, tools: []hexagon.Tool{fakeTool("read")}}
	m.servers["dead"] = &connectedServer{name: "dead", connected: false, tools: []hexagon.Tool{fakeTool("write")}}
	m.mu.Unlock()

	defs := m.ListToolDefinitions()
	if len(defs) != 1 {
		t.Fatalf("disconnected server tools must be excluded; got %d defs", len(defs))
	}
	if defs[0].Function.Name != "read" {
		t.Errorf("unexpected tool def: %s", defs[0].Function.Name)
	}
}

// TestCallTool_NotFound covers the lookup-miss branch.
func TestCallTool_NotFound(t *testing.T) {
	m := NewManager()
	if _, err := m.CallTool(context.Background(), "ghost", nil); err == nil {
		t.Error("unknown tool must error")
	}
}

// TestTryReconnect_Reconnects drives tryReconnect for a config whose server is
// marked disconnected, asserting a fresh connection replaces it.
func TestTryReconnect_Reconnects(t *testing.T) {
	stubStdioConnect(t, []hexagon.Tool{fakeTool("t")})
	m := NewManager()

	// Seed a config plus a stale (disconnected) server entry.
	m.mu.Lock()
	m.configs = []ServerConfig{{Name: "fs", Transport: "stdio", Command: "stub", Enabled: true}}
	m.servers["fs"] = &connectedServer{name: "fs", connected: false}
	m.mu.Unlock()

	m.tryReconnect()

	m.mu.RLock()
	defer m.mu.RUnlock()
	srv := m.servers["fs"]
	if srv == nil || !srv.connected {
		t.Fatalf("server should be reconnected and connected: %+v", srv)
	}
	if len(srv.tools) != 1 {
		t.Errorf("reconnected server should carry tools, got %d", len(srv.tools))
	}
}

// TestTryReconnect_SkipsHealthyAndDisabled ensures connected servers and
// disabled configs are left untouched (no stub call needed for either).
func TestTryReconnect_SkipsHealthyAndDisabled(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	orig := hexagon.ConnectMCPStdio
	hexagon.ConnectMCPStdio = func(ctx context.Context, command string, args ...string) ([]hexagon.Tool, func(), error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdio = orig })

	m := NewManager()
	m.mu.Lock()
	m.configs = []ServerConfig{
		{Name: "healthy", Transport: "stdio", Command: "stub", Enabled: true},
		{Name: "off", Transport: "stdio", Command: "stub", Enabled: false},
	}
	m.servers["healthy"] = &connectedServer{name: "healthy", connected: true}
	m.mu.Unlock()

	m.tryReconnect()

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("healthy + disabled configs should trigger no reconnect, got %d calls", calls)
	}
}

// TestConnectServer_SSEMissingEndpoint and friends cover validation branches in
// connectServer not already exercised by client_test.go (streamable + http).
func TestConnectServer_TransportValidation(t *testing.T) {
	m := NewManager()
	cases := []struct {
		name string
		cfg  ServerConfig
	}{
		{"streamable missing endpoint", ServerConfig{Name: "s", Transport: "streamable"}},
		{"http missing endpoint", ServerConfig{Name: "s", Transport: "http"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.connectServer(context.Background(), tc.cfg); err == nil {
				t.Error("missing endpoint must error")
			}
		})
	}
}

// TestRegisterServer_Success covers the enabled RegisterServer path end-to-end,
// including the replace-existing branch.
func TestRegisterServer_Success(t *testing.T) {
	calls := stubStdioConnect(t, []hexagon.Tool{fakeTool("t")})
	m := NewManager()
	cfg := ServerConfig{Name: "fs", Transport: "stdio", Command: "stub", Enabled: true}

	if err := m.RegisterServer(context.Background(), cfg); err != nil {
		t.Fatalf("RegisterServer: %v", err)
	}
	if len(m.ServerNames()) != 1 {
		t.Fatalf("server not registered: %v", m.ServerNames())
	}
	// Re-register same name -> old closed, no duplicate.
	if err := m.RegisterServer(context.Background(), cfg); err != nil {
		t.Fatalf("re-RegisterServer: %v", err)
	}
	if *calls != 1 {
		t.Errorf("old connection should be closed once on replace, got %d", *calls)
	}
	if len(m.ServerNames()) != 1 {
		t.Errorf("replace must not duplicate, got %v", m.ServerNames())
	}
}

// TestRegisterServer_ConnectError surfaces connect failures.
func TestRegisterServer_ConnectError(t *testing.T) {
	m := NewManager()
	if err := m.RegisterServer(context.Background(), ServerConfig{Name: "bad", Transport: "grpc", Enabled: true}); err == nil {
		t.Error("bad transport must error")
	}
}

// TestRemoveConfig_RemovesMatch exercises the unexported helper directly.
func TestRemoveConfig_RemovesMatch(t *testing.T) {
	in := []ServerConfig{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	out := removeConfig(in, "b")
	if len(out) != 2 {
		t.Fatalf("want 2 configs, got %d", len(out))
	}
	for _, c := range out {
		if c.Name == "b" {
			t.Error("b should be removed")
		}
	}
}

// TestAppendOrReplaceConfig_Append covers the append branch.
func TestAppendOrReplaceConfig_Append(t *testing.T) {
	in := []ServerConfig{{Name: "a"}}
	out := appendOrReplaceConfig(in, ServerConfig{Name: "b", Enabled: true})
	if len(out) != 2 {
		t.Fatalf("append should grow list, got %d", len(out))
	}
}
