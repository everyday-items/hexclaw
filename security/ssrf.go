// Package security 提供安全防护工具
//
// 当前包含:
//   - SSRF 防护: 阻止访问内网/私有 IP
//   - (D40) Prompt 注入防护: sanitize.go (Phase 9)
//   - (D21) Skill 安全扫描: skill_scanner.go (Phase 7)
package security

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// privateRanges 私有/保留 IP 段
var privateRanges = []net.IPNet{
	{IP: net.IP{10, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},        // 10.0.0.0/8
	{IP: net.IP{172, 16, 0, 0}, Mask: net.CIDRMask(12, 32)},     // 172.16.0.0/12
	{IP: net.IP{192, 168, 0, 0}, Mask: net.CIDRMask(16, 32)},    // 192.168.0.0/16
	{IP: net.IP{169, 254, 0, 0}, Mask: net.CIDRMask(16, 32)},    // 169.254.0.0/16 (link-local)
	{IP: net.IP{127, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},       // 127.0.0.0/8 (loopback)
	{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},       // IPv6 loopback
	{IP: net.ParseIP("fc00::"), Mask: net.CIDRMask(7, 128)},      // IPv6 unique local
	{IP: net.ParseIP("fe80::"), Mask: net.CIDRMask(10, 128)},     // IPv6 link-local
}

// blockedHosts 直接阻止的主机名
var blockedHosts = map[string]bool{
	"localhost":                true,
	"metadata.google.internal": true, // GCP metadata
	"169.254.169.254":         true, // AWS/Azure/GCP metadata endpoint
}

// ValidateURL 校验 URL 是否安全（非内网/私有 IP）
//
// 用于 FetchSkill、BrowserSkill 和 MCP HTTP 请求。
// 对标 OpenClaw 的 fetchWithSsrfGuard。
func ValidateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL missing host")
	}

	// 检查直接阻止的主机名
	if blockedHosts[strings.ToLower(host)] {
		return fmt.Errorf("SSRF blocked: host %q is not allowed", host)
	}

	// 解析 IP
	ips, err := net.LookupHost(host)
	if err != nil {
		// DNS 解析失败可能是正常域名暂时不可达，允许通过
		return nil
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		for _, cidr := range privateRanges {
			if cidr.Contains(ip) {
				return fmt.Errorf("SSRF blocked: %q resolves to private IP %s", host, ipStr)
			}
		}
	}

	return nil
}
