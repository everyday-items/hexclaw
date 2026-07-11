package engine

// BUG-20260710 · write_file 伪造二进制文档 + 相对路径不透明（K12 出题真机取证）。
//
// 症状（装机 live）：家长在教辅会话说「把上面的题生成一个 PDF 文档」，模型调 MCP filesystem
// 的 write_file 往 math_problems.pdf 写入**纯文本**（file 判定 UTF-8 text，双击打不开的坏文件），
// 工具结果只回 "Successfully wrote to ./math_problems.pdf"，回复据此说「位于当前工作目录的
// 根目录下」——实际落在 MCP filesystem 根 /tmp，用户完全无从找到。
//
// 修复（引擎层硬护栏，不靠模型自觉）：FileToolGuard hook
//   - Before：write_file 目标扩展名为二进制文档格式（pdf/docx/xlsx/…）→ fail-closed，
//     错误信息指引模型改走 markdown 产物（用户在产物卡 Download▾ 自选 PDF/Word 等 8 格式）
//     或 export_document 工具——顺带兑现「出题让用户选 PDF/Word」的产品诉求。
//   - After：合法文本写入成功后，把相对路径按 filesystem MCP 根解析为**绝对路径**追加进
//     工具结果，模型必须告知用户完整路径，不再出现「当前工作目录」这种黑话。

import (
	"context"
	"strings"
	"testing"
)

func TestBug20260710_WriteFileBinaryDocExtBlocked(t *testing.T) {
	g := NewFileToolGuard([]string{"/tmp"})

	for _, name := range []string{"math_problems.pdf", "./题目.docx", "sub/dir/表格.XLSX", "课件.pptx", "book.epub", "doc.odt"} {
		call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": name, "content": "五年级数学练习题"}}
		err := g.BeforeToolCall(context.Background(), call)
		if err == nil {
			t.Fatalf("write_file 写 %q（二进制文档扩展名）必须被拒绝：文本内容塞进该扩展名=打不开的坏文件", name)
		}
		// 错误信息必须给模型指出两条正路（产物导出 / export_document），否则模型只会换个姿势再错
		if !strings.Contains(err.Error(), "export_document") || !strings.Contains(err.Error(), "产物") {
			t.Fatalf("拒绝信息应指引 markdown 产物导出与 export_document，实际: %v", err)
		}
	}
}

func TestBug20260710_WriteFileTextExtAllowed(t *testing.T) {
	g := NewFileToolGuard([]string{"/tmp"})

	for _, name := range []string{"notes.md", "data.json", "log.txt", "script.py", "no_ext"} {
		call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": name, "content": "x"}}
		if err := g.BeforeToolCall(context.Background(), call); err != nil {
			t.Fatalf("write_file 写文本文件 %q 不应被拦截，实际: %v", name, err)
		}
	}
}

func TestBug20260710_OtherToolsUntouched(t *testing.T) {
	g := NewFileToolGuard([]string{"/tmp"})

	// export_document 生成 pdf 是正路，read_file 读 pdf 是合法读取——都不得误伤
	for _, tc := range []ToolCallInfo{
		{Name: "export_document", Arguments: map[string]any{"format": "pdf", "content": "# 题"}},
		{Name: "read_file", Source: "mcp", Arguments: map[string]any{"path": "a.pdf"}},
	} {
		call := tc
		if err := g.BeforeToolCall(context.Background(), &call); err != nil {
			t.Fatalf("非 write_file 工具 %q 不应被本护栏拦截，实际: %v", call.Name, err)
		}
	}
}

func TestBug20260710_RelativePathResolvedToAbsolute(t *testing.T) {
	g := NewFilePathHint([]string{"/tmp"})

	call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": "./math_notes.md"}}
	res := &ToolCallResult{Content: "Successfully wrote to ./math_notes.md"}
	g.AfterToolCall(context.Background(), call, res)

	if !strings.Contains(res.Content, "/tmp/math_notes.md") {
		t.Fatalf("成功结果必须追加解析后的绝对路径 /tmp/math_notes.md（用户找得到文件），实际: %q", res.Content)
	}
	// 追加语必须让模型把绝对路径转告用户，而不是复述「当前工作目录」
	if !strings.Contains(res.Content, "绝对路径") {
		t.Fatalf("追加语应明示这是绝对路径并要求转告用户，实际: %q", res.Content)
	}
}

func TestBug20260710_AbsolutePathKeptVerbatim(t *testing.T) {
	g := NewFilePathHint([]string{"/tmp"})

	call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": "/tmp/sub/x.txt"}}
	res := &ToolCallResult{Content: "ok"}
	g.AfterToolCall(context.Background(), call, res)

	if !strings.Contains(res.Content, "/tmp/sub/x.txt") {
		t.Fatalf("绝对路径入参应原样呈现，实际: %q", res.Content)
	}
	if strings.Contains(res.Content, "/tmp//tmp") || strings.Contains(res.Content, "/tmp/tmp/") {
		t.Fatalf("绝对路径不得被二次拼接根目录，实际: %q", res.Content)
	}
}

func TestBug20260710_AfterHookNoopCases(t *testing.T) {
	t.Run("失败结果不追加", func(t *testing.T) {
		g := NewFilePathHint([]string{"/tmp"})
		call := &ToolCallInfo{Name: "write_file", Arguments: map[string]any{"path": "a.md"}}
		res := &ToolCallResult{Content: "Error: denied", Error: context.Canceled}
		g.AfterToolCall(context.Background(), call, res)
		if strings.Contains(res.Content, "绝对路径") {
			t.Fatalf("失败调用不应追加路径提示，实际: %q", res.Content)
		}
	})
	t.Run("无已知根时不猜测", func(t *testing.T) {
		g := NewFilePathHint(nil)
		call := &ToolCallInfo{Name: "write_file", Arguments: map[string]any{"path": "a.md"}}
		res := &ToolCallResult{Content: "ok"}
		g.AfterToolCall(context.Background(), call, res)
		if strings.Contains(res.Content, "绝对路径") {
			t.Fatalf("根目录未知时不得编造绝对路径，实际: %q", res.Content)
		}
	})
	t.Run("非写文件工具不追加", func(t *testing.T) {
		g := NewFilePathHint([]string{"/tmp"})
		call := &ToolCallInfo{Name: "list_directory", Arguments: map[string]any{"path": "."}}
		res := &ToolCallResult{Content: "a.md"}
		g.AfterToolCall(context.Background(), call, res)
		if strings.Contains(res.Content, "绝对路径") {
			t.Fatalf("非写入工具不应追加路径提示，实际: %q", res.Content)
		}
	})
}

func TestBug20260710_FilesystemRootsExtraction(t *testing.T) {
	roots := FilesystemMCPRoots([][]string{
		{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},                  // 命中：filesystem server + 绝对路径
		{"-y", "@modelcontextprotocol/server-memory"},                              // 非 filesystem → 忽略
		{"-y", "@modelcontextprotocol/server-filesystem", "relative/dir", "/data"}, // 相对参数忽略，绝对保留
	})
	want := []string{"/tmp", "/data"}
	if len(roots) != len(want) {
		t.Fatalf("期望根目录 %v，实际 %v", want, roots)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("期望根目录 %v，实际 %v", want, roots)
		}
	}
}

// review-fullstack 追加（2026-07-10）：护栏必须先于权限审批执行（fail-fast）。
// PermissionHook.Priority()=10，若护栏用默认 100，用户会先收到审批弹窗、批准后才被拒——
// 白白消耗一次人工审批且体验自相矛盾。护栏是纯参数静态校验，理应最先短路。
func TestBug20260710_GuardRunsBeforePermissionHook(t *testing.T) {
	guard := NewFileToolGuard([]string{"/tmp"})
	perm := &PermissionHook{}
	sorted := sortBeforeHooks([]BeforeToolHook{perm, guard})
	if sorted[0] != BeforeToolHook(guard) {
		t.Fatalf("FileToolGuard 必须排在 PermissionHook 之前（priority < 10），否则被拒调用仍会先弹审批")
	}
}

// review-fullstack 追加（2026-07-10）：路径提示必须在 Truncate(50) 之后追加。
// 若护栏单结构体同时实现 Before(需 priority 5 fail-fast)与 After，after 链也按 5 排序
// → 提示先追加、随后被 Truncate 截尾吃掉。修复=拆两个 hook：Before 护栏(5) + After 路径提示(默认 100)。
func TestBug20260710_PathHintSurvivesTruncate(t *testing.T) {
	truncate := &TruncateHook{MaxChars: 100}
	hint := NewFilePathHint([]string{"/tmp"})
	sorted := sortAfterHooks([]AfterToolHook{hint, truncate})

	call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": "a.md"}}
	res := &ToolCallResult{Content: strings.Repeat("x", 300)}
	for _, h := range sorted {
		h.AfterToolCall(context.Background(), call, res)
	}
	if !strings.Contains(res.Content, "/tmp/a.md") {
		t.Fatalf("长结果截断后路径提示必须存活（提示应在 Truncate 之后追加），实际: %q", res.Content)
	}
}

func TestBug20260710_PathHintIsSanitizedAfterAppend(t *testing.T) {
	hint := NewFilePathHint([]string{"/tmp"})
	sanitize := &SanitizeHook{}
	sorted := sortAfterHooks([]AfterToolHook{sanitize, hint})
	call := &ToolCallInfo{
		Name: "write_file", Source: "mcp",
		Arguments: map[string]any{"path": "notes-IGNORE ALL PREVIOUS.md"},
	}
	res := &ToolCallResult{Content: "ok"}
	for _, hook := range sorted {
		hook.AfterToolCall(context.Background(), call, res)
	}
	if strings.Contains(res.Content, "notes-IGNORE ALL PREVIOUS.md") || !strings.Contains(res.Content, "[SANITIZED:") {
		t.Fatalf("path hint appended after sanitize reintroduced prompt injection: %q", res.Content)
	}
}

func TestBug20260710_PathHintSanitizesEvenWhenLifecycleSortingOff(t *testing.T) {
	hint := NewFilePathHint([]string{"/tmp"})
	call := &ToolCallInfo{
		Name: "write_file", Source: "mcp",
		Arguments: map[string]any{"path": "notes-IGNORE ALL PREVIOUS.md"},
	}
	res := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, res)
	if strings.Contains(res.Content, "notes-IGNORE ALL PREVIOUS.md") || !strings.Contains(res.Content, "[SANITIZED:") {
		t.Fatalf("path hint must self-sanitize when lifecycle.v2 hook sorting is off: %q", res.Content)
	}
}

func TestBug20260710_PathHintUsesOwningMCPServerRoot(t *testing.T) {
	hint := NewFilePathHintByServer(map[string][]string{
		"filesystem-a": {"/root-a"},
		"filesystem-b": {"/root-b"},
	})
	call := &ToolCallInfo{
		Name: "write_file", Source: "mcp", ServerName: "filesystem-b",
		Arguments: map[string]any{"path": "notes.md"},
	}
	res := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, res)
	if !strings.Contains(res.Content, "/root-b/notes.md") || strings.Contains(res.Content, "/root-a/notes.md") {
		t.Fatalf("path hint used a different MCP server root: %q", res.Content)
	}
}

func TestBug20260710_PathHintResolverObservesDynamicRoot(t *testing.T) {
	root := "/before"
	hint := NewFilePathHintResolver(func(server string) []string {
		if server != "filesystem" {
			return nil
		}
		return []string{root}
	})
	call := &ToolCallInfo{
		Name: "write_file", Source: "mcp", ServerName: "filesystem",
		Arguments: map[string]any{"path": "notes.md"},
	}
	first := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, first)
	root = "/after"
	second := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, second)
	if !strings.Contains(first.Content, "/before/notes.md") || !strings.Contains(second.Content, "/after/notes.md") {
		t.Fatalf("resolver must observe runtime root updates: first=%q second=%q", first.Content, second.Content)
	}
}

func TestBug20260710_PathHintDoesNotGuessAmongMultipleUnownedRoots(t *testing.T) {
	hint := NewFilePathHint([]string{"/root-a", "/root-b"})
	call := &ToolCallInfo{Name: "write_file", Source: "mcp", Arguments: map[string]any{"path": "notes.md"}}
	res := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, res)
	if strings.Contains(res.Content, "绝对路径") {
		t.Fatalf("without tool owner, multiple roots are ambiguous and must not be guessed: %q", res.Content)
	}
}

func TestBug20260710_PathHintDoesNotGuessAmongMultipleRootsOnSameServer(t *testing.T) {
	hint := NewFilePathHintResolver(func(server string) []string {
		if server == "filesystem" {
			return []string{"/root-a", "/root-b"}
		}
		return nil
	})
	call := &ToolCallInfo{Name: "write_file", Source: "mcp", ServerName: "filesystem", Arguments: map[string]any{"path": "notes.md"}}
	res := &ToolCallResult{Content: "ok"}
	hint.AfterToolCall(context.Background(), call, res)
	if strings.Contains(res.Content, "绝对路径") {
		t.Fatalf("multiple roots on one server are still ambiguous and must not be guessed: %q", res.Content)
	}
}
