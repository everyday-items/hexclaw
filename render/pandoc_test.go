package render

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requirePandoc 跳过测试 if pandoc 不可用。
// 这让 CI 在没装 pandoc 的机器上仍能跑过其它测试。
func requirePandoc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc not installed, skipping render integration test")
	}
}

func newTestRenderer(t *testing.T) *PandocRenderer {
	t.Helper()
	r, err := NewPandocRenderer("", "", filepath.Join(t.TempDir(), "sandbox"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPandocRenderer_FormatValidation(t *testing.T) {
	r := newTestRenderer(t)
	ctx := context.Background()

	_, err := r.Render(ctx, "hello", Format("xyz"), RenderOptions{})
	re, ok := err.(*RenderError)
	if !ok {
		t.Fatalf("expected RenderError, got %T", err)
	}
	if re.Code != CodeFormatUnsupported {
		t.Errorf("got code %s, want %s", re.Code, CodeFormatUnsupported)
	}
}

func TestPandocRenderer_HTML(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	result, err := r.Render(context.Background(),
		"# 标题\n\n一段中文段落 with English mix",
		FormatHTML, RenderOptions{Locale: "zh-CN"})
	if err != nil {
		t.Fatalf("render html: %v", err)
	}
	defer os.Remove(result.Path)

	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	// 自包含 HTML 应有 <!DOCTYPE
	if !strings.Contains(body, "<!DOCTYPE") {
		t.Error("html missing <!DOCTYPE (expected --standalone)")
	}
	// 中文应原样保留
	if !strings.Contains(body, "标题") {
		t.Error("html missing CJK content")
	}
	// 标题层级应转 <h1>
	if !strings.Contains(body, "<h1") {
		t.Error("html missing <h1>")
	}
	// MIME 正确
	if result.ContentType != "text/html; charset=utf-8" {
		t.Errorf("content type = %s", result.ContentType)
	}
}

func TestPandocRenderer_Docx(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	result, err := r.Render(context.Background(),
		"# Hello\n\nA paragraph.",
		FormatDocx, RenderOptions{})
	if err != nil {
		t.Fatalf("render docx: %v", err)
	}
	defer os.Remove(result.Path)

	if result.Size <= 0 {
		t.Error("docx size = 0")
	}
	if result.ContentType != FormatDocx.MIME() {
		t.Errorf("content type mismatch: %s", result.ContentType)
	}
	// docx 是 zip，magic bytes 应是 PK
	data, _ := os.ReadFile(result.Path)
	if len(data) < 2 || data[0] != 'P' || data[1] != 'K' {
		t.Error("docx output is not a valid zip (missing PK magic)")
	}
}

func TestPandocRenderer_RawHTMLDisabled(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	// 默认 raw_html 禁用，<script> 不应进入 html 输出
	result, err := r.Render(context.Background(),
		"hello\n\n<script>alert(1)</script>\n\nworld",
		FormatHTML, RenderOptions{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer os.Remove(result.Path)

	data, _ := os.ReadFile(result.Path)
	if strings.Contains(string(data), "<script>alert(1)</script>") {
		t.Error("raw <script> leaked into output despite default disable")
	}
}

func TestPandocRenderer_Determinism(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)
	ctx := context.Background()

	// 同 markdown 两次渲染 docx，core.xml 时间戳应已 normalized
	// 不强求 byte equality（zip 顺序仍可能微差），只验 core.xml 时间戳一致
	r1, err := r.Render(ctx, "hello", FormatDocx, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(r1.Path)

	r2, err := r.Render(ctx, "hello", FormatDocx, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(r2.Path)

	core1 := readCoreXML(t, r1.Path)
	core2 := readCoreXML(t, r2.Path)
	if string(core1) != string(core2) {
		t.Errorf("core.xml differs between two renders of same input")
	}
	// 时间戳应是 1970-01-01
	if !strings.Contains(string(core1), "1970-01-01") {
		t.Error("core.xml timestamp not normalized to 1970-01-01")
	}
}

// readCoreXML 从 docx zip 中读 docProps/core.xml
func readCoreXML(t *testing.T, path string) []byte {
	t.Helper()
	out, err := exec.Command("unzip", "-p", path, "docProps/core.xml").Output()
	if err != nil {
		// unzip 可能不可用；用纯 Go 的 zip 包
		return nil
	}
	return out
}

func TestPandocRenderer_EmptySandboxDirRejected(t *testing.T) {
	_, err := NewPandocRenderer("", "", "")
	if err == nil {
		t.Error("expected error for empty SandboxDir")
	}
}

// regression: macOS GUI app spawn 时 sidecar CWD=/（只读根），pandoc 处理多媒体
// 或 PDF 引擎时会创建 ./media-* 相对路径目录，必须把子进程 CWD 钉到 SandboxDir。
// 本用例 chdir 到只读路径模拟那个环境，确认渲染仍然成功。
func TestPandocRenderer_RenderSucceedsFromReadOnlyCWD(t *testing.T) {
	requirePandoc(t)
	r := newTestRenderer(t)

	// 保存原 CWD，结束后恢复
	origCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origCWD) }()

	// chdir 到 / —— macOS root 通常只读；即便不是只读，pandoc 也无权在 / 写
	if err := os.Chdir("/"); err != nil {
		t.Skipf("can't chdir to /: %v", err)
	}

	result, err := r.Render(context.Background(),
		"# 标题\n\n中文段落",
		FormatHTML, RenderOptions{})
	if err != nil {
		t.Fatalf("render from RO CWD failed (regression: cmd.Dir 没钉到 SandboxDir?): %v", err)
	}
	defer os.Remove(result.Path)
	if result.Size == 0 {
		t.Error("empty output from RO CWD")
	}
}
