package config

import (
	"net"
	"net/url"
	"strings"
)

const (
	ProviderLocalityAuto  = "auto"
	ProviderLocalityLocal = "local"
	ProviderLocalityCloud = "cloud"
)

// IsValidProviderLocality reports whether a configured locality is supported.
// Empty is the backwards-compatible spelling of auto.
func IsValidProviderLocality(locality string) bool {
	switch strings.ToLower(strings.TrimSpace(locality)) {
	case "", ProviderLocalityAuto, ProviderLocalityLocal, ProviderLocalityCloud:
		return true
	default:
		return false
	}
}

// IsLocalLLMProvider classifies the final model deployment location.
// Explicit locality is authoritative because a loopback endpoint can be a
// reverse proxy to a cloud model, while a private-LAN endpoint can host a real
// local model.
func IsLocalLLMProvider(provider LLMProviderConfig) bool {
	return IsLocalLLMProviderNamed("", provider)
}

// IsLocalLLMProviderNamed adds the provider map key as a last-resort legacy
// hint. Explicit locality remains authoritative; a configured endpoint is next;
// only an endpoint-less legacy provider named Ollama uses the name fallback.
func IsLocalLLMProviderNamed(name string, provider LLMProviderConfig) bool {
	switch strings.ToLower(strings.TrimSpace(provider.Locality)) {
	case ProviderLocalityLocal:
		return true
	case ProviderLocalityCloud:
		return false
	}
	if strings.TrimSpace(provider.BaseURL) != "" {
		return IsLocalProviderBaseURL(provider.BaseURL)
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "ollama")
}

// IsLocalProviderBaseURL classifies only the parsed endpoint host. Local-looking
// text in a public hostname, path, query, or userinfo must not bypass egress.
func IsLocalProviderBaseURL(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return host == "localhost" || host == "ollama" ||
		host == "host.docker.internal" || host == "host.containers.internal" ||
		strings.HasSuffix(host, ".local")
}
