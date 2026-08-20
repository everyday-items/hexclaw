package cron

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type bug20260820RoundTripFunc func(*http.Request) (*http.Response, error)

func (f bug20260820RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// RED: K12 回环端点需要 Sidecar capability，外部 URL 不得继承该凭据。
func TestBug20260820_StarlarkLoopbackCapabilityHeaderBoundary(t *testing.T) {
	var localAuth, externalAuth string
	eng := NewStarlarkEngine()
	eng.client = &http.Client{Transport: bug20260820RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "127.0.0.1":
			localAuth = r.Header.Get("Authorization")
		case "example.com":
			externalAuth = r.Header.Get("Authorization")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}
	eng.SetLoopbackCapabilityToken("capability-secret")

	script := "local = http_get(\"http://127.0.0.1:16060/api/k12/cron/mistake-sheet\")\n" +
		"external = http_get(\"https://example.com/public\")\n" +
		"emit({\"status\": \"success\"})"
	result, err := eng.Execute(context.Background(), &JobSpec{
		Runtime:    RuntimeStarlark,
		Script:     script,
		TimeoutSec: 10,
	})
	if err != nil {
		t.Fatalf("Execute infrastructure error: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("script should succeed, got status=%q error=%q", result.Status, result.Error)
	}
	if localAuth != "Bearer capability-secret" {
		t.Fatalf("loopback request must carry the sidecar capability, got %q", localAuth)
	}
	if externalAuth != "" {
		t.Fatalf("external request must not carry the sidecar capability, got %q", externalAuth)
	}
}
