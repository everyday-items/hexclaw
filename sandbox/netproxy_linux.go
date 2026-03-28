//go:build linux

package sandbox

import (
	"fmt"
	"log"
	"os/exec"
)

// LinuxProxyIntegration sets up iptables rules to redirect sandboxed traffic through the proxy.
//
// Uses iptables OUTPUT chain with owner match to redirect traffic from the sandbox UID.
// Falls back to environment variable injection if iptables is not available.
type LinuxProxyIntegration struct {
	proxyAddr string
	uid       int // sandbox process UID
	applied   bool
}

// NewLinuxProxyIntegration creates a Linux proxy integration.
func NewLinuxProxyIntegration(proxyAddr string, uid int) *LinuxProxyIntegration {
	return &LinuxProxyIntegration{proxyAddr: proxyAddr, uid: uid}
}

// Apply sets up iptables rules for transparent proxying.
func (p *LinuxProxyIntegration) Apply() error {
	// Check if iptables is available
	if _, err := exec.LookPath("iptables"); err != nil {
		log.Printf("[netproxy-linux] iptables not available, falling back to env var proxy")
		return nil // graceful degradation
	}

	// Redirect HTTP (80) and HTTPS (443) from sandbox UID through proxy
	rules := [][]string{
		{"-t", "nat", "-A", "OUTPUT", "-m", "owner", "--uid-owner", fmt.Sprintf("%d", p.uid),
			"-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", extractPort(p.proxyAddr)},
		{"-t", "nat", "-A", "OUTPUT", "-m", "owner", "--uid-owner", fmt.Sprintf("%d", p.uid),
			"-p", "tcp", "--dport", "443", "-j", "REDIRECT", "--to-port", extractPort(p.proxyAddr)},
	}

	for _, rule := range rules {
		cmd := exec.Command("iptables", rule...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("iptables rule failed: %s: %w", string(out), err)
		}
	}

	p.applied = true
	return nil
}

// Cleanup removes the iptables rules.
func (p *LinuxProxyIntegration) Cleanup() {
	if !p.applied {
		return
	}
	// Replace -A with -D to delete rules
	rules := [][]string{
		{"-t", "nat", "-D", "OUTPUT", "-m", "owner", "--uid-owner", fmt.Sprintf("%d", p.uid),
			"-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", extractPort(p.proxyAddr)},
		{"-t", "nat", "-D", "OUTPUT", "-m", "owner", "--uid-owner", fmt.Sprintf("%d", p.uid),
			"-p", "tcp", "--dport", "443", "-j", "REDIRECT", "--to-port", extractPort(p.proxyAddr)},
	}
	for _, rule := range rules {
		exec.Command("iptables", rule...).Run()
	}
	p.applied = false
}

func extractPort(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return "8080"
}
