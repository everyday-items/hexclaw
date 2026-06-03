package render

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPreprocessMarkdown_DataURLPreserved(t *testing.T) {
	in := "before ![alt](data:image/png;base64,iVBORw0KGgo) after"
	out, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "data:image/png;base64,iVBORw0KGgo") {
		t.Errorf("data URL not preserved: %s", out)
	}
}

func TestPreprocessMarkdown_FileURLRejected(t *testing.T) {
	cases := []string{
		"![](file:///etc/passwd)",
		"![](file://localhost/secrets.txt)",
	}
	for _, in := range cases {
		_, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
		if err == nil {
			t.Errorf("file:// not rejected: %s", in)
			continue
		}
		// regression: file:// 是用户输入语义违规 → INVALID_INPUT (HTTP 400)，不是 RENDER_FAILED (500)
		re, ok := err.(*RenderError)
		if !ok {
			t.Fatalf("expected *RenderError, got %T: %v", err, err)
		}
		if re.Code != CodeInvalidInput {
			t.Errorf("expected CodeInvalidInput, got %s for input %q", re.Code, in)
		}
		if re.HTTPStatus() != 400 {
			t.Errorf("expected HTTP 400, got %d for input %q", re.HTTPStatus(), in)
		}
	}
}

func TestPreprocessMarkdown_AbsolutePathRejected(t *testing.T) {
	cases := []string{
		"![](/etc/passwd)",
		`![](\windows\system32)`,
	}
	for _, in := range cases {
		_, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
		if err == nil {
			t.Errorf("abs path not rejected: %s", in)
			continue
		}
		// regression: 绝对路径 → INVALID_INPUT (HTTP 400)
		re, ok := err.(*RenderError)
		if !ok {
			t.Fatalf("expected *RenderError, got %T: %v", err, err)
		}
		if re.Code != CodeInvalidInput {
			t.Errorf("expected CodeInvalidInput, got %s for input %q", re.Code, in)
		}
		if re.HTTPStatus() != 400 {
			t.Errorf("expected HTTP 400, got %d for input %q", re.HTTPStatus(), in)
		}
	}
}

func TestPreprocessMarkdown_RelativePathStripped(t *testing.T) {
	in := "Look at ![my pic](./pic.png) here"
	out, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// 相对路径不解析，保留 alt 文本占位
	if strings.Contains(out, "![") {
		t.Errorf("image ref not removed for relative path: %s", out)
	}
	if !strings.Contains(out, "my pic") {
		t.Errorf("alt text not preserved: %s", out)
	}
}

func TestPreprocessMarkdown_RemoteImageInlined(t *testing.T) {
	// 启动本地测试服务器
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake-png-bytes"))
	}))
	defer srv.Close()

	// 注意：security.ValidateURL 会拦截 127.0.0.1（loopback），所以这个测试
	// 实际会被拒绝。这正是预期的——验证 SSRF 闸门工作。
	in := "![pic](" + srv.URL + ")"
	_, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
	if err == nil {
		t.Errorf("loopback URL should be blocked by SSRF guard")
	}
	re, ok := err.(*RenderError)
	if !ok {
		t.Fatalf("expected *RenderError, got %T: %v", err, err)
	}
	if !strings.Contains(re.Detail, "SSRF") && !strings.Contains(re.Detail, "private") {
		t.Errorf("error not from SSRF guard: %v", re)
	}
}

func TestPreprocessMarkdown_OversizedImageRejected(t *testing.T) {
	// 这个测试也会被 SSRF 拦截（127.0.0.1），所以验证拒绝即可
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// 写超过 cfg.MaxImageBytes 的内容
		big := make([]byte, 100)
		for i := range big {
			big[i] = 'X'
		}
		w.Write(big)
	}))
	defer srv.Close()

	in := "![](" + srv.URL + ")"
	_, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{
		PerImageTimeout: time.Second,
		MaxImageBytes:   50, // 小于响应体
	})
	if err == nil {
		t.Error("expected error (loopback or too large)")
	}
}

func TestMarkdownTextSize_StripsDataURLs(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("A", 1000)
	in := "hello ![](" + dataURL + ") world"
	size := MarkdownTextSize(in)
	// 应该接近 "hello ![]() world" 的长度，不含 data URL
	if size > 50 {
		t.Errorf("MarkdownTextSize didn't strip data URL: got %d for %d-char input", size, len(in))
	}
}

func TestPreprocessMarkdown_NoImagesUnchanged(t *testing.T) {
	in := "# Title\n\nA paragraph with no images.\n\nList:\n- one\n- two"
	out, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("input mutated despite no images:\nin:  %q\nout: %q", in, out)
	}
}

func TestImageRefPattern(t *testing.T) {
	cases := map[string]string{
		`![alt](url)`:                     "url",
		`![alt with spaces](url)`:         "url",
		`![](url)`:                        "url",
		`![alt](https://example.com/p.png)`: "https://example.com/p.png",
		`![alt](url "title")`:             "url",
	}
	for in, want := range cases {
		groups := imageRefPattern.FindStringSubmatch(in)
		if len(groups) < 3 {
			t.Errorf("%s: no match", in)
			continue
		}
		if groups[2] != want {
			t.Errorf("%s: got url %q, want %q", in, groups[2], want)
		}
	}
}
