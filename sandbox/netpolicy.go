package sandbox

import (
	"strings"
	"sync"
	"time"
)

// NetPolicy defines network access rules for sandboxed processes.
type NetPolicy struct {
	// DomainWhitelist contains allowed domain patterns (e.g. "*.github.com").
	DomainWhitelist []string
	// DomainBlacklist contains blocked domain patterns.
	DomainBlacklist []string
	// MaxRequestsPerMinute is the per-process rate limit (0 = unlimited).
	MaxRequestsPerMinute int
	// MaxResponseBodySize limits response body bytes (0 = unlimited).
	MaxResponseBodySize int64
	// AllowedPorts restricts outbound connections to these ports (empty = all).
	AllowedPorts []int
	// Mode controls the overall policy: "allow-all", "whitelist-only", "deny-all".
	Mode string

	mu             sync.Mutex
	counters       map[string]*rateBucket
	sweepCallCount int
}

type rateBucket struct {
	count    int
	windowAt time.Time
}

// NewDefaultPolicy returns a policy with sensible defaults:
// whitelist-only mode, ports 80/443, 120 req/min, 50 MB max body.
func NewDefaultPolicy() *NetPolicy {
	return &NetPolicy{
		Mode:                 "whitelist-only",
		AllowedPorts:         []int{80, 443},
		MaxRequestsPerMinute: 120,
		MaxResponseBodySize:  50 * 1024 * 1024, // 50 MB
		counters:             make(map[string]*rateBucket),
	}
}

// IsAllowed checks whether a request to host:port is permitted by the policy.
func (p *NetPolicy) IsAllowed(host string, port int) bool {
	if p.Mode == "deny-all" {
		return false
	}

	// Check blacklist first (always blocks, regardless of mode).
	for _, pat := range p.DomainBlacklist {
		if matchDomainPattern(pat, host) {
			return false
		}
	}

	// Port filter.
	if len(p.AllowedPorts) > 0 {
		allowed := false
		for _, ap := range p.AllowedPorts {
			if ap == port {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}

	if p.Mode == "allow-all" {
		return true
	}

	// whitelist-only: must match at least one whitelist pattern.
	for _, pat := range p.DomainWhitelist {
		if matchDomainPattern(pat, host) {
			return true
		}
	}
	return false
}

// CheckRateLimit returns true if the process is within its rate limit.
// Uses a simple fixed-window counter that resets every minute.
func (p *NetPolicy) CheckRateLimit(processID string) bool {
	if p.MaxRequestsPerMinute <= 0 {
		return true
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.counters == nil {
		p.counters = make(map[string]*rateBucket)
	}

	now := time.Now()

	// Sweep stale entries every 100 calls to bound map growth.
	p.sweepCallCount++
	if p.sweepCallCount >= 100 {
		p.sweepCallCount = 0
		for id, b := range p.counters {
			if now.Sub(b.windowAt) >= 2*time.Minute {
				delete(p.counters, id)
			}
		}
	}
	bucket, ok := p.counters[processID]
	if !ok || now.Sub(bucket.windowAt) >= time.Minute {
		p.counters[processID] = &rateBucket{count: 1, windowAt: now}
		return true
	}

	bucket.count++
	return bucket.count <= p.MaxRequestsPerMinute
}

// ValidateResponseSize returns true if the response body size is acceptable.
func (p *NetPolicy) ValidateResponseSize(size int64) bool {
	if p.MaxResponseBodySize <= 0 {
		return true
	}
	return size <= p.MaxResponseBodySize
}

// matchDomainPattern matches a host against a pattern.
// Supports exact match and wildcard prefix "*.example.com" (matches example.com and sub.example.com).
func matchDomainPattern(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = strings.ToLower(strings.TrimSpace(host))

	if pattern == host {
		return true
	}

	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) || host == pattern[2:]
	}

	return false
}
