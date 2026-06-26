package engine

import "testing"

// 修 P10 review #1（安全 🔴）：app_heal 自愈写操作必须与 tool.policy.engine flag **无关**地强制审批。
// flag 默认 OFF → 走 legacy classifyRisk；若 app_heal 不在黑名单则默认配置下"审批后自愈"形同虚设。
// 本测试钉死：legacy 路径判 app_heal=dangerous（always-approve）。
func TestPermission_AppHealAlwaysGated_FlagIndependent(t *testing.T) {
	hook := NewPermissionHook(nil)
	if got := hook.classifyRisk("app_heal"); got != "dangerous" {
		t.Fatalf("app_heal 应被 legacy 路径判 dangerous(强制审批)，实际 %q —— 网关会在默认配置失效", got)
	}
	// 对照：只读自省工具不该被误判为高危
	if got := hook.classifyRisk("app_query"); got == "dangerous" {
		t.Fatalf("app_query(只读) 不应 dangerous，实际 %q", got)
	}
}
