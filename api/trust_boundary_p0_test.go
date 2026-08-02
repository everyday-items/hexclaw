package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestTrustBoundaryP0_DenyByDefaultAndExactPublicAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "api-capability"
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetSidecarCapabilityToken("desktop-capability")

	tests := []struct {
		name   string
		method string
		path   string
		remote string
		token  string
		want   int
	}{
		{name: "health is public", method: http.MethodGet, path: "/health", remote: "203.0.113.7:4321", want: http.StatusOK},
		{name: "version is public", method: http.MethodGet, path: "/api/v1/version", remote: "203.0.113.7:4321", want: http.StatusOK},
		{name: "webhook receiver is public", method: http.MethodPost, path: "/api/v1/webhooks/github", remote: "203.0.113.7:4321", want: http.StatusNotFound},
		{name: "webhook management is protected", method: http.MethodPost, path: "/api/v1/webhooks", remote: "203.0.113.7:4321", want: http.StatusUnauthorized},
		{name: "nested fake webhook path is protected", method: http.MethodPost, path: "/api/v1/webhooks/github/forged", remote: "203.0.113.7:4321", want: http.StatusUnauthorized},
		{name: "read api is protected", method: http.MethodGet, path: "/api/v1/sessions", remote: "203.0.113.7:4321", want: http.StatusUnauthorized},
		{name: "chat is protected", method: http.MethodPost, path: "/api/v1/chat", remote: "203.0.113.7:4321", want: http.StatusUnauthorized},
		{name: "websocket is protected", method: http.MethodGet, path: "/ws", remote: "203.0.113.7:4321", want: http.StatusUnauthorized},
		{name: "api capability authorizes remote api", method: http.MethodGet, path: "/api/v1/sessions", remote: "203.0.113.7:4321", token: "api-capability", want: http.StatusNotFound},
		{name: "sidecar capability authorizes loopback", method: http.MethodGet, path: "/api/v1/sessions", remote: "127.0.0.1:4321", token: "desktop-capability", want: http.StatusNotFound},
		{name: "configured sidecar rejects anonymous loopback", method: http.MethodGet, path: "/api/v1/sessions", remote: "127.0.0.1:4321", want: http.StatusUnauthorized},
		{name: "sidecar capability is never valid remotely", method: http.MethodGet, path: "/api/v1/sessions", remote: "203.0.113.7:4321", token: "desktop-capability", want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.RemoteAddr = tt.remote
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			srv.routes().ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("%s %s status=%d body=%s, want %d", tt.method, tt.path, w.Code, w.Body.String(), tt.want)
			}
		})
	}
}

func TestTrustBoundaryP0_ChatIdentityComesOnlyFromAuthenticatedPrincipal(t *testing.T) {
	tests := []struct {
		name         string
		remote       string
		apiToken     string
		sidecarToken string
		requestToken string
		wantPlatform adapter.Platform
		wantUserID   string
	}{
		{
			name: "remote api claims cannot forge desktop principal", remote: "203.0.113.8:9876",
			apiToken: "api-capability", sidecarToken: "desktop-capability", requestToken: "api-capability",
			wantPlatform: adapter.PlatformAPI, wantUserID: "api-user",
		},
		{
			name: "loopback sidecar gets server-derived desktop principal", remote: "127.0.0.1:9876",
			apiToken: "api-capability", sidecarToken: "desktop-capability", requestToken: "desktop-capability",
			wantPlatform: adapter.PlatformDesktop, wantUserID: defaultDesktopUserID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Server.APIToken = tt.apiToken
			eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
			srv := NewServer(cfg, eng, nil, nil)
			srv.SetSidecarCapabilityToken(tt.sidecarToken)

			body := `{"message":"hello","platform":"desktop","user_id":"victim"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
			req.RemoteAddr = tt.remote
			req.Header.Set("Authorization", "Bearer "+tt.requestToken)
			w := httptest.NewRecorder()
			srv.routes().ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
			}
			if eng.lastMsg == nil {
				t.Fatal("engine did not receive chat message")
			}
			if eng.lastMsg.Platform != tt.wantPlatform || eng.lastMsg.UserID != tt.wantUserID {
				t.Fatalf("message principal=(%s,%s), want (%s,%s)", eng.lastMsg.Platform, eng.lastMsg.UserID, tt.wantPlatform, tt.wantUserID)
			}
		})
	}
}

func TestTrustBoundaryP0_NonLoopbackWithoutConfiguredAPITokenFailsClosed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.APIToken = ""
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "should-not-run"}}, nil, nil)
	srv.SetSidecarCapabilityToken("desktop-only")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.RemoteAddr = "198.51.100.7:7654"
	req.Header.Set("Authorization", "Bearer desktop-only")
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback request without configured API token status=%d body=%s, want 401", w.Code, w.Body.String())
	}
}
