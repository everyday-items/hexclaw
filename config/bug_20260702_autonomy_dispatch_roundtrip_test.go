package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Bug 2026-07-02：Save 把 SystemDispatch 的「未覆盖」(nil) 序列化成 `cron: []`，
// 重新加载后变成「显式覆盖为空」——full_access 的 {"*"} 通配矩阵被整体清空，
// 档位名义上全功能、实际按 strict 跑（设置页矩阵全"需审批"）。
//
// 根治：字段改 *[]string + omitempty，nil/空的三态语义必须扛过 Save→Load 往返。

// TestAutonomySystemDispatch_SaveLoadRoundTrip_NilStaysNil 验证未设置任何
// 覆盖时，Save 不得写出 system_dispatch 空数组，重载后覆盖仍为 nil。
func TestAutonomySystemDispatch_SaveLoadRoundTrip_NilStaysNil(t *testing.T) {
	cfg := &Config{}
	cfg.Security.Autonomy.Profile = "full_access"

	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cron: []") {
		t.Fatalf("Save 不应把 nil 覆盖固化为空数组，got:\n%s", raw)
	}

	var loaded Config
	if err := yaml.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	sd := loaded.Security.Autonomy.SystemDispatch
	for name, f := range map[string]*[]string{
		"cron": sd.Cron, "webhook": sd.Webhook, "heartbeat": sd.Heartbeat,
		"workflow": sd.Workflow, "spawn": sd.Spawn, "solve": sd.Solve,
	} {
		if f != nil {
			t.Errorf("重载后 %s 覆盖应保持 nil（用档位默认），got %v", name, *f)
		}
	}
}

// TestAutonomySystemDispatch_SaveLoadRoundTrip_ExplicitEmptySurvives 验证
// 显式空覆盖（该来源什么都不自动放行）不会在往返后被丢弃——丢了就是反向安全回归。
func TestAutonomySystemDispatch_SaveLoadRoundTrip_ExplicitEmptySurvives(t *testing.T) {
	cfg := &Config{}
	cfg.Security.Autonomy.Profile = "function_first"
	cfg.Security.Autonomy.SystemDispatch.Webhook = &[]string{}

	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var loaded Config
	if err := yaml.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	sd := loaded.Security.Autonomy.SystemDispatch
	if sd.Webhook == nil || len(*sd.Webhook) != 0 {
		t.Errorf("显式空覆盖应扛过往返：want 非 nil 空切片, got %v", sd.Webhook)
	}
	if sd.Cron != nil {
		t.Errorf("未设置的 cron 不应被往返污染, got %v", *sd.Cron)
	}
}
