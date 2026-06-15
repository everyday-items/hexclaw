package cron

// Method 1 (property/invariant): the Go<->Starlark value conversion must
// round-trip JSON values, and the SSRF guard must reject every private/
// link-local range (not just the sampled three).

import (
	"encoding/json"
	"net"
	"reflect"
	"testing"
)

func TestBug20260615_StarlarkValueRoundTrip_Invariant(t *testing.T) {
	cases := []string{
		`{"a":1,"b":"x","c":[1,2,3],"d":{"e":true,"f":null}}`,
		`[{"word":"标题A","n":42},{"word":"标题B","n":0}]`,
		`{"nested":{"deep":[{"k":"v"},{"k":"w"}]},"empty":[]}`,
	}
	for _, js := range cases {
		var orig any
		if err := json.Unmarshal([]byte(js), &orig); err != nil {
			t.Fatalf("seed unmarshal: %v", err)
		}
		back := starlarkToGo(goToStarlark(orig))
		reJS, _ := json.Marshal(back)
		var reParsed any
		_ = json.Unmarshal(reJS, &reParsed)
		if !reflect.DeepEqual(orig, reParsed) {
			t.Errorf("round-trip mismatch:\n  in : %s\n  out: %s", js, reJS)
		}
	}
}

func TestBug20260615_SSRFGuard_Invariant(t *testing.T) {
	// Every loopback/private/link-local/unspecified IP must be blocked; only
	// public addresses are allowed. F-3 added loopback to the blocked set (KB
	// ingest moved to the in-process kb_ingest builtin).
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.1",
		"192.168.0.1", "169.254.169.254", "169.254.1.1", "0.0.0.0", "fd00::1", "fe80::1"}
	allowed := []string{"8.8.8.8", "1.1.1.1", "39.156.66.10"}
	for _, ip := range blocked {
		if !isBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s must be blocked", ip)
		}
	}
	for _, ip := range allowed {
		if isBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s must be allowed (public only)", ip)
		}
	}
}
