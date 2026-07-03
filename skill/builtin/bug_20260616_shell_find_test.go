package builtin

// 功能优先回归：shell skill 不再维护应用层命令白名单。
// find 的 -exec/-delete 等 action primaries 作为普通 shell 能力保留，避免工程任务
// 因过窄白名单失败；是否允许由外层权限策略/用户配置决定。

import "testing"

func TestShellFindActionPrimitivesAllowedFunctionFirst(t *testing.T) {
	for _, cmd := range []string{
		"find . -name x -exec rm -rf {} +",
		"find . -delete",
	} {
		if reason := checkAllowed(cmd); reason != "" {
			t.Errorf("function-first shell should allow find action primitive %q, got %q", cmd, reason)
		}
	}
	if reason := checkAllowed("find . -name *.go"); reason != "" {
		t.Errorf("plain find should remain allowed, got %q", reason)
	}
}
