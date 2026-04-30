// Package demo 提供 v0.4.0 H5 ExtensionPlugin 协议的参考实现。
//
// 用途：
//   - 让 plugin 作者照着抄一份最小骨架（Manifest + Init/Start/Stop/Health/Info）
//   - 在 manager_test 中作为"实现 ExtensionPlugin 的合规 plugin"用例
//   - 让运维/QA 在不引入第三方 plugin 的情况下验证 H5 校验链是否生效
//
// 不暴露任何业务能力（无 Skills、无 Adapter、无 Hooks）；仅声明
// CapReadSkills + CapEmitEvents 让 ExtensionContext 注入对应 API 用以演示沙箱。
//
// flag plugin.extension.v1 OFF 时本插件依然可注册（Manager.Register 跳过 Manifest 校验）；
// flag ON 时 Manager 会调用 ValidateManifest，验证 Name / Version / MinHostVersion / Capabilities。
package demo

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexagon"
	hexplugin "github.com/hexagon-codes/hexagon/plugin"
	"github.com/hexagon-codes/hexclaw/plugin"
)

// Plugin 是 H5 协议下最小可用的示例插件。
type Plugin struct {
	info      hexagon.PluginInfo
	started   atomic.Bool
	initCalls atomic.Int32
	stopCalls atomic.Int32
}

// New 构造一个 DemoPlugin 实例。
func New() *Plugin {
	return &Plugin{
		info: hexagon.PluginInfo{
			Name:        "com.hexclaw.demo",
			Version:     "0.4.0",
			Type:        plugin.TypeSkill,
			Description: "示例插件：演示 v0.4.0 H5 Manifest + Capability 沙箱",
			Author:      "HexClaw",
			License:     "Apache-2.0",
		},
	}
}

// Info 实现 hexagon.Plugin。
func (p *Plugin) Info() hexagon.PluginInfo { return p.info }

// Manifest 实现 plugin.ExtensionPlugin —— 必填字段全部就绪以通过 ValidateManifest。
func (p *Plugin) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:           "com.hexclaw.demo",
		Version:        "0.4.0",
		MinHostVersion: "0.4.0",
		Capabilities: []plugin.Capability{
			plugin.CapReadSkills,
			plugin.CapEmitEvents,
		},
		Description: "示例插件：演示 Manifest + Capability 沙箱",
	}
}

// Init 实现 hexagon.Plugin —— DemoPlugin 不消费任何 config。
func (p *Plugin) Init(_ context.Context, _ map[string]any) error {
	p.initCalls.Add(1)
	return nil
}

// Start 实现 hexagon.Plugin —— 标记 started 状态供 Health 报告。
func (p *Plugin) Start(_ context.Context) error {
	p.started.Store(true)
	return nil
}

// Stop 实现 hexagon.Plugin。
func (p *Plugin) Stop(_ context.Context) error {
	p.started.Store(false)
	p.stopCalls.Add(1)
	return nil
}

// Health 实现 hexagon.Plugin —— started 后 healthy，否则 unknown。
func (p *Plugin) Health() hexplugin.HealthStatus {
	state := hexplugin.HealthStateUnknown
	if p.started.Load() {
		state = hexplugin.HealthStateHealthy
	}
	return hexplugin.HealthStatus{
		Status:    state,
		LastCheck: time.Now(),
	}
}

// InitCalls 暴露给测试断言 Init 调用次数。
func (p *Plugin) InitCalls() int32 { return p.initCalls.Load() }

// StopCalls 暴露给测试断言 Stop 调用次数。
func (p *Plugin) StopCalls() int32 { return p.stopCalls.Load() }

// IsStarted 暴露给测试断言当前是否在运行。
func (p *Plugin) IsStarted() bool { return p.started.Load() }
