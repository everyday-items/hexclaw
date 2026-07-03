//go:build darwin

package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

// BUG-20260626：用户在「数据连接器」加了本地目录(/Users/<u>/work)并已连接，但问 Agent 列目录时
// code_exec 跑 os.listdir 报「Directory not found」。根因之一：RegisterAdvanced 构建 code_exec 的
// 沙箱配置只设了 Workspace，从未把 config.Skill.Sandbox.Filesystem.AllowedPaths（经 SkillDeps 传入）
// 注入沙箱可读路径 → seatbelt deny-default 把授权目录挡在外面。
//
// 本测试：把授权目录经 SkillDeps.SandboxReadablePaths 传入 → code_exec 必须能读到。
// 修复前(builtin.go 未接线) RED：读不到；修复后 GREEN。
// 与 toolkit 层一致，授权目录落在家目录下（系统放行清单之外），模拟真实 /Users/<u>/work。
func TestBug20260626_CodeExecReadsConnectorAuthorizedDir(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec 不可用")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无家目录: %v", err)
	}
	authorized, err := os.MkdirTemp(home, ".hexclaw-conn-test-")
	if err != nil {
		t.Skipf("家目录不可写: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(authorized) })
	marker := filepath.Join(authorized, "marker.txt")
	if err := os.WriteFile(marker, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := skill.NewRegistry()
	deps := &SkillDeps{
		Workspace:            t.TempDir(),
		SandboxReadablePaths: []string{authorized}, // 模拟连接器授权目录经 config 注入
	}
	RegisterAdvanced(registry, config.BuiltinConfig{CodeExec: true}, deps)

	if deps.CodeExecSkill == nil {
		t.Fatal("code_exec 未注册")
	}

	res, err := deps.CodeExecSkill.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "import os\nprint(os.path.exists(" + pyStr(authorized) + "), os.path.isfile(" + pyStr(marker) + "))",
	})
	if err != nil {
		t.Fatalf("code_exec 执行失败: %v", err)
	}
	got := firstOutputLine(res.Content)
	if got != "True True" {
		t.Fatalf("连接器授权目录应可被 code_exec 读到，期望 'True True' 实得 %q", got)
	}
}
