package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestProviderHTTPClientBlocksDNSRebindingAtActualDial(t *testing.T) {
	var reached atomic.Int64
	var dialCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	dialTarget := upstream.Listener.Addr().String()
	client, err := newProviderHTTPClient(
		"https://embedding.example.test/v1",
		config.ProviderPrivateNetworkAccess{},
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialCalls.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, dialTarget)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("https://embedding.example.test/v1/models")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrProviderEndpointPolicy) {
		t.Fatalf("rebound dial error = %v, want ErrProviderEndpointPolicy", err)
	}
	if reached.Load() != 0 {
		t.Fatalf("rebound request reached loopback upstream %d times", reached.Load())
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("blocked DNS candidate opened %d TCP connections, want pre-connect rejection", dialCalls.Load())
	}
}

func TestProviderHTTPClientPrevalidatesEveryDNSCandidateBeforeDial(t *testing.T) {
	var dialCalls atomic.Int64
	client, err := newProviderHTTPClient(
		"https://embedding.example.test/v1",
		config.ProviderPrivateNetworkAccess{},
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("8.8.8.8")},
				{IP: net.ParseIP("127.0.0.1")},
			}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("https://embedding.example.test/v1/models")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrProviderEndpointPolicy) || dialCalls.Load() != 0 {
		t.Fatalf("mixed DNS candidates err=%v dialCalls=%d, want pre-connect rejection", err, dialCalls.Load())
	}
}

func TestProviderHTTPClientDoesNotTreatPrivateHTTPAuthorizationAsPublicPlaintextOptIn(t *testing.T) {
	var dialCalls atomic.Int64
	client, err := newProviderHTTPClient(
		"http://embedding.lan.example/v1",
		config.ProviderPrivateNetworkAccess{Host: "embedding.lan.example", Allowed: true},
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://embedding.lan.example/v1/models")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrProviderEndpointPolicy) || dialCalls.Load() != 0 {
		t.Fatalf("public plaintext resolution err=%v dialCalls=%d, want pre-connect rejection", err, dialCalls.Load())
	}
}

func TestProviderHTTPClientLoopbackLogicalHostMustResolveOnlyToLoopback(t *testing.T) {
	var dialCalls atomic.Int64
	client, err := newProviderHTTPClient(
		"http://localhost:11434/v1",
		config.ProviderPrivateNetworkAccess{},
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://localhost:11434/v1/models")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrProviderEndpointPolicy) || dialCalls.Load() != 0 {
		t.Fatalf("localhost-to-public resolution err=%v dialCalls=%d, want pre-connect rejection", err, dialCalls.Load())
	}
}

func TestProviderHTTPClientPreservesExplicitLoopbackEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	client, err := NewProviderHTTPClient(upstream.URL+"/v1", config.ProviderPrivateNetworkAccess{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	guarded, ok := client.Transport.(*providerOriginTransport)
	if !ok || guarded.transport == nil || guarded.transport.Proxy != nil {
		t.Fatalf("provider transport must disable environment proxy: %T", client.Transport)
	}
}

func TestProviderHTTPClientFixedOriginAdapterKeepsDestinationPolicy(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetCalls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, target.URL+"/steal", status)
			}))
			defer origin.Close()

			client, err := NewProviderHTTPClient(
				origin.URL, config.ProviderPrivateNetworkAccess{},
				WithProviderFixedOriginAdapterTransport(),
			)
			if err != nil {
				t.Fatal(err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("fixed-origin adapter transport = %T, want *http.Transport", client.Transport)
			}
			if transport.Proxy != nil {
				t.Fatal("fixed-origin adapter transport must keep environment proxies disabled")
			}
			req, err := http.NewRequest(http.MethodPost, origin.URL+"/api/chat", strings.NewReader("private document"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, ErrProviderEndpointPolicy) {
				t.Fatalf("redirect error = %v, want ErrProviderEndpointPolicy", err)
			}
			if targetCalls.Load() != 0 {
				t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
			}
		})
	}
}

func TestProviderHTTPClientFixedOriginAdapterRejectsDirectOriginOrSchemeDrift(t *testing.T) {
	var originCalls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	var otherCalls atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()

	client, err := NewProviderHTTPClient(
		origin.URL, config.ProviderPrivateNetworkAccess{},
		WithProviderFixedOriginAdapterTransport(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"origin": other.URL + "/steal",
		"scheme": "https://" + strings.TrimPrefix(origin.URL, "http://") + "/steal",
	} {
		t.Run(name, func(t *testing.T) {
			resp, requestErr := client.Get(target)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(requestErr, ErrProviderEndpointPolicy) {
				t.Fatalf("direct %s drift error = %v, want ErrProviderEndpointPolicy", name, requestErr)
			}
		})
	}
	if originCalls.Load() != 0 || otherCalls.Load() != 0 {
		t.Fatalf("rejected direct requests reached origin=%d other=%d", originCalls.Load(), otherCalls.Load())
	}
}

func TestProviderHTTPClientFixedOriginAdapterSupportsVerifiedHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewProviderHTTPClient(
		server.URL, config.ProviderPrivateNetworkAccess{},
		WithProviderFixedOriginAdapterTransport(),
	)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("fixed-origin adapter transport = %T, want *http.Transport", client.Transport)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}

	resp, err := client.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestProviderHTTPClientAddsOnlyImplicitRequestScopedIdempotencyKey(t *testing.T) {
	received := make(chan string, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	client, err := NewProviderHTTPClient(provider.URL+"/v1", config.ProviderPrivateNetworkAccess{})
	if err != nil {
		t.Fatal(err)
	}
	requestWithKey := func(key string) *http.Request {
		t.Helper()
		ctx := WithProviderClientRequestKey(context.Background(), key)
		req, requestErr := http.NewRequestWithContext(
			ctx, http.MethodPost, provider.URL+"/v1/embeddings", nil,
		)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return req
	}

	documentRequest := requestWithKey("  durable-document-batch  ")
	resp, err := client.Do(documentRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := documentRequest.Header.Get("Idempotency-Key"); got != "" {
		t.Fatalf("provider transport mutated caller request header to %q", got)
	}

	explicitRequest := requestWithKey("durable-must-not-overwrite")
	explicitRequest.Header.Set("Idempotency-Key", "caller-explicit")
	resp, err = client.Do(explicitRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	nonEmbeddingCtx := WithProviderClientRequestKey(context.Background(), "must-not-leak")
	nonEmbeddingRequest, err := http.NewRequestWithContext(
		nonEmbeddingCtx, http.MethodGet, provider.URL+"/v1/models", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(nonEmbeddingRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	queryRequest, err := http.NewRequest(
		http.MethodPost, provider.URL+"/v1/embeddings", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(queryRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got := <-received; got != "durable-document-batch" {
		t.Fatalf("document Idempotency-Key=%q, want trimmed durable key", got)
	}
	if got := <-received; got != "caller-explicit" {
		t.Fatalf("explicit Idempotency-Key overwritten with %q", got)
	}
	if got := <-received; got != "" {
		t.Fatalf("non-embedding request leaked Idempotency-Key=%q", got)
	}
	if got := <-received; got != "" {
		t.Fatalf("adjacent request without a durable key leaked Idempotency-Key=%q", got)
	}
}

func TestProviderHTTPClientKeepsRequestIdempotencyKeyAcrossSameOriginRedirect(t *testing.T) {
	received := make(chan string, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Idempotency-Key")
		if r.URL.Path == "/v1/embeddings" {
			http.Redirect(w, r, "/v1/embeddings/final", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	client, err := NewProviderHTTPClient(provider.URL+"/v1", config.ProviderPrivateNetworkAccess{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProviderClientRequestKey(context.Background(), "durable-redirect-batch")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL+"/v1/embeddings", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	first, second := <-received, <-received
	if first != "durable-redirect-batch" || second != first {
		t.Fatalf("redirect Idempotency-Key first=%q second=%q", first, second)
	}
}

func TestProviderHTTPClientSupportsLongLocalResponseHeaderBudget(t *testing.T) {
	client, err := NewProviderHTTPClient(
		"http://localhost:11434/v1",
		config.ProviderPrivateNetworkAccess{},
		WithProviderResponseHeaderTimeout(10*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	guarded, ok := client.Transport.(*providerOriginTransport)
	if !ok || guarded.transport == nil {
		t.Fatalf("provider transport = %T", client.Transport)
	}
	if got := guarded.transport.ResponseHeaderTimeout; got != 10*time.Minute {
		t.Fatalf("response header timeout = %s, want 10m", got)
	}
}

func TestProviderHTTPClientRejectsCredentialsInBaseURL(t *testing.T) {
	client, err := NewProviderHTTPClient(
		"https://user:secret@api.openai.com/v1",
		config.ProviderPrivateNetworkAccess{},
	)
	if client != nil || !errors.Is(err, ErrProviderEndpointPolicy) {
		t.Fatalf("userinfo base URL client=%v err=%v, want endpoint policy rejection", client, err)
	}
}

func TestProviderHTTPClientRejectsQueryAndFragmentInBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"https://api.openai.com/v1?tenant=private",
		"https://api.openai.com/v1?",
		"https://api.openai.com/v1#embeddings",
	} {
		client, err := NewProviderHTTPClient(baseURL, config.ProviderPrivateNetworkAccess{})
		if client != nil || !errors.Is(err, ErrProviderEndpointPolicy) {
			t.Fatalf("ambiguous base URL %q client=%v err=%v, want endpoint policy rejection", baseURL, client, err)
		}
	}
}

func TestProviderHTTPClientRedirectsStayOnExactOrigin(t *testing.T) {
	var redirectedReached atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedReached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, redirected.URL+"/secret", http.StatusFound)
	}))
	defer origin.Close()
	client, err := NewProviderHTTPClient(origin.URL+"/v1", config.ProviderPrivateNetworkAccess{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(origin.URL + "/v1/embeddings")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrProviderEndpointPolicy) {
		t.Fatalf("cross-origin redirect error = %v, want ErrProviderEndpointPolicy", err)
	}
	if redirectedReached.Load() != 0 {
		t.Fatalf("cross-origin redirect reached target %d times", redirectedReached.Load())
	}
}

func TestProviderDialPolicyRequiresExactPrivateAuthorizationAndAlwaysBlocksMetadata(t *testing.T) {
	privateAddr := &net.TCPAddr{IP: net.ParseIP("10.0.0.8"), Port: 443}
	if err := validateProviderDialTarget("private.example.test", privateAddr, config.ProviderPrivateNetworkAccess{}); !errors.Is(err, ErrProviderEndpointPolicy) {
		t.Fatalf("private address without authorization error = %v", err)
	}
	access := config.ProviderPrivateNetworkAccess{Host: "private.example.test", Allowed: true}
	if err := validateProviderDialTarget("private.example.test", privateAddr, access); err != nil {
		t.Fatalf("exact private authorization rejected: %v", err)
	}
	if err := validateProviderDialTarget("other.example.test", privateAddr, access); !errors.Is(err, ErrProviderEndpointPolicy) {
		t.Fatalf("mismatched private authorization error = %v", err)
	}
	for _, ip := range []string{"169.254.169.254", "169.254.170.2", "fe80::1", "fd00:ec2::254", "0.0.0.0"} {
		metadataAccess := config.ProviderPrivateNetworkAccess{Host: "private.example.test", Allowed: true}
		if err := validateProviderDialTarget("private.example.test", &net.TCPAddr{IP: net.ParseIP(ip), Port: 80}, metadataAccess); !errors.Is(err, ErrProviderEndpointPolicy) {
			t.Fatalf("metadata/link-local %s bypassed explicit authorization: %v", ip, err)
		}
	}
}

func TestProviderDialPolicyRejectsReservedAndBenchmarkTargets(t *testing.T) {
	for _, ip := range []string{
		"192.0.2.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"2001:db8::1",
		"fec0::1",
	} {
		access := config.ProviderPrivateNetworkAccess{Host: "embedding.example.test", Allowed: true}
		err := validateProviderDialTarget(
			"embedding.example.test",
			&net.TCPAddr{IP: net.ParseIP(ip), Port: 443},
			access,
		)
		if !errors.Is(err, ErrProviderEndpointPolicy) {
			t.Fatalf("reserved/benchmark target %s error = %v, want endpoint policy rejection", ip, err)
		}
	}
}
