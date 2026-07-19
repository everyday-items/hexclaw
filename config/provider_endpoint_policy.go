package config

import (
	"fmt"
	"net"
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
// metadata, link-local and unspecified destinations are never allowed.
func ValidateProviderEndpointAccess(baseURL string, access ProviderPrivateNetworkAccess) error {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("invalid provider base_url")
	}
	host := normalizeProviderEndpointHost(u.Hostname())
	if host == "metadata.google.internal" {
		return fmt.Errorf("provider endpoint is a blocked metadata host")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("provider endpoint is a blocked link-local or unspecified host")
	}
	if ip.IsLoopback() {
		return nil
	}
	if !ip.IsPrivate() {
		return nil
	}
	if access.Allowed && normalizeProviderEndpointHost(access.Host) == host {
		return nil
	}
	return fmt.Errorf("provider endpoint on private network requires exact host authorization")
}
