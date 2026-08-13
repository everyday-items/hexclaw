package render

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type redirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (f redirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// renderSSRFGuarded is deliberately package-private. Production callers cannot
// mark an arbitrary RoundTripper as safe; this in-memory test transport never
// opens a socket and is therefore safe to use for deterministic fixtures.
func (redirectRoundTripFunc) renderSSRFGuarded() {}

type unguardedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f unguardedRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type peerAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *peerAddrConn) RemoteAddr() net.Addr { return c.remote }

func redirectResponse(req *http.Request, location string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{location}},
		Body:       io.NopCloser(strings.NewReader("redirect")),
		Request:    req,
	}
}

func imageResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(strings.NewReader("png")),
		Request:    req,
	}
}

func TestFetchToDataURLValidatesEveryRedirectTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "loopback", target: "http://127.0.0.1/private.png"},
		{name: "cloud metadata", target: "http://169.254.169.254/latest/meta-data/iam/security-credentials/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{
				Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return redirectResponse(req, tt.target), nil
					}
					return imageResponse(req), nil
				}),
			}

			_, err := fetchToDataURL(context.Background(), "http://93.184.216.34/public.png", client, 1<<20)
			if err == nil || !strings.Contains(err.Error(), "SSRF") {
				t.Fatalf("redirect to %s must be rejected by SSRF validation, got %v", tt.target, err)
			}
			if calls != 1 {
				t.Fatalf("redirect target must be rejected before RoundTrip: calls=%d, want 1", calls)
			}
		})
	}
}

func TestFetchToDataURLComposesCallerRedirectPolicyWithoutMutation(t *testing.T) {
	callerErr := errors.New("caller redirect policy")
	callerCalls := 0
	callerPolicy := func(req *http.Request, via []*http.Request) error {
		callerCalls++
		if req.URL.String() != "http://93.184.216.35/next.png" {
			t.Fatalf("caller policy received URL %q", req.URL)
		}
		if len(via) != 1 {
			t.Fatalf("caller policy received %d prior requests, want 1", len(via))
		}
		return callerErr
	}
	client := &http.Client{
		Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return redirectResponse(req, "http://93.184.216.35/next.png"), nil
		}),
		CheckRedirect: callerPolicy,
	}
	before := reflect.ValueOf(client.CheckRedirect).Pointer()

	_, err := fetchToDataURL(context.Background(), "http://93.184.216.34/public.png", client, 1<<20)
	if err == nil || !strings.Contains(err.Error(), callerErr.Error()) {
		t.Fatalf("caller redirect error must be preserved, got %v", err)
	}
	if callerCalls != 1 {
		t.Fatalf("caller redirect policy calls=%d, want 1", callerCalls)
	}
	if after := reflect.ValueOf(client.CheckRedirect).Pointer(); after != before {
		t.Fatal("fetchToDataURL mutated the caller's shared HTTP client")
	}
}

func TestFetchToDataURLAppliesFiveHopLimitToCustomClient(t *testing.T) {
	transportCalls := 0
	callerCalls := 0
	client := &http.Client{
		Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls++
			if transportCalls > 8 {
				return imageResponse(req), nil
			}
			return redirectResponse(req, fmt.Sprintf("http://93.184.216.34/hop/%d", transportCalls)), nil
		}),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			callerCalls++
			return nil
		},
	}

	_, err := fetchToDataURL(context.Background(), "http://93.184.216.34/public.png", client, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "too many redirects (5)") {
		t.Fatalf("custom clients must retain the five-hop redirect limit, got %v", err)
	}
	if transportCalls != 5 {
		t.Fatalf("five-hop policy must stop before the sixth request: RoundTrip calls=%d, want 5", transportCalls)
	}
	if callerCalls != 4 {
		t.Fatalf("caller redirect policy should run for allowed hops: calls=%d, want 4", callerCalls)
	}
}

func TestFetchToDataURLRejectsPrivateActualPeerAfterPublicValidation(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	served := make(chan struct{})
	go func() {
		defer close(served)
		request, err := http.ReadRequest(bufio.NewReader(serverConn))
		if err != nil {
			return
		}
		_ = request.Body.Close()
		_, _ = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Type: image/png\r\nContent-Length: 3\r\nConnection: close\r\n\r\npng")
	}()

	var dialAddr string
	client := &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialAddr = addr
			// Simulate a DNS-rebinding/custom-dial race: URL validation saw a
			// public address, but the socket actually landed on loopback.
			return &peerAddrConn{
				Conn:   clientConn,
				remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80},
			}, nil
		},
	}}

	_, err := fetchToDataURL(context.Background(), "http://93.184.216.34/public.png", client, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("a private actual peer must be rejected after public URL validation, got %v", err)
	}
	if host, _, splitErr := net.SplitHostPort(dialAddr); splitErr != nil || host != "93.184.216.34" {
		t.Fatalf("dial must be pinned to the validated public IP, addr=%q err=%v", dialAddr, splitErr)
	}
	_ = serverConn.Close()
	<-served
}

func TestFetchToDataURLRejectsUnguardedCustomTransport(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: unguardedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return imageResponse(req), nil
	})}

	_, err := fetchToDataURL(context.Background(), "http://93.184.216.34/public.png", client, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("unguarded custom transport must fail closed, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("unguarded transport was invoked %d times", calls)
	}
}

func TestFetchToDataURLUsesCallerContextForInitialSSRFValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transportCalls := 0
	client := &http.Client{Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		transportCalls++
		return imageResponse(req), nil
	})}

	_, err := fetchToDataURL(ctx, "https://context-propagation.invalid/image.png", client, 1<<20)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		t.Fatalf("initial SSRF validation did not preserve the caller cancellation: %v", err)
	}
	if transportCalls != 0 {
		t.Fatalf("transport was called after caller cancellation: %d", transportCalls)
	}
}

func TestRedirectValidationUsesRedirectRequestContext(t *testing.T) {
	client, err := clientWithSafeRedirects(&http.Client{Transport: redirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return imageResponse(req), nil
	})})
	if err != nil {
		t.Fatalf("clientWithSafeRedirects() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://context-propagation.invalid/redirect.png", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	err = client.CheckRedirect(request, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("redirect SSRF validation error = %v, want context.Canceled", err)
	}
}
