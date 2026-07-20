package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestKnowledgeEndpointsUseManagementAuthWithoutBreakingLoopbackDesktop(t *testing.T) {
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/knowledge/documents"},
		{http.MethodGet, "/api/v1/knowledge/documents/doc-1"},
		{http.MethodGet, "/api/v1/knowledge/corpora/default/embedding-policy"},
		{http.MethodGet, "/api/v1/knowledge/jobs/job-1"},
		{http.MethodDelete, "/api/v1/knowledge/documents/doc-1"},
	}

	for _, withToken := range []bool{false, true} {
		t.Run(map[bool]string{false: "server-token-unconfigured", true: "server-token-configured"}[withToken], func(t *testing.T) {
			cfg := config.DefaultConfig()
			if withToken {
				cfg.Server.APIToken = "knowledge-secret"
			}
			srv := NewServer(cfg, nil, nil, nil)
			reached := false
			guarded := srv.apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			for _, endpoint := range paths {
				reached = false
				req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
				req.RemoteAddr = "203.0.113.42:54321"
				rec := httptest.NewRecorder()
				guarded.ServeHTTP(rec, req)
				want := http.StatusForbidden
				if withToken {
					want = http.StatusUnauthorized
				}
				if rec.Code != want || reached {
					t.Fatalf("%s %s remote status=%d reached=%v want=%d", endpoint.method, endpoint.path, rec.Code, reached, want)
				}

				reached = false
				loopback := httptest.NewRequest(endpoint.method, endpoint.path, nil)
				loopback.RemoteAddr = "127.0.0.1:54321"
				loopbackRec := httptest.NewRecorder()
				guarded.ServeHTTP(loopbackRec, loopback)
				if loopbackRec.Code != http.StatusOK || !reached {
					t.Fatalf("%s %s loopback status=%d reached=%v", endpoint.method, endpoint.path, loopbackRec.Code, reached)
				}
			}

			if withToken {
				reached = false
				req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/documents", nil)
				req.RemoteAddr = "203.0.113.42:54321"
				req.Header.Set("Authorization", "Bearer knowledge-secret")
				rec := httptest.NewRecorder()
				guarded.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || !reached {
					t.Fatalf("authorized remote status=%d reached=%v", rec.Code, reached)
				}
			}
		})
	}
}
