package agents

import (
	"testing"
)

// TestFactory_ListRoles 测试角色列表
func TestFactory_ListRoles(t *testing.T) {
	f := NewFactory()
	roles := f.ListRoles()

	if len(roles) != 6 {
		t.Fatalf("期望 6 个角色，实际 %d 个: %v", len(roles), roles)
	}

	// 检查是否包含所有预定义角色
	expected := map[string]bool{
		"assistant": false, "researcher": false, "writer": false,
		"coder": false, "translator": false, "analyst": false,
	}
	for _, name := range roles {
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("缺少角色: %s", name)
		}
	}
}

// TestFactory_GetRole 测试获取角色
func TestFactory_GetRole(t *testing.T) {
	f := NewFactory()

	role, ok := f.GetRole("coder")
	if !ok {
		t.Fatal("应能获取 coder 角色")
	}
	if role.Name != "coder" {
		t.Fatalf("期望 coder，实际 %s", role.Name)
	}

	_, ok = f.GetRole("nonexistent")
	if ok {
		t.Fatal("不应获取到不存在的角色")
	}
}

// TestFactory_RegisterRole 测试注册自定义角色
func TestFactory_RegisterRole(t *testing.T) {
	f := NewFactory()

	customRole := AssistantRole
	customRole.Name = "custom"
	customRole.Goal = "自定义目标"

	f.RegisterRole("custom", customRole)

	role, ok := f.GetRole("custom")
	if !ok {
		t.Fatal("应能获取自定义角色")
	}
	if role.Goal != "自定义目标" {
		t.Fatalf("期望自定义目标，实际 %s", role.Goal)
	}

	roles := f.ListRoles()
	if len(roles) != 7 {
		t.Fatalf("期望 7 个角色（6 预定义 + 1 自定义），实际 %d", len(roles))
	}
}

// TestRoles_AllHaveGoalAndBackstory 测试所有角色都有目标和背景
func TestRoles_AllHaveGoalAndBackstory(t *testing.T) {
	for name, role := range allRoles {
		if role.Name == "" {
			t.Errorf("角色 %s 缺少 Name", name)
		}
		if role.Goal == "" {
			t.Errorf("角色 %s 缺少 Goal", name)
		}
		if role.Backstory == "" {
			t.Errorf("角色 %s 缺少 Backstory", name)
		}
		if len(role.Expertise) == 0 {
			t.Errorf("角色 %s 缺少 Expertise", name)
		}
	}
}
