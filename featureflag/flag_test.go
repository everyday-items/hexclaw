package featureflag

import (
	"context"
	"testing"
)

// 测试基础注册 / 查询行为
func TestRegister_Lookup(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	Register(Flag{Name: "test_flag_1", Default: true, Stage: StageGA, Description: "x"})
	if got, ok := Lookup("test_flag_1"); !ok || got.Name != "test_flag_1" {
		t.Fatalf("Lookup 失败：got=%+v ok=%v", got, ok)
	}
	if _, ok := Lookup("nonexistent"); ok {
		t.Fatal("未注册的 flag 应返回 false")
	}
}

// 重复注册 panic（init time 失败）
func TestRegister_DuplicatePanics(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Flag{Name: "dup", Stage: StageGA})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("重复注册应 panic")
		}
	}()
	Register(Flag{Name: "dup", Stage: StageGA})
}

// 空名 panic
func TestRegister_EmptyNamePanics(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("空名应 panic")
		}
	}()
	Register(Flag{Name: ""})
}

// Static fail-closed 行为
func TestStatic_UnregisteredReturnsFalse(t *testing.T) {
	s := NewStatic([]Flag{}, map[string]bool{"random": true})
	if s.IsEnabled("random") {
		t.Fatal("未注册的 flag 即使 override 也应 OFF（fail-closed）")
	}
}

// Alpha 阶段强制 OFF
func TestStatic_AlphaForcedOff(t *testing.T) {
	flags := []Flag{
		{Name: "alpha_with_default_true", Default: true, Stage: StageAlpha},
	}
	s := NewStatic(flags, nil)
	if s.IsEnabled("alpha_with_default_true") {
		t.Fatal("Alpha 阶段即便 Default=true 也应 OFF")
	}
}

// Alpha 阶段允许用户显式 override 启用
func TestStatic_AlphaUserOverride(t *testing.T) {
	flags := []Flag{
		{Name: "alpha_v2", Default: false, Stage: StageAlpha},
	}
	s := NewStatic(flags, map[string]bool{"alpha_v2": true})
	if !s.IsEnabled("alpha_v2") {
		t.Fatal("用户 override 应优先于 Alpha 强制 default")
	}
}

// GA 阶段尊重 Default
func TestStatic_GADefaultRespected(t *testing.T) {
	flags := []Flag{
		{Name: "ga_on", Default: true, Stage: StageGA},
		{Name: "ga_off", Default: false, Stage: StageGA},
	}
	s := NewStatic(flags, nil)
	if !s.IsEnabled("ga_on") {
		t.Error("ga_on default=true 应 ON")
	}
	if s.IsEnabled("ga_off") {
		t.Error("ga_off default=false 应 OFF")
	}
}

// 用户 override 覆盖 default
func TestStatic_OverrideOverridesDefault(t *testing.T) {
	flags := []Flag{
		{Name: "f", Default: true, Stage: StageGA},
	}
	s := NewStatic(flags, map[string]bool{"f": false})
	if s.IsEnabled("f") {
		t.Fatal("override=false 应优先于 default=true")
	}
}

// Snapshot 字段完整
func TestStatic_Snapshot(t *testing.T) {
	flags := []Flag{
		{Name: "z_flag", Default: true, Stage: StageGA, Description: "z desc", SinceVersion: "0.4.0"},
		{Name: "a_flag", Default: false, Stage: StageBeta, Description: "a desc", SinceVersion: "0.4.0"},
	}
	s := NewStatic(flags, map[string]bool{"a_flag": true})
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("应有 2 项；got %d", len(snap))
	}
	// 字典序：a_flag 在前
	if snap[0].Name != "a_flag" {
		t.Errorf("应字典序排；got=%s", snap[0].Name)
	}
	if !snap[0].UserOverride || !snap[0].Enabled {
		t.Errorf("a_flag 有 override 应 enabled；got=%+v", snap[0])
	}
	if snap[1].UserOverride {
		t.Errorf("z_flag 无 override；got=%+v", snap[1])
	}
}

// ctx 注入 + 查询
func TestContext_WithFromEnabled(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Flag{Name: "ctx_test", Default: true, Stage: StageGA})
	flags := NewStatic(Registered(), nil)

	ctx := WithContext(context.Background(), flags)
	if !Enabled(ctx, "ctx_test") {
		t.Fatal("ctx 注入后应可查询")
	}
	if Enabled(ctx, "missing") {
		t.Fatal("未注册的 flag 应 OFF")
	}
}

// ctx 未注入时 fail-closed，未注册 flag 仍 OFF。
func TestContext_DefaultFallback(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Flag{Name: "ga_default_on", Default: true, Stage: StageGA})
	Register(Flag{Name: "alpha_default_true", Default: true, Stage: StageAlpha})

	if Enabled(context.Background(), "ga_default_on") {
		t.Fatal("ctx 没注入时应 fail-closed，即使 GA default=true")
	}
	if Enabled(context.Background(), "alpha_default_true") {
		t.Fatal("Alpha 仍应按有效默认值关闭")
	}
	if Enabled(nil, "missing") {
		t.Fatal("未注册 flag 仍应 OFF")
	}
}

// nil Flags 不破坏 ctx，仍走 fail-closed fallback。
func TestContext_WithNilNoop(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Flag{Name: "nil_noop_default_on", Default: true, Stage: StageGA})

	ctx := WithContext(context.Background(), nil)
	if Enabled(ctx, "nil_noop_default_on") {
		t.Fatal("nil Flags 注入应 fallback 到 fail-closed")
	}
}

// Mutable Set / Clear
func TestMutable_SetClear(t *testing.T) {
	flags := []Flag{
		{Name: "m1", Default: false, Stage: StageGA},
	}
	m := NewMutable(flags)
	if m.IsEnabled("m1") {
		t.Fatal("默认 OFF")
	}
	if !m.Set("m1", true) {
		t.Fatal("Set 应成功")
	}
	if !m.IsEnabled("m1") {
		t.Fatal("Set 后应 ON")
	}
	m.Clear("m1")
	if m.IsEnabled("m1") {
		t.Fatal("Clear 后应回归 default OFF")
	}
}

// Mutable Set 未注册返回 false
func TestMutable_SetUnregistered(t *testing.T) {
	m := NewMutable(nil)
	if m.Set("missing", true) {
		t.Fatal("未注册的 flag Set 应返回 false")
	}
}

// Stage.String()
func TestStage_String(t *testing.T) {
	cases := map[Stage]string{
		StageAlpha:      "alpha",
		StageBeta:       "beta",
		StageGA:         "ga",
		StageDeprecated: "deprecated",
		Stage(99):       "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Stage(%d).String()=%s want=%s", s, got, want)
		}
	}
}
