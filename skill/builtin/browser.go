package builtin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/skill"
)

// BrowserSkill 网页浏览技能
//
// 支持:
//   - fetch: 获取网页文本内容
//   - extract: 提取标题、链接、元描述
//   - post: 提交表单数据
type BrowserSkill struct {
	client       *http.Client
	allowPrivate bool // 仅测试用：允许访问内网地址
}

// NewBrowserSkill 创建浏览器技能
func NewBrowserSkill() *BrowserSkill {
	// 使用 safe dialer 防止 DNS rebinding 绕过 SSRF 检查
	safeDialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// DNS 解析后检查 IP
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ipAddr := range ips {
				if isPrivateIP(ipAddr.IP) {
					return nil, fmt.Errorf("禁止连接内网地址: %s -> %s", host, ipAddr.IP)
				}
			}
			// 使用解析后的第一个 IP 直接连接
			return safeDialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &BrowserSkill{
		client: &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}
}

func (s *BrowserSkill) Name() string        { return "browser" }
func (s *BrowserSkill) Description() string  { return "网页获取、内容提取和表单提交" }

func (s *BrowserSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	keywords := []string{"fetch url", "打开网页", "获取网页", "browse ", "extract from"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func (s *BrowserSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	action, _ := args["action"].(string)
	targetURL, _ := args["url"].(string)

	if targetURL == "" {
		// 尝试从 query 中提取 URL
		if query, ok := args["query"].(string); ok {
			targetURL = extractURL(query)
		}
	}

	if targetURL == "" {
		return nil, fmt.Errorf("缺少 url 参数")
	}

	// 验证 URL
	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("无效 URL: %s", targetURL)
	}

	// SSRF 防护：屏蔽内网/保留地址
	if !s.allowPrivate && isPrivateHost(parsed.Hostname()) {
		return nil, fmt.Errorf("禁止访问内网地址: %s", parsed.Hostname())
	}

	switch action {
	case "post":
		return s.doPost(ctx, targetURL, args)
	case "extract":
		return s.doExtract(ctx, targetURL, args)
	default: // "fetch" or empty
		return s.doFetch(ctx, targetURL)
	}
}

const maxBodySize = 1 << 20 // 1MB

func (s *BrowserSkill) doFetch(ctx context.Context, targetURL string) (*skill.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "HexClaw/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	text := stripHTML(string(body))

	return &skill.Result{
		Content: text,
		Metadata: map[string]string{
			"url":         targetURL,
			"status":      fmt.Sprintf("%d", resp.StatusCode),
			"content_type": resp.Header.Get("Content-Type"),
		},
	}, nil
}

func (s *BrowserSkill) doExtract(ctx context.Context, targetURL string, args map[string]any) (*skill.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "HexClaw/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	html := string(body)

	selector, _ := args["selector"].(string)
	var content string

	switch selector {
	case "title":
		content = extractTitle(html)
	case "links":
		links := extractLinks(html, targetURL)
		content = strings.Join(links, "\n")
	case "meta":
		content = extractMetaDescription(html)
	default: // "text"
		content = stripHTML(html)
	}

	return &skill.Result{
		Content: content,
		Data: map[string]any{
			"title":       extractTitle(html),
			"description": extractMetaDescription(html),
			"link_count":  len(extractLinks(html, targetURL)),
		},
		Metadata: map[string]string{
			"url":    targetURL,
			"action": "extract",
		},
	}, nil
}

func (s *BrowserSkill) doPost(ctx context.Context, targetURL string, args map[string]any) (*skill.Result, error) {
	data, _ := args["data"].(map[string]string)
	if data == nil {
		return nil, fmt.Errorf("post 请求缺少 data 参数")
	}

	form := url.Values{}
	for k, v := range data {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "HexClaw/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return &skill.Result{
		Content: stripHTML(string(body)),
		Metadata: map[string]string{
			"url":    targetURL,
			"status": fmt.Sprintf("%d", resp.StatusCode),
		},
	}, nil
}

// HTML 处理辅助函数

var (
	reTag      = regexp.MustCompile(`<[^>]*>`)
	reSpace    = regexp.MustCompile(`\s{2,}`)
	reScript   = regexp.MustCompile(`(?is)<script.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style.*?</style>`)
	reTitle    = regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)
	reLink     = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["']`)
	reMeta     = regexp.MustCompile(`(?i)<meta[^>]+name=["']description["'][^>]+content=["']([^"']*)["']`)
	reMetaAlt  = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"']*)["'][^>]+name=["']description["']`)
	reURL      = regexp.MustCompile(`https?://[^\s<>"']+`)
)

func stripHTML(html string) string {
	// 移除 script 和 style（使用包级预编译正则）
	html = reScript.ReplaceAllString(html, "")
	html = reStyle.ReplaceAllString(html, "")

	text := reTag.ReplaceAllString(html, " ")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = reSpace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func extractTitle(html string) string {
	matches := reTitle.FindStringSubmatch(html)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func extractLinks(html, baseURL string) []string {
	matches := reLink.FindAllStringSubmatch(html, -1)
	var links []string
	seen := make(map[string]bool)
	base, _ := url.Parse(baseURL)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		href := m[1]
		if strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
			continue
		}
		// 解析相对 URL
		if parsed, err := url.Parse(href); err == nil && base != nil {
			href = base.ResolveReference(parsed).String()
		}
		if !seen[href] {
			seen[href] = true
			links = append(links, href)
		}
	}
	return links
}

func extractMetaDescription(html string) string {
	matches := reMeta.FindStringSubmatch(html)
	if len(matches) > 1 {
		return matches[1]
	}
	matches = reMetaAlt.FindStringSubmatch(html)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func extractURL(text string) string {
	match := reURL.FindString(text)
	return match
}

// isPrivateHost 检查是否为内网/保留地址（SSRF 防护）
func isPrivateHost(host string) bool {
	// 特殊主机名
	switch strings.ToLower(host) {
	case "localhost", "":
		return true
	}

	// 云元数据地址
	if host == "169.254.169.254" || host == "metadata.google.internal" {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// 可能带端口，尝试解析
		h, _, err := net.SplitHostPort(host)
		if err == nil {
			ip = net.ParseIP(h)
		}
	}
	if ip == nil {
		return false // 域名，允许（DNS 解析可能指向内网，但这是基础防护）
	}

	// RFC 1918 / RFC 6598 / 回环 / 链路本地
	privateRanges := []struct {
		network string
	}{
		{"10.0.0.0/8"},
		{"172.16.0.0/12"},
		{"192.168.0.0/16"},
		{"127.0.0.0/8"},
		{"169.254.0.0/16"},
		{"100.64.0.0/10"},
		{"::1/128"},
		{"fc00::/7"},
		{"fe80::/10"},
	}

	for _, r := range privateRanges {
		_, cidr, _ := net.ParseCIDR(r.network)
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// isPrivateIP 检查 IP 是否为内网/保留地址（用于 DNS 解析后的二次检查）
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 云元数据 169.254.169.254
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	// RFC 6598 (CGNAT)
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	if cgnat.Contains(ip) {
		return true
	}
	return false
}
