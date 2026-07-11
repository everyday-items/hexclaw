package engine

// FileToolGuard —— write_file 文件落盘护栏（BUG-20260710）。
//
// 背景（K12 出题真机取证）：用户说「把上面的题生成一个 PDF」，模型调 MCP filesystem 的
// write_file 往 .pdf 里写纯文本 → 打不开的坏文件；工具结果只回相对路径 "./x.pdf"，
// 模型据此告诉用户「在当前工作目录」——实际在 filesystem MCP 根（默认 /tmp），用户找不到。
//
// 两条引擎层硬护栏（不依赖模型自觉，任何模型接入都生效）：
//
//  1. Before：write_file 目标扩展名是二进制文档格式 → fail-closed。错误信息给模型指两条正路：
//     ① 直接输出 markdown（桌面端渲染为产物，用户在产物卡 Download▾ 自选 PDF/docx 等 8 格式）；
//     ② 调 export_document（内置 pandoc/typst 渲染真文档，返回绝对路径）。
//  2. After：合法写入成功后，按 filesystem MCP 根把相对路径解析为绝对路径追加进结果，
//     并要求模型把完整路径转告用户。
//
// 根目录来源：cmd/hexclaw/main.go 注入 mcp.Manager.FilesystemRoots 动态解析器，
// 路径提示按本次实际命中的 MCP server owner 查询，运行时重配也不会读旧快照。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/hexclaw/security"
)

// binaryDocExts 是「往里写纯文本必然产生坏文件」的二进制文档扩展名集合。
// 注意不含 rtf/html/svg 等文本容器格式——模型输出对应标记文本是合法的。
var binaryDocExts = map[string]struct{}{
	".pdf": {}, ".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {},
	".ppt": {}, ".pptx": {}, ".epub": {}, ".odt": {}, ".ods": {}, ".odp": {},
}

// FileToolGuard 实现 BeforeToolHook + AfterToolHook。
type FileToolGuard struct {
	// filesystem MCP 的允许根目录（按配置顺序）；空则 After 不做路径解析（不猜测）。
	fsRoots []string
}

// NewFileToolGuard 创建护栏。roots 传 FilesystemMCPRoots 的产出。
func NewFileToolGuard(roots []string) *FileToolGuard {
	return &FileToolGuard{fsRoots: roots}
}

// Priority 让本护栏在 before 链最先执行（先于 PermissionHook 的 10）：
// 纯参数静态校验应 fail-fast——注定被拒的调用不该先消耗一次人工审批。
// 注意：正因这个 priority，After 侧路径提示拆到独立的 FilePathHint（优先级 70，
// 在 Truncate(50) 之后追加，长结果截断时提示才能存活）——单结构体两用会让
// after 链也按 5 排序、提示被 Truncate 吃掉（TestBug20260710_PathHintSurvivesTruncate）。
func (g *FileToolGuard) Priority() int { return 5 }

// FilesystemMCPRoots 从各 MCP server 的启动 args 中提取 filesystem server 的允许根目录：
// 仅当 args 里出现 server-filesystem 时，收集其中的绝对路径参数（相对参数无从解析，忽略）。
func FilesystemMCPRoots(serverArgs [][]string) []string {
	var roots []string
	for _, args := range serverArgs {
		isFS := false
		for _, a := range args {
			if strings.Contains(a, "server-filesystem") {
				isFS = true
				break
			}
		}
		if !isFS {
			continue
		}
		for _, a := range args {
			if filepath.IsAbs(a) {
				roots = append(roots, a)
			}
		}
	}
	return roots
}

// writeToolPath 提取写文件类工具的目标路径；非写文件工具返回空。
// MCP filesystem 的写入口是 write_file(path, content)；将来内置同名工具也走同一护栏。
func writeToolPath(call *ToolCallInfo) string {
	if call == nil || call.Name != "write_file" {
		return ""
	}
	for _, key := range []string{"path", "file_path", "filename"} {
		if v, ok := call.Arguments[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// BeforeToolCall：拦截把文本当二进制文档写的调用（fail-closed）。
func (g *FileToolGuard) BeforeToolCall(_ context.Context, call *ToolCallInfo) error {
	path := writeToolPath(call)
	if path == "" {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	if _, bad := binaryDocExts[ext]; !bad {
		return nil
	}
	return fmt.Errorf(
		"write_file 只能写纯文本文件；%q 的 %s 是二进制文档格式，把文本直接写进去会得到一个打不开的坏文件。"+
			"正确做法二选一：① 直接在回答里输出 markdown 内容（桌面端会渲染为产物，用户在产物卡片 Download▾ "+
			"下拉里自选导出 PDF / docx(Word) 等 8 种格式）；② 调用 export_document 工具"+
			"（format=%s），由内置渲染引擎生成真实文档并返回绝对路径", path, ext, strings.TrimPrefix(ext, "."))
}

// FilePathHint —— write_file 成功结果的绝对路径提示（After-only，独立于 FileToolGuard）。
// Priority 70：在 Truncate(50) 后追加，随后必须再经 Sanitize(80)，避免攻击者控制的
// 文件名把间接 prompt injection 重新带回结果。
type FilePathHint struct {
	fsRoots       []string
	rootsByServer map[string][]string
	rootResolver  func(serverName string) []string
}

// NewFilePathHint 创建路径提示 hook。roots 传 FilesystemMCPRoots 的产出。
func NewFilePathHint(roots []string) *FilePathHint {
	return &FilePathHint{fsRoots: append([]string(nil), roots...)}
}

// NewFilePathHintByServer binds configured roots to their MCP server owner.
func NewFilePathHintByServer(roots map[string][]string) *FilePathHint {
	cloned := make(map[string][]string, len(roots))
	for server, values := range roots {
		cloned[server] = append([]string(nil), values...)
	}
	return &FilePathHint{rootsByServer: cloned}
}

// NewFilePathHintResolver resolves roots at call time, so runtime MCP
// add/remove/reconfigure operations do not leave a stale startup snapshot.
func NewFilePathHintResolver(resolve func(serverName string) []string) *FilePathHint {
	return &FilePathHint{rootResolver: resolve}
}

func (g *FilePathHint) Priority() int { return 70 }

// AfterToolCall：写入成功后追加解析出的绝对路径，让模型能向用户说清文件真实位置。
func (g *FilePathHint) AfterToolCall(_ context.Context, call *ToolCallInfo, result *ToolCallResult) {
	if result == nil || result.Error != nil {
		return
	}
	path := writeToolPath(call)
	if path == "" {
		return
	}
	abs := path
	if !filepath.IsAbs(abs) {
		roots := g.rootsForServer(call.ServerName)
		if len(roots) != 1 {
			return // 根目录未知或有多个候选，宁缺毋滥，不编造路径
		}
		abs = filepath.Join(roots[0], abs)
	}
	abs = filepath.Clean(abs)
	hint := fmt.Sprintf(
		"\n[路径说明] 该文件的绝对路径是 %s ——回复用户时请给出这个完整路径（可提示在「访达/文件管理器」前往），"+
			"不要使用\"当前工作目录\"等用户无法定位的说法", abs)
	result.Content += security.SanitizeToolOutput(hint, 0)
}

func (g *FilePathHint) rootsForServer(serverName string) []string {
	if g == nil {
		return nil
	}
	if serverName != "" && g.rootResolver != nil {
		if roots := g.rootResolver(serverName); len(roots) > 0 {
			return roots
		}
	}
	if serverName != "" && len(g.rootsByServer) > 0 {
		if roots := g.rootsByServer[serverName]; len(roots) > 0 {
			return roots
		}
	}
	// Legacy constructor has no owner mapping. It is safe only when exactly
	// one root exists; choosing fsRoots[0] among several fabricates a path.
	if len(g.fsRoots) == 1 {
		return g.fsRoots
	}
	return nil
}
