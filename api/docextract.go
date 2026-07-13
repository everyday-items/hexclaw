package api

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/hexagon/rag/loader"
)

// 文档文本抽取：在桌面 WKWebView 前端无法可靠解析（PDF 需 Web Worker、老 .doc 需原生工具）的格式
// 统一下沉后端处理。PDF 用 poppler pdftotext（CJK/ToUnicode 完整，解决中文乱码）、老 .doc 用 macOS
// textutil、.pptx 复用 hexagon PPTXLoader；.docx 见 handler_knowledge.go 的 extractDocxText。

// findTool 解析外部工具可执行路径：
//  1. 环境变量覆盖 HEXCLAW_<NAME>（打包时注入内嵌二进制路径）
//  2. 与本进程同目录（sidecar 旁内嵌）
//  3. PATH
//  4. 已知系统路径
func findTool(name string, knownPaths ...string) string {
	if env := os.Getenv("HEXCLAW_" + strings.ToUpper(name)); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), name)
		if st, statErr := os.Stat(bundled); statErr == nil && !st.IsDir() {
			return bundled
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, c := range knownPaths {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// runToolOnTemp 把 data 落临时文件后调用 bin（args 中的 "{}" 占位替换为临时文件路径），返回 stdout。
func runToolOnTemp(ctx context.Context, bin, ext string, data []byte, args ...string) (string, error) {
	f, err := os.CreateTemp("", "hexdoc-*"+ext)
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	resolved := make([]string, len(args))
	for i, a := range args {
		if a == "{}" {
			resolved[i] = tmp
		} else {
			resolved[i] = a
		}
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, resolved...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s 执行失败: %w: %s", filepath.Base(bin), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

var pdftotextKnownPaths = []string{"/opt/homebrew/bin/pdftotext", "/usr/local/bin/pdftotext", "/usr/bin/pdftotext"}

// extractPDFText 优先用 poppler pdftotext（CJK 完整）；不可用/失败时降级 hexagon rag/loader
// （纯 Go、无外部依赖，简单 PDF 可用，复杂中文 PDF 可能乱码——故仅作兜底）。
func extractPDFText(ctx context.Context, data []byte) (string, int, error) {
	pdftotextPageCount := 0
	if bin := findTool("pdftotext", pdftotextKnownPaths...); bin != "" {
		if text, err := runToolOnTemp(ctx, bin, ".pdf", data, "-enc", "UTF-8", "-q", "{}", "-"); err == nil {
			normalized, pageCount := normalizeExtractedPDFText(text)
			pdftotextPageCount = pageCount
			if strings.TrimSpace(text) != "" {
				return normalized, pageCount, nil
			}
			// 扫描版 PDF 的 pdftotext 可能只返回分页符；先记下页数，但仍继续走
			// 纯 Go loader 兜底。部分简单 PDF 没有字体映射，pdftotext 为空而 loader
			// 仍能读到正文，不能因拿到了页数就提前返回空文本。
		}
	}
	docs, err := loader.NewPDFLoaderFromReader(bytes.NewReader(data)).Load(ctx)
	if err != nil {
		return "", pdftotextPageCount, err
	}
	var raw strings.Builder
	pageCount := 0
	for i, d := range docs {
		if i > 0 {
			raw.WriteByte('\f')
		}
		raw.WriteString(d.Content)
	}
	if len(docs) > 0 {
		if pc, ok := docs[0].Metadata["page_count"].(int); ok {
			pageCount = pc
		}
	}
	normalized, inferredPages := normalizeExtractedPDFText(raw.String())
	if pageCount <= 0 {
		pageCount = inferredPages
	}
	if pageCount <= 0 {
		pageCount = pdftotextPageCount
	}
	return normalized, pageCount, nil
}

// normalizeExtractedPDFText 把 pdftotext 的 form-feed 分页符转换为 Markdown 页标题，
// 同时返回真实页数。这样 MarkdownSplitter 会把“第 N 页”写入 header_path，检索结果
// 能定位页码；也让摄取层可以针对百页文档选择有界的视觉解析策略。
func normalizeExtractedPDFText(raw string) (string, int) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	trimmedLineEnd := strings.TrimRight(raw, " \t\n")
	separatorCount := strings.Count(raw, "\f")
	pageCount := separatorCount
	if !strings.HasSuffix(trimmedLineEnd, "\f") && strings.TrimSpace(raw) != "" {
		pageCount++
	}
	if pageCount == 0 {
		return strings.TrimSpace(raw), 0
	}

	pages := strings.Split(raw, "\f")
	if pageCount == 1 {
		return strings.TrimSpace(pages[0]), pageCount
	}
	var b strings.Builder
	for i, page := range pages {
		if i >= pageCount {
			break
		}
		page = strings.TrimSpace(page)
		if page == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## PDF 第 %d 页\n\n%s", i+1, page)
	}
	return b.String(), pageCount
}

// extractDOCText 解析老版 .doc（OLE2 二进制）：用 macOS 内置 textutil（CJK 安全）。
// 无成熟跨平台纯 Go 方案，其他平台暂不支持，提示用户转存 .docx。
func extractDOCText(ctx context.Context, data []byte) (string, error) {
	bin := findTool("textutil", "/usr/bin/textutil")
	if bin == "" {
		return "", fmt.Errorf("当前平台暂不支持解析老版 .doc，请另存为 .docx 后再上传")
	}
	return runToolOnTemp(ctx, bin, ".doc", data, "-convert", "txt", "-encoding", "UTF-8", "-stdout", "{}")
}

// extractPPTXText 复用 hexagon PPTXLoader（OOXML 即 UTF-8 XML，CJK 安全）。其 API 仅吃文件路径，落临时文件。
func extractPPTXText(ctx context.Context, data []byte) (string, error) {
	f, err := os.CreateTemp("", "hexdoc-*.pptx")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	docs, err := loader.NewPPTXLoader(tmp).Load(ctx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, d := range docs {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(d.Content)
	}
	return sb.String(), nil
}
