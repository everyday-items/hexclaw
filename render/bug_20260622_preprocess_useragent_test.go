package render

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Bug 2026-06-22（AP-016 同类）：render 预处理 fetchToDataURL 拉取图片 URL 时不带默认
// User-Agent，反爬站对 Go 默认 UA 返回 HTML，导致内嵌的"图片"其实是 HTML 页面（mime 错位）。
// UA 分流服务器精确复现：非浏览器 UA → text/html；浏览器 UA → image/png。
// 修复前 fetchToDataURL 拿到 HTML → data:text/html（RED）；修复后默认浏览器 UA → image/png（GREEN）。
func TestBug20260622_PreprocessFetch_SetsDefaultUserAgent(t *testing.T) {
	client := &http.Client{Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		contentType := "image/png"
		body := "\x89PNG\r\n\x1a\nfake-png-bytes"
		ua := req.Header.Get("User-Agent")
		if ua == "" || strings.Contains(ua, "Go-http-client") {
			contentType = "text/html"
			body = "<!DOCTYPE html><html><body>anti-bot</body></html>"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	got, err := fetchToDataURL(context.Background(), "http://93.184.216.34/image.png", client, 1<<20)
	if err != nil {
		t.Fatalf("fetchToDataURL: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png") {
		t.Fatalf("fetchToDataURL 应带默认浏览器 User-Agent 取到真实图片，避免反爬站返回 HTML 致 mime 错位；got %.48q", got)
	}
}
