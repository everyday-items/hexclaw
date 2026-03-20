package builtin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

func TestBrowserSkillFetch(t *testing.T) {
	s := &BrowserSkill{
		client: httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><title>Test Page</title></head><body><p>Hello World</p></body></html>`))
		})),
		allowPrivate: false,
	}
	result, err := s.Execute(context.Background(), map[string]any{
		"action": "fetch",
		"url":    "https://example.com/test",
	})
	if err != nil {
		t.Fatalf("fetch 失败: %v", err)
	}
	if !strings.Contains(result.Content, "Hello World") {
		t.Errorf("内容不包含 'Hello World': %s", result.Content)
	}
	if result.Metadata["status"] != "200" {
		t.Errorf("状态码不是 200: %s", result.Metadata["status"])
	}
}

func TestBrowserSkillExtract(t *testing.T) {
	html := `<html>
<head>
	<title>测试页面</title>
	<meta name="description" content="这是一个测试页面">
</head>
<body>
	<a href="/page1">链接1</a>
	<a href="https://example.com">链接2</a>
	<p>正文内容</p>
</body>
</html>`

	s := &BrowserSkill{
		client: httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(html))
		})),
		allowPrivate: false,
	}

	// 提取标题
	result, err := s.Execute(context.Background(), map[string]any{
		"action":   "extract",
		"url":      "https://example.com/page",
		"selector": "title",
	})
	if err != nil {
		t.Fatalf("extract title 失败: %v", err)
	}
	if result.Content != "测试页面" {
		t.Errorf("标题不匹配: %s", result.Content)
	}

	// 提取 meta
	result, err = s.Execute(context.Background(), map[string]any{
		"action":   "extract",
		"url":      "https://example.com/page",
		"selector": "meta",
	})
	if err != nil {
		t.Fatalf("extract meta 失败: %v", err)
	}
	if result.Content != "这是一个测试页面" {
		t.Errorf("meta 不匹配: %s", result.Content)
	}

	// 提取链接
	result, err = s.Execute(context.Background(), map[string]any{
		"action":   "extract",
		"url":      "https://example.com/page",
		"selector": "links",
	})
	if err != nil {
		t.Fatalf("extract links 失败: %v", err)
	}
	if !strings.Contains(result.Content, "example.com") {
		t.Errorf("链接不包含 example.com: %s", result.Content)
	}
}

func TestBrowserSkillMatch(t *testing.T) {
	s := NewBrowserSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"fetch url https://example.com", true},
		{"打开网页 example.com", true},
		{"获取网页内容", true},
		{"你好", false},
		{"天气怎么样", false},
	}

	for _, tt := range tests {
		if got := s.Match(tt.input); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBrowserSkillMissingURL(t *testing.T) {
	s := NewBrowserSkill()
	_, err := s.Execute(context.Background(), map[string]any{
		"action": "fetch",
	})
	if err == nil {
		t.Error("缺少 URL 应报错")
	}
}

func TestBrowserSkillInvalidURL(t *testing.T) {
	s := NewBrowserSkill()
	_, err := s.Execute(context.Background(), map[string]any{
		"url": "ftp://invalid.com",
	})
	if err == nil {
		t.Error("无效 URL scheme 应报错")
	}
}

func TestBrowserSkillSSRF(t *testing.T) {
	s := NewBrowserSkill() // allowPrivate=false (默认)

	tests := []struct {
		name string
		url  string
	}{
		{"localhost", "http://localhost:8080/secret"},
		{"loopback", "http://127.0.0.1/secret"},
		{"private 10.x", "http://10.0.0.1/secret"},
		{"private 172.16.x", "http://172.16.0.1/secret"},
		{"private 192.168.x", "http://192.168.1.1/secret"},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"link-local", "http://169.254.1.1/"},
		{"CGN", "http://100.64.0.1/"},
		{"IPv6 loopback", "http://[::1]/secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Execute(context.Background(), map[string]any{
				"action": "fetch",
				"url":    tt.url,
			})
			if err == nil {
				t.Errorf("应阻止访问 %s", tt.url)
			}
			if !strings.Contains(err.Error(), "禁止访问内网地址") {
				t.Errorf("错误信息不正确: %v", err)
			}
		})
	}
}

func TestIsPrivateHost(t *testing.T) {
	privateHosts := []string{
		"localhost", "127.0.0.1", "10.0.0.1", "172.16.0.1",
		"192.168.1.1", "169.254.169.254", "100.64.0.1",
		"metadata.google.internal", "::1",
	}
	for _, h := range privateHosts {
		if !isPrivateHost(h) {
			t.Errorf("应识别 %s 为内网地址", h)
		}
	}

	publicHosts := []string{
		"example.com", "8.8.8.8", "1.1.1.1", "github.com",
	}
	for _, h := range publicHosts {
		if isPrivateHost(h) {
			t.Errorf("不应将 %s 识别为内网地址", h)
		}
	}
}

func TestStripHTML(t *testing.T) {
	html := `<html><head><script>alert('xss')</script><style>.a{color:red}</style></head><body><p>Hello &amp; World</p></body></html>`
	text := stripHTML(html)
	if strings.Contains(text, "alert") {
		t.Error("script 应被移除")
	}
	if strings.Contains(text, "color") {
		t.Error("style 应被移除")
	}
	if !strings.Contains(text, "Hello & World") {
		t.Errorf("实体解码不正确: %s", text)
	}
}
