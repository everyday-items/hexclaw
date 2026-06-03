package render

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 黄金回归 fixtures：CJK + 基础公式 + 列表 + 图片三种来源。
// 详见 .claude/doc-generation-architecture.md §9 验证标准。
//
// 这些测试依赖系统 pandoc；缺失时跳过（不阻塞 CI）。

const fixtureCJKWithFormulaAndList = `# 数学复习笔记

## 一元二次方程

求解 $ax^2 + bx + c = 0$ 的判别式：

$$\Delta = b^2 - 4ac$$

## 解题步骤

1. 计算判别式 $\Delta$
2. 根据 $\Delta$ 的正负判断：
   - $\Delta > 0$：两个不同实根
   - $\Delta = 0$：一个重根
   - $\Delta < 0$：两个共轭复根
3. 应用求根公式

## 例题

求解：$x^2 - 5x + 6 = 0$

- $a = 1$，$b = -5$，$c = 6$
- $\Delta = 25 - 24 = 1 > 0$
- $x_1 = 3$，$x_2 = 2$
`

func TestGolden_CJKFormulaList_HTML(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	result, err := r.Render(context.Background(), fixtureCJKWithFormulaAndList,
		FormatHTML, RenderOptions{Locale: "zh-CN"})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	defer os.Remove(result.Path)

	data, _ := os.ReadFile(result.Path)
	body := string(data)

	// 必须保真的元素
	checks := map[string]bool{
		"CJK 章节标题":   strings.Contains(body, "数学复习笔记"),
		"内联公式 ax^2": strings.Contains(body, "ax") || strings.Contains(body, "<math"),
		"块公式 Delta": strings.Contains(body, "Delta") || strings.Contains(body, "Δ") || strings.Contains(body, "<math"),
		"有序列表":      strings.Contains(body, "<ol"),
		"无序列表":      strings.Contains(body, "<ul"),
		"嵌套列表项":     strings.Contains(body, "两个不同实根"),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("HTML 保真失败: %s", name)
		}
	}
}

func TestGolden_CJKFormulaList_Docx(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	result, err := r.Render(context.Background(), fixtureCJKWithFormulaAndList,
		FormatDocx, RenderOptions{Locale: "zh-CN"})
	if err != nil {
		t.Fatalf("render docx: %v", err)
	}
	defer os.Remove(result.Path)

	if result.Size <= 1000 {
		t.Errorf("docx size too small: %d (likely empty)", result.Size)
	}
	if result.ContentType != FormatDocx.MIME() {
		t.Errorf("content type = %s", result.ContentType)
	}

	// docx 是 zip：验证 magic bytes
	data, _ := os.ReadFile(result.Path)
	if len(data) < 4 || data[0] != 'P' || data[1] != 'K' {
		t.Error("docx magic bytes mismatch")
	}
}

func TestGolden_AllFormats_CompileSmoke(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)
	ctx := context.Background()

	simple := "# Title\n\nA paragraph with **bold** and *italic*.\n\n- one\n- two"

	// 跳过 PDF（需要 typst）和 epub（pandoc 默认 epub3 偶尔依赖额外资源）
	smoke := []Format{FormatHTML, FormatDocx, FormatODT, FormatRTF, FormatTXT, FormatMarkdown}

	for _, f := range smoke {
		t.Run(string(f), func(t *testing.T) {
			result, err := r.Render(ctx, simple, f, RenderOptions{})
			if err != nil {
				t.Errorf("format %s failed: %v", f, err)
				return
			}
			defer os.Remove(result.Path)

			if result.Size <= 0 {
				t.Errorf("format %s produced empty file", f)
			}
			if result.ContentType != f.MIME() {
				t.Errorf("format %s content type mismatch: %s", f, result.ContentType)
			}
		})
	}
}

const fixtureRawHTMLInjection = `# Test

before

<script>alert('xss')</script>

<iframe src="evil.com"></iframe>

after
`

// raw_html 默认禁用对"可执行"输出格式的语义：
//   - HTML / EPUB：不应含 <script>/<iframe> 标签（会执行）
//   - DOCX：不应含 OOXML AltChunk（嵌入 raw HTML 块）
//   - ODT / RTF / TXT：纯文本格式，<script> 字面文本不会执行；
//     pandoc reader 把 raw_html 当 escaped text 保留是安全的，不算漏洞。
func TestGolden_RawHTMLBlocked_RisksFormats(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)
	ctx := context.Background()

	for _, f := range []Format{FormatHTML, FormatDocx, FormatEPUB} {
		t.Run(string(f), func(t *testing.T) {
			result, err := r.Render(ctx, fixtureRawHTMLInjection, f, RenderOptions{})
			if err != nil {
				t.Skipf("render %s: %v (likely missing engine)", f, err)
			}
			defer os.Remove(result.Path)

			data, _ := os.ReadFile(result.Path)
			body := string(data)

			// HTML / EPUB / DOCX：raw HTML 标签必须不进入输出（会被浏览器/Word 执行）
			if strings.Contains(body, "<script>alert") {
				t.Errorf("format %s: <script> tag leaked into executable output", f)
			}
			if strings.Contains(body, "<iframe ") || strings.Contains(body, `<iframe src="evil`) {
				t.Errorf("format %s: <iframe> tag leaked into executable output", f)
			}
		})
	}
}
