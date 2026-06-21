package cron

// BUG-F4 (review 2026-06-21): the cron starlark http_get/http_post SSRF dialer
// guard (isBlockedIP) delegated entirely to toolkit/net/ip.IsPrivateOrReservedIP,
// which is built from Go stdlib predicates (IsPrivate/IsLoopback/...). Those do
// NOT cover RFC6598 CGNAT 100.64.0.0/10 (routinely used for internal carrier /
// cloud infrastructure) nor a few other IANA special-purpose ranges. An
// LLM-authored or payload-influenced cron script could therefore http_get an
// internal address in those ranges, violating the "only public addresses" invariant.
//
// This locks the extra reserved ranges as blocked while keeping real public IPs allowed.

import (
	"net"
	"testing"
)

func TestBUGF4_SSRFGuardBlocksReservedRanges(t *testing.T) {
	mustBlock := []string{
		"100.64.0.1",     // RFC6598 CGNAT (lower)
		"100.127.255.1",  // RFC6598 CGNAT (upper)
		"192.0.0.1",      // RFC6890 IETF protocol assignments
		"198.18.0.1",     // RFC2544 benchmarking
		"198.19.255.255", // RFC2544 benchmarking (upper)
	}
	for _, s := range mustBlock {
		addr := net.ParseIP(s)
		if addr == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !isBlockedIP(addr) {
			t.Errorf("BUG-F4: reserved/CGNAT address %s must be blocked by the SSRF guard", s)
		}
	}

	// Regression guard: genuine public IPs must stay reachable.
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("BUG-F4: public IP %s must remain allowed", s)
		}
	}

	// Sanity: the existing private/loopback/metadata coverage must be preserved.
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254"} {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("BUG-F4 regression: %s must remain blocked", s)
		}
	}
}
