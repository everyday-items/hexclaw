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
	if bin := findTool("pdftotext", pdftotextKnownPaths...); bin != "" {
		if text, err := runToolOnTemp(ctx, bin, ".pdf", data, "-enc", "UTF-8", "-q", "{}", "-"); err == nil {
			if strings.TrimSpace(text) != "" {
				return text, 0, nil
			}
		}
	}
	docs, err := loader.NewPDFLoaderFromReader(bytes.NewReader(data)).Load(ctx)
	if err != nil {
		return "", 0, err
	}
	var sb strings.Builder
	pageCount := 0
	for i, d := range docs {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(d.Content)
	}
	if len(docs) > 0 {
		if pc, ok := docs[0].Metadata["page_count"].(int); ok {
			pageCount = pc
		}
	}
	return sb.String(), pageCount, nil
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
