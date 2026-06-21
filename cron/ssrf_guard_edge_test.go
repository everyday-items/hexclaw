package cron

// Edge-case coverage for the cron SSRF dialer guard (isBlockedIP) beyond BUG-F4:
// IPv4-mapped IPv6 forms of reserved ranges (a common bypass attempt), IPv6 ULA /
// loopback / link-local, and a property sweep across every host in the CGNAT block
// sampling to ensure the whole /10 is covered (not just the boundaries).

import (
	"net"
	"testing"
)

func TestSSRFGuard_IPv6MappedReservedBlocked(t *testing.T) {
	// IPv4-mapped IPv6 of reserved v4 ranges must also be blocked — the dialer
	// Control hook may see either form depending on resolver.
	mustBlock := []string{
		"::ffff:100.64.0.1",      // CGNAT, v6-mapped
		"::ffff:169.254.169.254", // cloud metadata, v6-mapped
		"::ffff:10.0.0.1",        // RFC1918, v6-mapped
		"::ffff:198.18.0.1",      // benchmarking, v6-mapped
		"fd00::1",                // IPv6 ULA (private)
		"fe80::1",                // IPv6 link-local
		"::1",                    // IPv6 loopback
	}
	for _, s := range mustBlock {
		addr := net.ParseIP(s)
		if addr == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !isBlockedIP(addr) {
			t.Errorf("SSRF guard must block %s", s)
		}
	}
}

// TestSSRFGuard_CGNATWholeBlockBlocked sweeps representative hosts across the
// whole 100.64.0.0/10 block (not just boundaries) so a future change that
// narrows the CIDR is caught.
func TestSSRFGuard_CGNATWholeBlockBlocked(t *testing.T) {
	for _, second := range []byte{64, 80, 96, 112, 127} { // 100.64–100.127 = the /10
		addr := net.IPv4(100, second, 0, 1)
		if !isBlockedIP(addr) {
			t.Errorf("CGNAT 100.%d.0.1 must be blocked", second)
		}
	}
	// Just outside the /10 on both sides must remain allowed (public).
	for _, s := range []string{"100.63.255.255", "100.128.0.0"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s is outside CGNAT /10 and must remain allowed", s)
		}
	}
}
