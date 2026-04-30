package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// recordingHook 实现 LifecycleHook，记录调用以供断言。
type recordingHook struct {
	mu              sync.Mutex
	connected       []string
	disconnected    []string
	disconnectReason map[string]string
}

func newRecordingHook() *recordingHook {
	return &recordingHook{disconnectReason: map[string]string{}}
}

func (h *recordingHook) OnServerConnected(_ context.Context, name string, _ int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected = append(h.connected, name)
}
func (h *recordingHook) OnServerDisconnected(_ context.Context, name, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.disconnected = append(h.disconnected, name)
	h.disconnectReason[name] = reason
}
func (h *recordingHook) snapshot() (conn, disc []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn = append(conn, h.connected...)
	disc = append(disc, h.disconnected...)
	return
}

func withMCPFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagMCPLifecycleV2: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// 直接构造一个已"连接"的 fake server，避免依赖真实子进程 / SSE。
func injectFakeServer(m *Manager, name string) {
	m.mu.Lock()
	m.servers[name] = &connectedServer{name: name, connected: true}
	m.mu.Unlock()
}

func TestAddLifecycleHook_StoresEvenWhenFlagOff(t *testing.T) {
	m := NewManager()
	h := newRecordingHook()
	m.AddLifecycleHook(h)
	if len(m.hooks.snapshot()) != 1 {
		t.Errorf("hook 应被注册；got %d", len(m.hooks.snapshot()))
	}
}

func TestUnregisterServer_FiresHook_WhenFlagOn(t *testing.T) {
	m := NewManager()
	h := newRecordingHook()
	m.AddLifecycleHook(h)
	injectFakeServer(m, "alpha")

	ctx := withMCPFlag(context.Background(), true)
	if !m.UnregisterServer(ctx, "alpha") {
		t.Fatal("UnregisterServer 应返回 true")
	}

	conn, disc := h.snapshot()
	if len(disc) != 1 || disc[0] != "alpha" {
		t.Errorf("Disconnected hook 应被触发一次；got %v", disc)
	}
	if len(conn) != 0 {
		t.Errorf("Connected hook 不应被触发；got %v", conn)
	}
	if h.disconnectReason["alpha"] != "manual unregister" {
		t.Errorf("reason 不对；got %q", h.disconnectReason["alpha"])
	}
}

func TestUnregisterServer_NoHook_WhenFlagOff(t *testing.T) {
	m := NewManager()
	h := newRecordingHook()
	m.AddLifecycleHook(h)
	injectFakeServer(m, "alpha")

	ctx := withMCPFlag(context.Background(), false)
	if !m.UnregisterServer(ctx, "alpha") {
		t.Fatal("UnregisterServer 仍应正常工作")
	}

	_, disc := h.snapshot()
	if len(disc) != 0 {
		t.Errorf("flag OFF 时不应触发 hook；got %v", disc)
	}
}

func TestUnregisterServer_NotFoundReturnsFalse(t *testing.T) {
	m := NewManager()
	if m.UnregisterServer(context.Background(), "nope") {
		t.Error("不存在的 server 应返回 false")
	}
}

func TestRegisterServer_DisabledOnlyStoresConfig(t *testing.T) {
	m := NewManager()
	h := newRecordingHook()
	m.AddLifecycleHook(h)

	ctx := withMCPFlag(context.Background(), true)
	if err := m.RegisterServer(ctx, ServerConfig{Name: "x", Enabled: false}); err != nil {
		t.Fatalf("disabled 注册不应报错；got %v", err)
	}
	conn, _ := h.snapshot()
	if len(conn) != 0 {
		t.Errorf("disabled server 不应触发 connected hook；got %v", conn)
	}
	// 但 config 应被保存
	m.mu.RLock()
	cnt := 0
	for _, c := range m.configs {
		if c.Name == "x" {
			cnt++
		}
	}
	m.mu.RUnlock()
	if cnt != 1 {
		t.Errorf("config 应被记录 1 次；got %d", cnt)
	}
}

func TestRegisterServer_RejectsEmptyName(t *testing.T) {
	m := NewManager()
	if err := m.RegisterServer(context.Background(), ServerConfig{Name: "", Enabled: true}); err == nil {
		t.Error("空 name 应报错")
	}
}

func TestSafeFire_RecoverHookPanic(t *testing.T) {
	called := false
	safeFire(func() {
		called = true
		panic("boom")
	})
	if !called {
		t.Error("hook 仍应被调用")
	}
}

// 验证多 hook 都被触发；保留注册顺序
func TestLifecycle_MultipleHooks_AllFired(t *testing.T) {
	m := NewManager()
	h1, h2 := newRecordingHook(), newRecordingHook()
	m.AddLifecycleHook(h1)
	m.AddLifecycleHook(h2)
	injectFakeServer(m, "alpha")

	ctx := withMCPFlag(context.Background(), true)
	m.UnregisterServer(ctx, "alpha")

	_, d1 := h1.snapshot()
	_, d2 := h2.snapshot()
	if len(d1) != 1 || len(d2) != 1 {
		t.Errorf("两 hook 都应被触发；got h1=%v h2=%v", d1, d2)
	}
}
