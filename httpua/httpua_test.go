package httpua

import (
	"net/http"
	"strings"
	"testing"
)

func TestSet_AddsDefaultWhenAbsent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	Set(req)
	if got := req.Header.Get("User-Agent"); got != Default {
		t.Fatalf("Set 应在缺省时填默认浏览器 UA；got %q", got)
	}
}

func TestSet_PreservesExplicitUserAgent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("User-Agent", "custom-agent/1.0")
	Set(req)
	if got := req.Header.Get("User-Agent"); got != "custom-agent/1.0" {
		t.Fatalf("Set 不应覆盖调用方显式设置的 UA；got %q", got)
	}
}

func TestDefault_IsBrowserLike_NotGoDefault(t *testing.T) {
	if Default == "" || strings.Contains(Default, "Go-http-client") {
		t.Fatalf("Default 必须是浏览器 UA、非 Go 默认 Go-http-client；got %q", Default)
	}
	if !strings.HasPrefix(Default, "Mozilla/") {
		t.Fatalf("Default 应遵循浏览器 UA 惯例（Mozilla/ 开头）；got %q", Default)
	}
}
