package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bug 2026-06-22（AP-016 同类）：engine 图片下载 downloadAsDataURI（走 toolkit httpx.RawClient）
// 不带默认 User-Agent，反爬站对 Go 默认 UA 返回 HTML，多模态附件里塞进 HTML 而非图片。
// UA 分流服务器复现：非浏览器 UA → text/html；浏览器 UA → image/png。
// 修复前 → data:text/html（RED）；修复后默认浏览器 UA → data:image/png（GREEN）。
func TestBug20260622_ImageDownload_SetsDefaultUserAgent(t *testing.T) {
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

	got, err := downloadAsDataURI(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("downloadAsDataURI: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png") {
		t.Fatalf("downloadAsDataURI 应带默认浏览器 User-Agent；got %.48q", got)
	}
}
