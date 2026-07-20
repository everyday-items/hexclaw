package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// ProviderPrivateNetworkAccess records an explicit, host-scoped authorization
// for a provider endpoint on RFC1918/ULA space.
type ProviderPrivateNetworkAccess struct {
	Host    string `yaml:"host" json:"host"`
	Allowed bool   `yaml:"allowed" json:"allowed"`
}

func normalizeProviderEndpointHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	return strings.Trim(host, "[]")
}

// ValidateProviderEndpointAccess is the backend security boundary for user
// supplied provider URLs. Loopback is supported for desktop proxies; private
// LAN addresses require an authorization bound to the exact normalized host;
// metadata and special-purpose destinations are never allowed.
func ValidateProviderEndpointAccess(baseURL string, access ProviderPrivateNetworkAccess) error {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("invalid provider endpoint base_url")
	}
	host := normalizeProviderEndpointHost(u.Hostname())
	if host == "metadata.google.internal" {
		return fmt.Errorf("provider endpoint is a blocked metadata host")
	}
	ip := net.ParseIP(host)
	if u.Scheme == "http" && !providerPlainHTTPAuthorized(host, ip, access) {
		return fmt.Errorf("provider endpoint requires https outside explicitly local networks")
	}
	if ip == nil {
		return nil
	}
	return ValidateProviderResolvedEndpointAccess(host, ip, access)
}

func providerPlainHTTPAuthorized(
	host string,
	ip net.IP,
	access ProviderPrivateNetworkAccess,
) bool {
	if host == "localhost" || ip != nil && ip.IsLoopback() {
		return true
	}
	if !access.Allowed || normalizeProviderEndpointHost(access.Host) != host {
		return false
	}
	// A literal public address cannot masquerade as an explicitly private
	// endpoint. Hostnames are resolved and revalidated immediately before dial;
	// exact host authorization is the user's opt-in for an HTTP-only LAN service.
	return ip == nil || ip.IsPrivate()
}

// ValidateProviderResolvedEndpointAccess applies the provider endpoint policy
// to an IP selected by DNS resolution before a TCP connection is opened.
// Loopback is allowed only for an explicitly loopback logical host. RFC1918
// and ULA destinations require authorization for that exact logical host.
func ValidateProviderResolvedEndpointAccess(
	logicalHost string,
	ip net.IP,
	access ProviderPrivateNetworkAccess,
) error {
	logicalHost = normalizeProviderEndpointHost(logicalHost)
	if ip == nil {
		return fmt.Errorf("provider endpoint resolved to an invalid address")
	}
	if providerEndpointIPAlwaysBlocked(ip) {
		return fmt.Errorf("provider endpoint resolved to a blocked special-purpose address")
	}
	logicalIP := net.ParseIP(logicalHost)
	logicalLoopback := logicalHost == "localhost" || logicalIP != nil && logicalIP.IsLoopback()
	if logicalLoopback {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("provider endpoint loopback host resolved outside loopback")
	}
	if ip.IsLoopback() {
		return fmt.Errorf("provider endpoint resolved to loopback from a non-loopback host")
	}
	if ip.IsPrivate() {
		if access.Allowed && normalizeProviderEndpointHost(access.Host) == logicalHost {
			return nil
		}
		return fmt.Errorf("provider endpoint on private network requires exact host authorization")
	}
	if !ip.IsGlobalUnicast() {
		return fmt.Errorf("provider endpoint resolved to a non-global-unicast address")
	}
	return nil
}

var providerEndpointBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func providerEndpointIPAlwaysBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return false
	}
	if ip.IsUnspecified() || ip.Equal(net.IPv4bcast) || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("169.254.170.2")) ||
		ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range providerEndpointBlockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
