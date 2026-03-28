//go:build windows

package sandbox

import (
	"fmt"
	"log"
	"os/exec"
)

// WindowsProxyIntegration configures proxy for sandboxed processes on Windows.
//
// Uses WinHTTP proxy settings via netsh:
//   - netsh winhttp set proxy proxy-server="127.0.0.1:8080"
//   - Falls back to environment variable injection
type WindowsProxyIntegration struct {
	proxyAddr string
	applied   bool
}

// NewWindowsProxyIntegration creates a Windows proxy integration.
func NewWindowsProxyIntegration(proxyAddr string) *WindowsProxyIntegration {
	return &WindowsProxyIntegration{proxyAddr: proxyAddr}
}

// EnvVars returns environment variables for the sandboxed process.
func (p *WindowsProxyIntegration) EnvVars() []string {
	return ProxyEnvVars(p.proxyAddr)
}

// Apply sets the system-level WinHTTP proxy for the sandboxed process.
func (p *WindowsProxyIntegration) Apply() error {
	// Use netsh to set proxy (affects current user context)
	cmd := exec.Command("netsh", "winhttp", "set", "proxy", fmt.Sprintf("proxy-server=%s", p.proxyAddr))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[netproxy-windows] netsh proxy failed: %s, falling back to env vars", string(out))
		return nil // graceful degradation
	}
	p.applied = true
	return nil
}

// Cleanup resets the WinHTTP proxy setting.
func (p *WindowsProxyIntegration) Cleanup() {
	if !p.applied {
		return
	}
	exec.Command("netsh", "winhttp", "reset", "proxy").Run()
	p.applied = false
}
