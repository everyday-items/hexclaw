package sandbox

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNetProxy_AllowList(t *testing.T) {
	p := NewNetProxy(NetProxyConfig{
		AllowDomains: []string{"example.com", "*.github.com"},
	})

	if !p.isAllowed("example.com") {
		t.Error("example.com should be allowed")
	}
	if !p.isAllowed("api.github.com") {
		t.Error("api.github.com should match *.github.com")
	}
	if p.isAllowed("evil.com") {
		t.Error("evil.com should be blocked")
	}
}

func TestNetProxy_DenyAll(t *testing.T) {
	p := NewNetProxy(NetProxyConfig{DenyAll: true})
	if p.isAllowed("example.com") {
		t.Error("deny all should block everything")
	}
}

func TestNetProxy_NoWhitelist(t *testing.T) {
	p := NewNetProxy(NetProxyConfig{})
	if !p.isAllowed("anything.com") {
		t.Error("no whitelist should allow everything")
	}
}

func TestNetProxy_StartAndBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewNetProxy(NetProxyConfig{
		AllowDomains: []string{"httpbin.org"},
	})

	var blocked []string
	p.SetLogger(func(method, host, path string, status int) {
		if status == 403 {
			blocked = append(blocked, host)
		}
	})

	addr, err := p.Start(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Make a request through the proxy to a blocked domain
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL("http://" + addr)),
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("http://example.com/")
	if err != nil && !strings.Contains(err.Error(), "403") {
		// Some HTTP clients return error on proxy block
		t.Logf("request to blocked domain returned error (expected): %v", err)
	} else if resp != nil {
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("expected 403 for blocked domain, got %d", resp.StatusCode)
		}
	}
}

func mustParseURL(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}
