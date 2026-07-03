package engine

import "testing"

// app_heal 在 legacy classifyRisk 路径也应保持 consequential 分类。
// 交互式会话可据此走确认门；无人值守系统派发由 PermissionHook 的 autonomy 矩阵决定。
func TestPermission_AppHealAlwaysGated_FlagIndependent(t *testing.T) {
	hook := NewPermissionHook(nil)
	if got := hook.classifyRisk("app_heal"); got != "dangerous" {
		t.Fatalf("app_heal 应被 legacy 路径判 dangerous，实际 %q", got)
	}
	// 对照：只读自省工具不该被误判为高危
	if got := hook.classifyRisk("app_query"); got == "dangerous" {
		t.Fatalf("app_query(只读) 不应 dangerous，实际 %q", got)
	}
}
