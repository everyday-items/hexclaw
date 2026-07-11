package mcp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/tool"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/testing/mock"
)

func seedRestartableServer(m *Manager, name string, cleanup func()) {
	m.mu.Lock()
	m.configs = []ServerConfig{{Name: name, Enabled: true, Transport: "stdio", Command: "stub"}}
	m.servers[name] = &connectedServer{name: name, connected: true, cleanup: cleanup}
	m.mu.Unlock()
}

func TestRestartServer_AfterCloseRejectsWithoutConnecting(t *testing.T) {
	m := NewManager()
	seedRestartableServer(m, "fs", func() {})
	m.Close()
	var connects atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		connects.Add(1)
		return []hexagon.Tool{fakeTool("new")}, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })

	err := m.RestartServer(context.Background(), "fs")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") && !strings.Contains(err.Error(), "关闭") {
		t.Fatalf("restart after Close must fail closed, got %v", err)
	}
	if got := connects.Load(); got != 0 {
		t.Fatalf("closed manager must not start a connector, got %d", got)
	}
}

func TestConnect_AfterCloseRejectsWithoutConnecting(t *testing.T) {
	m := NewManager()
	m.Close()
	var connects atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		connects.Add(1)
		return nil, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })
	_, err := m.Connect(context.Background(), []ServerConfig{{Name: "fs", Enabled: true, Transport: "stdio", Command: "stub"}})
	if err == nil {
		t.Fatal("Connect after Close must fail")
	}
	if got := connects.Load(); got != 0 {
		t.Fatalf("closed manager started %d connectors", got)
	}
}

func TestConnect_CloseDuringConnectorCannotResurrect(t *testing.T) {
	m := NewManager()
	started := make(chan struct{})
	release := make(chan struct{})
	var candidateClosed atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		close(started)
		<-release
		return []hexagon.Tool{fakeTool("candidate")}, func() { candidateClosed.Add(1) }, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })
	errCh := make(chan error, 1)
	go func() {
		_, err := m.Connect(context.Background(), []ServerConfig{{Name: "fs", Enabled: true, Transport: "stdio", Command: "stub"}})
		errCh <- err
	}()
	<-started
	m.Close()
	close(release)
	if err := <-errCh; err == nil {
		t.Fatal("Connect racing Close must fail")
	}
	if len(m.ServerNames()) != 0 {
		t.Fatalf("Connect resurrected server after Close: %v", m.ServerNames())
	}
	if got := candidateClosed.Load(); got != 1 {
		t.Fatalf("stale connected candidate close count=%d want 1", got)
	}
}

func TestRestartServer_CloseDuringConnectCannotResurrect(t *testing.T) {
	m := NewManager()
	seedRestartableServer(m, "fs", func() {})
	started := make(chan struct{})
	release := make(chan struct{})
	var candidateClosed atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		close(started)
		<-release
		return []hexagon.Tool{fakeTool("candidate")}, func() { candidateClosed.Add(1) }, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })

	errCh := make(chan error, 1)
	go func() { errCh <- m.RestartServer(context.Background(), "fs") }()
	<-started
	m.Close()
	close(release)
	err := <-errCh
	if err == nil {
		t.Fatal("restart racing Close must be rejected")
	}
	if got := len(m.ServerNames()); got != 0 {
		t.Fatalf("restart resurrected %d servers after Close", got)
	}
	if got := candidateClosed.Load(); got != 1 {
		t.Fatalf("stale candidate must be closed exactly once, got %d", got)
	}
}

func TestRestartServer_RemoveDuringConnectCannotResurrect(t *testing.T) {
	m := NewManager()
	seedRestartableServer(m, "fs", func() {})
	started := make(chan struct{})
	release := make(chan struct{})
	var candidateClosed atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		close(started)
		<-release
		return []hexagon.Tool{fakeTool("candidate")}, func() { candidateClosed.Add(1) }, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })

	errCh := make(chan error, 1)
	go func() { errCh <- m.RestartServer(context.Background(), "fs") }()
	<-started
	if err := m.RemoveServer("fs"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	close(release)
	if err := <-errCh; err == nil {
		t.Fatal("restart racing RemoveServer must be rejected")
	}
	if got := len(m.ServerNames()); got != 0 {
		t.Fatalf("restart resurrected removed server: %v", m.ServerNames())
	}
	if got := candidateClosed.Load(); got != 1 {
		t.Fatalf("stale candidate must be closed exactly once, got %d", got)
	}
}

func TestRestartServer_ConcurrentLatestInvocationWins(t *testing.T) {
	m := NewManager()
	var oldClosed atomic.Int32
	seedRestartableServer(m, "fs", func() { oldClosed.Add(1) })
	started := make(chan int, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var calls atomic.Int32
	var candidateClosed [2]atomic.Int32
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		idx := int(calls.Add(1)) - 1
		started <- idx
		<-releases[idx]
		return []hexagon.Tool{fakeTool(fmt.Sprintf("candidate-%d", idx+1))}, func() { candidateClosed[idx].Add(1) }, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })

	errCh := make(chan error, 2)
	go func() { errCh <- m.RestartServer(context.Background(), "fs") }()
	if got := <-started; got != 0 {
		t.Fatalf("first connector index=%d", got)
	}
	go func() { errCh <- m.RestartServer(context.Background(), "fs") }()
	if got := <-started; got != 1 {
		t.Fatalf("second connector index=%d", got)
	}
	// The later invocation completes first and commits. The earlier candidate
	// must be rejected when it eventually finishes.
	close(releases[1])
	if err := <-errCh; err != nil {
		t.Fatalf("latest restart failed: %v", err)
	}
	close(releases[0])
	if err := <-errCh; err == nil {
		t.Fatal("older concurrent restart must fail its stale revision CAS")
	}
	infos := m.ListToolInfos()
	if len(infos) != 1 || infos[0].Name != "candidate-2" {
		t.Fatalf("latest restart did not win: %+v", infos)
	}
	if got := oldClosed.Load(); got != 1 {
		t.Fatalf("old server close count=%d want 1", got)
	}
	if got := candidateClosed[0].Load(); got != 1 {
		t.Fatalf("stale candidate close count=%d want 1", got)
	}
	if got := candidateClosed[1].Load(); got != 0 {
		t.Fatalf("winning candidate was closed unexpectedly: %d", got)
	}
}

func TestCallTool_StaleConnectionFailureCannotDisconnectRestartedServer(t *testing.T) {
	m := NewManager()
	entered := make(chan struct{})
	release := make(chan struct{})
	oldTool := mock.NewTool("read", mock.WithToolExecuteFn(func(context.Context, map[string]any) (tool.Result, error) {
		close(entered)
		<-release
		return tool.Result{}, io.EOF
	}))
	m.mu.Lock()
	m.configs = []ServerConfig{{Name: "fs", Enabled: true, Transport: "stdio", Command: "stub"}}
	m.servers["fs"] = &connectedServer{name: "fs", connected: true, tools: []hexagon.Tool{oldTool}}
	m.mu.Unlock()

	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		return []hexagon.Tool{fakeTool("fresh")}, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })

	callDone := make(chan error, 1)
	go func() {
		_, _, err := m.CallToolWithOwner(context.Background(), "read", nil)
		callDone <- err
	}()
	<-entered

	if err := m.RestartServer(context.Background(), "fs"); err != nil {
		t.Fatalf("RestartServer: %v", err)
	}
	close(release)
	if err := <-callDone; err == nil {
		t.Fatal("stale tool call must still report its connection failure")
	}

	m.mu.RLock()
	current := m.servers["fs"]
	m.mu.RUnlock()
	if current == nil || !current.connected {
		t.Fatalf("stale failure disconnected the fresh server: %+v", current)
	}
	infos := m.ListToolInfos()
	if len(infos) != 1 || infos[0].Name != "fresh" {
		t.Fatalf("fresh restarted server tools disappeared: %+v", infos)
	}
}

func TestRemoveAndCloseReleaseManagerLockBeforeTransportClose(t *testing.T) {
	tests := []struct {
		name string
		act  func(*Manager)
	}{
		{name: "remove", act: func(m *Manager) { _ = m.RemoveServer("fs") }},
		{name: "close", act: func(m *Manager) { m.Close() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager()
			closed := make(chan struct{})
			var once sync.Once
			seedRestartableServer(m, "fs", func() {
				_ = m.ServerNames() // deadlocks if transport closes under m.mu
				once.Do(func() { close(closed) })
			})
			done := make(chan struct{})
			go func() {
				tt.act(m)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("manager lock was held while closing transport")
			}
			select {
			case <-closed:
			default:
				t.Fatal("transport cleanup was not called")
			}
		})
	}
}

func TestAllReplacementPathsReleaseManagerLockBeforeTransportClose(t *testing.T) {
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(context.Context, string, map[string]string, ...string) ([]hexagon.Tool, func(), error) {
		return []hexagon.Tool{fakeTool("new")}, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })
	cfg := ServerConfig{Name: "fs", Enabled: true, Transport: "stdio", Command: "stub"}
	tests := []struct {
		name string
		act  func(*Manager)
	}{
		{name: "register", act: func(m *Manager) { _ = m.RegisterServer(context.Background(), cfg) }},
		{name: "add", act: func(m *Manager) { _ = m.AddServer(context.Background(), cfg) }},
		{name: "add-best-effort", act: func(m *Manager) { _, _ = m.AddServerBestEffort(context.Background(), cfg) }},
		{name: "reconnect", act: func(m *Manager) { m.tryReconnect() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager()
			closed := make(chan struct{})
			var once sync.Once
			seedRestartableServer(m, "fs", func() {
				_ = m.ServerNames()
				once.Do(func() { close(closed) })
			})
			if tt.name == "reconnect" {
				m.mu.Lock()
				m.servers["fs"].connected = false
				m.mu.Unlock()
			}
			done := make(chan struct{})
			go func() { tt.act(m); close(done) }()
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("manager lock was held while closing replaced transport")
			}
			select {
			case <-closed:
			default:
				t.Fatal("replaced transport cleanup was not called")
			}
		})
	}
}
