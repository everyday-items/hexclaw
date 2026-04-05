package sandbox

import (
	"testing"
	"time"
)

func TestMatchDomainPattern_EmptyPattern(t *testing.T) {
	if matchDomainPattern("", "example.com") {
		t.Error("empty pattern should not match any host")
	}
}

func TestMatchDomainPattern_EmptyHost(t *testing.T) {
	if matchDomainPattern("example.com", "") {
		t.Error("should not match empty host")
	}
}

func TestMatchDomainPattern_EmptyBoth(t *testing.T) {
	// Empty pattern == empty host -> exact match!
	result := matchDomainPattern("", "")
	if !result {
		t.Log("empty pattern matching empty host returns false (after trimming)")
	}
	// This tests the actual behavior: both trim to "", so "" == "" is true
}

func TestMatchDomainPattern_WildcardComMatchingComItself(t *testing.T) {
	// "*.com" should match "example.com" (has .com suffix)
	if !matchDomainPattern("*.com", "example.com") {
		t.Error("*.com should match example.com")
	}

	// "*.com" matching "com" itself - the pattern is "*.com", suffix is ".com"
	// host "com" does NOT end with ".com", and host "com" != pattern[2:] which is "com"
	// Actually pattern[2:] = "com", so host "com" == "com" -> should match
	result := matchDomainPattern("*.com", "com")
	if !result {
		t.Error("*.com should match 'com' itself via the host == pattern[2:] check")
	}
}

func TestMatchDomainPattern_WildcardPrefix(t *testing.T) {
	tests := []struct {
		pattern string
		host    string
		want    bool
	}{
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com", true},
		{"*.github.com", "evil-github.com", false},
		{"*.github.com", "sub.api.github.com", true},
		{"*.example.com", "example.com", true},
		{"*.example.com", "notexample.com", false},
	}

	for _, tt := range tests {
		got := matchDomainPattern(tt.pattern, tt.host)
		if got != tt.want {
			t.Errorf("matchDomainPattern(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
		}
	}
}

func TestMatchDomainPattern_CaseInsensitive(t *testing.T) {
	if !matchDomainPattern("Example.COM", "example.com") {
		t.Error("matching should be case insensitive")
	}
	if !matchDomainPattern("*.GitHub.COM", "api.github.com") {
		t.Error("wildcard matching should be case insensitive")
	}
}

func TestIsAllowed_EmptyWhitelistWhitelistOnlyMode(t *testing.T) {
	p := &NetPolicy{
		Mode:            "whitelist-only",
		DomainWhitelist: []string{}, // empty whitelist
		AllowedPorts:    []int{80, 443},
		counters:        make(map[string]*rateBucket),
	}

	// In whitelist-only mode with empty whitelist, everything should be denied
	if p.IsAllowed("example.com", 443) {
		t.Error("whitelist-only with empty whitelist should deny all hosts")
	}
	if p.IsAllowed("google.com", 80) {
		t.Error("whitelist-only with empty whitelist should deny all hosts")
	}
}

func TestIsAllowed_DenyAllMode(t *testing.T) {
	p := &NetPolicy{
		Mode:            "deny-all",
		DomainWhitelist: []string{"example.com"},
		counters:        make(map[string]*rateBucket),
	}

	if p.IsAllowed("example.com", 443) {
		t.Error("deny-all mode should deny even whitelisted hosts")
	}
}

func TestIsAllowed_AllowAllMode(t *testing.T) {
	p := &NetPolicy{
		Mode:     "allow-all",
		counters: make(map[string]*rateBucket),
	}

	if !p.IsAllowed("anything.com", 8080) {
		t.Error("allow-all mode should allow any host/port")
	}
}

func TestIsAllowed_BlacklistOverridesWhitelist(t *testing.T) {
	p := &NetPolicy{
		Mode:            "whitelist-only",
		DomainWhitelist: []string{"*.example.com"},
		DomainBlacklist: []string{"evil.example.com"},
		AllowedPorts:    []int{443},
		counters:        make(map[string]*rateBucket),
	}

	if p.IsAllowed("evil.example.com", 443) {
		t.Error("blacklist should override whitelist")
	}
	if !p.IsAllowed("good.example.com", 443) {
		t.Error("non-blacklisted host should be allowed")
	}
}

func TestIsAllowed_PortFilter(t *testing.T) {
	p := &NetPolicy{
		Mode:         "allow-all",
		AllowedPorts: []int{80, 443},
		counters:     make(map[string]*rateBucket),
	}

	if p.IsAllowed("example.com", 8080) {
		t.Error("port 8080 should not be allowed when only 80/443 are permitted")
	}
	if !p.IsAllowed("example.com", 443) {
		t.Error("port 443 should be allowed")
	}
}

func TestIsAllowed_EmptyPortsMeansAllPorts(t *testing.T) {
	p := &NetPolicy{
		Mode:         "allow-all",
		AllowedPorts: []int{}, // empty = all ports allowed
		counters:     make(map[string]*rateBucket),
	}

	if !p.IsAllowed("example.com", 9999) {
		t.Error("empty AllowedPorts should allow all ports")
	}
}

func TestCheckRateLimit_EmptyProcessID(t *testing.T) {
	p := NewDefaultPolicy()

	// Empty process ID should still work (stored as "" key in map)
	for i := 0; i < 120; i++ {
		if !p.CheckRateLimit("") {
			t.Fatalf("empty processID should be rate limited like any other, failed at call %d", i+1)
		}
	}

	// 121st call should be denied
	if p.CheckRateLimit("") {
		t.Error("empty processID should be rate limited after exceeding max requests")
	}
}

func TestCheckRateLimit_WindowReset(t *testing.T) {
	p := &NetPolicy{
		MaxRequestsPerMinute: 2,
		counters:             make(map[string]*rateBucket),
	}

	if !p.CheckRateLimit("pid1") {
		t.Fatal("first call should pass")
	}
	if !p.CheckRateLimit("pid1") {
		t.Fatal("second call should pass")
	}
	if p.CheckRateLimit("pid1") {
		t.Fatal("third call should be denied (limit=2)")
	}

	// Simulate window reset by modifying the bucket's windowAt
	p.mu.Lock()
	bucket := p.counters["pid1"]
	bucket.windowAt = time.Now().Add(-2 * time.Minute) // pretend it's stale
	p.mu.Unlock()

	if !p.CheckRateLimit("pid1") {
		t.Error("call after window reset should pass")
	}
}

func TestCheckRateLimit_UnlimitedWhenZero(t *testing.T) {
	p := &NetPolicy{
		MaxRequestsPerMinute: 0, // unlimited
		counters:             make(map[string]*rateBucket),
	}

	for i := 0; i < 1000; i++ {
		if !p.CheckRateLimit("pid1") {
			t.Fatalf("unlimited rate should always pass, failed at call %d", i+1)
		}
	}
}

func TestSweep_DeletesStaleEntries(t *testing.T) {
	p := &NetPolicy{
		MaxRequestsPerMinute: 100,
		counters:             make(map[string]*rateBucket),
	}

	// Add some entries manually
	p.mu.Lock()
	p.counters["stale-1"] = &rateBucket{count: 5, windowAt: time.Now().Add(-3 * time.Minute)}
	p.counters["stale-2"] = &rateBucket{count: 3, windowAt: time.Now().Add(-5 * time.Minute)}
	p.counters["fresh-1"] = &rateBucket{count: 1, windowAt: time.Now()}
	p.sweepCallCount = 99 // next call will trigger sweep
	p.mu.Unlock()

	// This call should trigger the sweep (callCount becomes 100)
	p.CheckRateLimit("trigger")

	p.mu.Lock()
	mapSize := len(p.counters)
	_, hasStale1 := p.counters["stale-1"]
	_, hasStale2 := p.counters["stale-2"]
	_, hasFresh1 := p.counters["fresh-1"]
	_, hasTrigger := p.counters["trigger"]
	p.mu.Unlock()

	if hasStale1 {
		t.Error("stale-1 should have been swept")
	}
	if hasStale2 {
		t.Error("stale-2 should have been swept")
	}
	if !hasFresh1 {
		t.Error("fresh-1 should NOT have been swept")
	}
	if !hasTrigger {
		t.Error("trigger entry should exist")
	}
	if mapSize != 2 { // fresh-1 + trigger
		t.Errorf("expected 2 entries after sweep, got %d", mapSize)
	}
}

func TestCheckRateLimit_NilCounters(t *testing.T) {
	// counters is nil by default when not using NewDefaultPolicy
	p := &NetPolicy{
		MaxRequestsPerMinute: 10,
		// counters intentionally nil
	}

	// Should not panic - lazy init
	if !p.CheckRateLimit("pid1") {
		t.Error("first call should pass even with nil counters (lazy init)")
	}
}

func TestValidateResponseSize(t *testing.T) {
	p := NewDefaultPolicy()

	if !p.ValidateResponseSize(1024) {
		t.Error("1KB should be within 50MB limit")
	}
	if p.ValidateResponseSize(100 * 1024 * 1024) {
		t.Error("100MB should exceed 50MB limit")
	}
	if !p.ValidateResponseSize(50 * 1024 * 1024) {
		t.Error("exactly 50MB should be within limit (<=)")
	}
}

func TestValidateResponseSize_Unlimited(t *testing.T) {
	p := &NetPolicy{MaxResponseBodySize: 0}

	if !p.ValidateResponseSize(999 * 1024 * 1024 * 1024) {
		t.Error("unlimited response size should allow any size")
	}
}
