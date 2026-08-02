package config

import (
	"strings"
	"testing"
)

func TestServerAuthP0_NonLoopbackListenerRequiresAPIToken(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.10", "hexclaw.internal"} {
		t.Run(host, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Server.Host = host
			cfg.Server.APIToken = ""
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("host %q without server.api_token must fail closed", host)
			}
			if !strings.Contains(err.Error(), "server.api_token") {
				t.Fatalf("validation error %q does not identify server.api_token", err)
			}
		})
	}
}

func TestServerAuthP0_LoopbackOrAuthenticatedListenerPasses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  string
		token string
	}{
		{name: "ipv4 loopback compatibility", host: "127.0.0.1"},
		{name: "localhost compatibility", host: "localhost"},
		{name: "ipv6 loopback compatibility", host: "::1"},
		{name: "remote with token", host: "0.0.0.0", token: "configured-capability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Server.Host = tc.host
			cfg.Server.APIToken = tc.token
			if err := cfg.Validate(); err != nil {
				t.Fatalf("valid authenticated listener rejected: %v", err)
			}
		})
	}
}
