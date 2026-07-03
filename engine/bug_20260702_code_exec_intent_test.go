package engine

import "testing"

// bug_20260702（E）：code_exec 强制 tool_choice 的双语关键词启发式过于脆弱——
// 单个动词子串（如 "run code"）会误命中元讨论/how-to 问句（"how do I run code safely"），
// 命中即 restrictToolsToCodeExecWhenRequired 剥掉其余全部工具。收紧后：元讨论/how-to
// 不再强制；明确的祈使执行意图仍强制。

func TestBug20260702_CodeExecIntent_SuppressesMetaDiscussion(t *testing.T) {
	// 这些是"讨论/询问如何做"，绝不能强制 code_exec（旧代码 force=true → FAIL）。
	notForced := []string{
		"how do I run code safely",
		"how to run code in python",
		"如何执行代码",
		"怎么运行 python 代码",
		"运行 python 代码的最佳实践是什么",
	}
	for _, s := range notForced {
		if shouldForceCodeExecTool(s) {
			t.Errorf("meta/how-to must NOT force code_exec: %q", s)
		}
	}
}

func TestBug20260702_CodeExecIntent_KeepsGenuineImperatives(t *testing.T) {
	// 明确的祈使执行意图必须仍然强制。
	forced := []string{
		"执行代码：print('hi')",
		"执行这段代码：print(1)",
		"请调用 code_exec 运行 Python 脚本",
		"执行一些网络爬虫 Python 试下",
	}
	for _, s := range forced {
		if !shouldForceCodeExecTool(s) {
			t.Errorf("genuine imperative must force code_exec: %q", s)
		}
	}
}
