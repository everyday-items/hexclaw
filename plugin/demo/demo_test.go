// demo_test.go 验证 v0.4.0 H5 完整 plugin lifecycle：
// Manager.SetHostContext → Register（Manifest 校验） → Init → Start → Stop。
//
// 重点：
//   - flag plugin.extension.v1 ON + Manifest 合法 → Register 成功
//   - flag ON + 缺 MinHostVersion / 不兼容版本 → ManifestError，Register 失败
//   - flag OFF → 跳过校验，Register 直接通过（v0.3 行为）
package demo_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/plugin"
	"github.com/hexagon-codes/hexclaw/plugin/demo"
)

func newFlags(on bool) featureflag.Flags {
	return featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		plugin.FlagPluginExtensionV1: on,
	})
}

func TestDemoPlugin_FullLifecycle_FlagOn(t *testing.T) {
	mgr := plugin.NewManager()
	mgr.SetHostContext("0.4.0", newFlags(true))

	p := demo.New()
	if err := mgr.Register(p); err != nil {
		t.Fatalf("Register 应成功；got %v", err)
	}

	ctx := context.Background()
	if err := mgr.StartAll(ctx, nil); err != nil {
		t.Fatalf("StartAll 应成功；got %v", err)
	}
	if !p.IsStarted() {
		t.Error("Start 后 IsStarted 应为 true")
	}
	if got := p.InitCalls(); got != 1 {
		t.Errorf("Init 应调用一次；got %d", got)
	}

	mgr.StopAll(ctx)
	if p.IsStarted() {
		t.Error("Stop 后 IsStarted 应为 false")
	}
	if got := p.StopCalls(); got != 1 {
		t.Errorf("Stop 应调用一次；got %d", got)
	}
}

func TestDemoPlugin_HostVersionTooLow_FlagOn_Rejected(t *testing.T) {
	mgr := plugin.NewManager()
	// host 版本低于 plugin MinHostVersion (0.4.0)
	mgr.SetHostContext("0.3.0", newFlags(true))

	if err := mgr.Register(demo.New()); err == nil {
		t.Fatal("MinHostVersion=0.4.0 但 host=0.3.0 应该拒绝注册")
	}
}

func TestDemoPlugin_FlagOff_SkipsValidation(t *testing.T) {
	mgr := plugin.NewManager()
	mgr.SetHostContext("0.0.0", newFlags(false)) // 即使 host 版本不兼容

	if err := mgr.Register(demo.New()); err != nil {
		t.Fatalf("flag OFF 应跳过校验；got %v", err)
	}
}

func TestDemoPlugin_ManifestStructure(t *testing.T) {
	m := demo.New().Manifest()
	if m.Name != "com.hexclaw.demo" {
		t.Errorf("Name=%q want com.hexclaw.demo", m.Name)
	}
	if m.MinHostVersion != "0.4.0" {
		t.Errorf("MinHostVersion=%q want 0.4.0", m.MinHostVersion)
	}
	caps := m.Capabilities
	hasRead, hasEmit := false, false
	for _, c := range caps {
		if c == plugin.CapReadSkills {
			hasRead = true
		}
		if c == plugin.CapEmitEvents {
			hasEmit = true
		}
	}
	if !hasRead || !hasEmit {
		t.Errorf("应同时声明 CapReadSkills + CapEmitEvents；got %v", caps)
	}
}
