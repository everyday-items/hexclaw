//go:build darwin

package sandbox

import (
	"log"
)

// DarwinProxyIntegration configures proxy for sandboxed processes on macOS.
//
// macOS doesn't support iptables. Instead:
//   - Environment variables (HTTP_PROXY/HTTPS_PROXY) are injected into the sandbox
//   - Seatbelt profile can restrict network to proxy address only
//   - System proxy settings can be configured via networksetup (not recommended for sandbox)
type DarwinProxyIntegration struct {
	proxyAddr string
}

// NewDarwinProxyIntegration creates a macOS proxy integration.
func NewDarwinProxyIntegration(proxyAddr string) *DarwinProxyIntegration {
	return &DarwinProxyIntegration{proxyAddr: proxyAddr}
}

// EnvVars returns environment variables to inject into the sandboxed process.
// This is the primary proxy mechanism on macOS.
func (p *DarwinProxyIntegration) EnvVars() []string {
	return ProxyEnvVars(p.proxyAddr)
}

// SeatbeltNetworkRule generates a Seatbelt SBPL rule that restricts
// network access to only the proxy address.
//
// Example SBPL:
//
//	(allow network-outbound (remote ip "127.0.0.1:8080"))
//	(deny network-outbound)
func (p *DarwinProxyIntegration) SeatbeltNetworkRule() string {
	return `(allow network-outbound (remote ip "` + p.proxyAddr + `"))
(deny network-outbound (remote ip "*"))`
}

// Apply configures the proxy (env vars only on macOS).
func (p *DarwinProxyIntegration) Apply() error {
	log.Printf("[netproxy-darwin] proxy configured via env vars: %s", p.proxyAddr)
	return nil
}

// Cleanup is a no-op on macOS (env vars are per-process).
func (p *DarwinProxyIntegration) Cleanup() {}
