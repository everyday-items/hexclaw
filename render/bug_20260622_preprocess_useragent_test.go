package render

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bug 2026-06-22（AP-016 同类）：render 预处理 fetchToDataURL 拉取图片 URL 时不带默认
// User-Agent，反爬站对 Go 默认 UA 返回 HTML，导致内嵌的"图片"其实是 HTML 页面（mime 错位）。
// UA 分流服务器精确复现：非浏览器 UA → text/html；浏览器 UA → image/png。
// 修复前 fetchToDataURL 拿到 HTML → data:text/html（RED）；修复后默认浏览器 UA → image/png（GREEN）。
func TestBug20260622_PreprocessFetch_SetsDefaultUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if ua == "" || strings.Contains(ua, "Go-http-client") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!DOCTYPE html><html><body>anti-bot</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake-png-bytes"))
	}))
	defer srv.Close()

	got, err := fetchToDataURL(context.Background(), srv.URL, &http.Client{}, 1<<20)
	if err != nil {
		t.Fatalf("fetchToDataURL: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png") {
		t.Fatalf("fetchToDataURL 应带默认浏览器 User-Agent 取到真实图片，避免反爬站返回 HTML 致 mime 错位；got %.48q", got)
	}
}
