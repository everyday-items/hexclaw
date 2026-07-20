package usecase_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRealEvalProviderErrorDoesNotLeakResponseBody(t *testing.T) {
	t.Parallel()
	const secret = "child-model-response-secret-44291"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(secret))
	}))
	defer server.Close()

	_, _, err := realEvalChat(context.Background(), server.URL, "local-model", "local-key", nil, nil)
	if err == nil {
		t.Fatal("provider error response must fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error leaked response body: %v", err)
	}
}
