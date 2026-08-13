package builtin

import (
	"os"
	"strings"
	"testing"
)

// P0.2（T4.1 架构评审 · 单一沙箱路径锁）：全栈唯一执行原语须走 toolkit/os/sandbox（code_exec 样板），
// skill/builtin 下**禁止裸 os/exec 执行**。已弃用的 code.go/shell.go 走 grandfather 白名单（opt-in +
// 安全警告 + 计划 P2 遥测窗口后删除），除此之外任何文件新增 `exec.Command`/`exec.CommandContext`
// 即 FAIL——机械挡死「手搓裸 exec 绕过沙箱」整类复发（举一反三）。
//
// 若确有「可信环境完全不隔离宿主执行」诉求，应收进 code_exec 的显式能力位（默认关 + 审计），
// 而非新增独立裸 exec skill（见评审 §九）。
func TestSingleSandboxPath_NoBareExecOutsideDeprecated(t *testing.T) {
	// grandfather：已弃用、待删的裸 exec skill（P2 删除后应从白名单移除、届时锁自动收紧）。
	deprecated := map[string]bool{"code.go": true, "shell.go": true}
	markers := []string{"exec.Command(", "exec.CommandContext("}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || deprecated[name] {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := stripLineComments(string(b))
		for _, m := range markers {
			if strings.Contains(src, m) {
				violations = append(violations, name+" 含 "+m)
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("Bare exec bypasses the sandbox; use toolkit/os/sandbox sb.Exec through code_exec.go:\n%s",
			strings.Join(violations, "\n"))
	}
}

// stripLineComments 去掉 // 行注释，避免注释里提到 exec.Command 误报（AP-189：匹配前先剥注释）。
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
