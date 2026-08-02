package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260802: a frozen assessing deadline must be able to extend only this
// request's guarded response-header budget. The short numbers deliberately
// model the production 120s/600s relation without making this regression slow.
func TestBUG20260802ProviderRequestDeadlineExtendsOnlyThisResponseHeaderGuard(t *testing.T) {
	const defaultHeaderBudget = 25 * time.Millisecond
	const delayedHeader = 80 * time.Millisecond
	const stageBudget = 300 * time.Millisecond

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delayedHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	client, err := NewProviderHTTPClient(
		provider.URL,
		config.ProviderPrivateNetworkAccess{},
		WithProviderResponseHeaderTimeout(defaultHeaderBudget),
	)
	if err != nil {
		t.Fatal(err)
	}
	guarded, ok := client.Transport.(*providerOriginTransport)
	if !ok || guarded.transport == nil {
		t.Fatalf("provider transport = %T", client.Transport)
	}

	stageCtx, cancel := context.WithTimeout(context.Background(), stageBudget)
	defer cancel()
	stageCtx = WithProviderRequestResponseHeaderTimeout(stageCtx, stageBudget)
	budget, ok := ProviderRequestResponseHeaderTimeoutFromContext(stageCtx)
	if !ok || budget <= delayedHeader {
		t.Fatalf("request-local response-header budget = %s ok=%t, want > %s", budget, ok, delayedHeader)
	}
	if deadline, hasDeadline := stageCtx.Deadline(); !hasDeadline || budget > time.Until(deadline)+20*time.Millisecond {
		t.Fatalf("request-local budget = %s outlives context deadline %v", budget, deadline)
	}

	req, err := http.NewRequestWithContext(stageCtx, http.MethodGet, provider.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request with frozen stage budget = %v, want delayed-header success", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := guarded.transport.ResponseHeaderTimeout; got != defaultHeaderBudget {
		t.Fatalf("shared response-header timeout mutated to %s, want %s", got, defaultHeaderBudget)
	}
}

func TestBUG20260802ProviderDefaultHeaderGuardRemainsForRequestsWithoutStageBudget(t *testing.T) {
	const defaultHeaderBudget = 25 * time.Millisecond
	const delayedHeader = 80 * time.Millisecond

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delayedHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	client, err := NewProviderHTTPClient(
		provider.URL,
		config.ProviderPrivateNetworkAccess{},
		WithProviderResponseHeaderTimeout(defaultHeaderBudget),
	)
	if err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, ok := ProviderRequestResponseHeaderTimeoutFromContext(reqCtx); ok {
		t.Fatal("ordinary request unexpectedly has a stage header budget")
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, provider.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("ordinary request unexpectedly crossed %s response-header guard", defaultHeaderBudget)
	}
}
