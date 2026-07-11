package render

import (
	"context"
	"io"
	"net/http"
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
	client := &http.Client{Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return imageResponse(req), nil
	})}
	in := "![pic](http://93.184.216.34/image.png)"
	out, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{
		HTTPClient:      client,
		PerImageTimeout: time.Second,
		MaxImageBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("远程图片应被正常内联: %v", err)
	}
	if !strings.Contains(out, "data:image/png;base64,") {
		t.Errorf("图片未内联为 data URL: %s", out)
	}
}

func TestPreprocessMarkdown_OversizedImageRejected(t *testing.T) {
	// 响应体超过 MaxImageBytes，应被 CodeInputTooLarge 拒绝。
	client := &http.Client{Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		// 写超过 cfg.MaxImageBytes 的内容
		big := make([]byte, 100)
		for i := range big {
			big[i] = 'X'
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(strings.NewReader(string(big))),
			Request:    req,
		}, nil
	})}

	in := "![](http://93.184.216.34/image.png)"
	_, err := PreprocessMarkdown(context.Background(), in, PreprocessConfig{
		HTTPClient:      client,
		PerImageTimeout: time.Second,
		MaxImageBytes:   50, // 小于响应体
	})
	if err == nil {
		t.Error("expected error (image too large)")
	} else if renderErr, ok := err.(*RenderError); !ok || renderErr.Code != CodeInputTooLarge {
		t.Fatalf("oversized image error = %T %v, want %s", err, err, CodeInputTooLarge)
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
		`![alt](url)`:                       "url",
		`![alt with spaces](url)`:           "url",
		`![](url)`:                          "url",
		`![alt](https://example.com/p.png)`: "https://example.com/p.png",
		`![alt](url "title")`:               "url",
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
