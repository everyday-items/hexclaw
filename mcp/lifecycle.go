// lifecycle.go 实现 v0.4.0 H3 MCP Lifecycle hook 协议（feature-flag gated）。
//
// 老路径：Manager.Connect 批量连接所有 server，没有钩子让外部代码观测连接状态。
// 这导致 Audit / Metrics / Events 投递必须靠扫日志反推，无法精确捕获 server 上下线。
//
// H3 引入 LifecycleHook：
//   - OnServerConnected(ctx, name, tools)：连接成功 / 重连成功后触发
//   - OnServerDisconnected(ctx, name, reason)：主动 Close 或检测到断连后触发
//
// hook 列表 + 调度只在 feature flag mcp.lifecycle.v2 开启时生效。flag 关闭时
// AddLifecycleHook 仍然记录 hook（以便 flag 后续被打开后立即生效），但 trigger
// 时不会调用任何 hook —— 所以引入 hook 不会改变 v0.3 默认行为。
package mcp

import (
	"context"
	"sync"

	"github.com/hexagon-codes/hexclaw/events"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagMCPLifecycleV2 控制 H3 lifecycle hook 是否生效。alpha 默认 OFF。
const FlagMCPLifecycleV2 = "mcp.lifecycle.v2"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagMCPLifecycleV2,
		Default:      true, // alpha 强制 OFF
		Description:  "Enable MCP server lifecycle hooks (OnServerConnected / OnServerDisconnected) and dynamic Register/Unregister APIs.",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// LifecycleHook 接收 MCP server 连接状态变更事件。
//
// 钩子调用是同步的：长时间阻塞会延迟 Manager 的连接 / 断开操作。复杂 sink
// （HTTP / DB 写入）应在 hook 内部 spawn goroutine 异步处理。
type LifecycleHook interface {
	// OnServerConnected 在 server 首次连接成功 / 重连成功后被调用。
	// toolCount 是该次连接发现的工具数量；name 是 ServerConfig.Name。
	OnServerConnected(ctx context.Context, name string, toolCount int)
	// OnServerDisconnected 在主动 Close / 检测到断连 / 卸载后被调用。
	// reason 是简短的下线原因描述（"manual unregister" / "transport closed" / "shutdown"）。
	OnServerDisconnected(ctx context.Context, name string, reason string)
}

// hooksRegistry 管理 Manager 上挂的 LifecycleHook 列表（线程安全）。
type hooksRegistry struct {
	mu    sync.RWMutex
	hooks []LifecycleHook
}

func (r *hooksRegistry) add(h LifecycleHook) {
	if h == nil {
		return
	}
	r.mu.Lock()
	r.hooks = append(r.hooks, h)
	r.mu.Unlock()
}

func (r *hooksRegistry) snapshot() []LifecycleHook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]LifecycleHook, len(r.hooks))
	copy(out, r.hooks)
	return out
}

// fireConnected 在 flag 开启时触发所有已注册 hook 的 OnServerConnected。
// flag 关闭时是 no-op。
//
// v0.4.0 H6：同时投递 mcp.server.connected 结构化事件（events.transport.v1
// 关闭时 Emit 自身退化为 no-op）。
func (r *hooksRegistry) fireConnected(ctx context.Context, name string, toolCount int) {
	if !featureflag.Enabled(ctx, FlagMCPLifecycleV2) {
		return
	}
	for _, h := range r.snapshot() {
		safeFire(func() { h.OnServerConnected(ctx, name, toolCount) })
	}
	_ = events.Emit(ctx, events.New("mcp.server.connected", events.SeverityInfo).
		WithSource("mcp.lifecycle").
		With("server", name).
		With("tool_count", toolCount))
}

// fireDisconnected 在 flag 开启时触发所有已注册 hook 的 OnServerDisconnected。
// flag 关闭时是 no-op。
func (r *hooksRegistry) fireDisconnected(ctx context.Context, name, reason string) {
	if !featureflag.Enabled(ctx, FlagMCPLifecycleV2) {
		return
	}
	for _, h := range r.snapshot() {
		safeFire(func() { h.OnServerDisconnected(ctx, name, reason) })
	}
	_ = events.Emit(ctx, events.New("mcp.server.disconnected", events.SeverityInfo).
		WithSource("mcp.lifecycle").
		With("server", name).
		With("reason", reason))
}

// safeFire 把单个 hook 调用包在 panic recover 里，避免坏 hook 卡死整个 Manager。
func safeFire(fn func()) {
	defer func() { _ = recover() }()
	fn()
}
