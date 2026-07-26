package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

// ErrProviderEndpointPolicy identifies an embedding request blocked before any
// HTTP bytes reach an untrusted dial target or redirect origin.
var ErrProviderEndpointPolicy = errors.New("provider endpoint policy violation")

type providerDialContext func(context.Context, string, string) (net.Conn, error)
type providerLookupIPAddr func(context.Context, string) ([]net.IPAddr, error)

type providerHTTPClientConfig struct {
	responseHeaderTimeout time.Duration
	fixedOriginAdapter    bool
}

// ProviderHTTPClientOption adjusts bounded transport behavior without
// weakening destination, redirect, credential or proxy policy.
type ProviderHTTPClientOption func(*providerHTTPClientConfig)

// WithProviderResponseHeaderTimeout overrides the default time allowed for a
// provider to produce response headers. It is useful for slow local embedding
// runtimes; caller request contexts remain the authoritative overall budget.
func WithProviderResponseHeaderTimeout(timeout time.Duration) ProviderHTTPClientOption {
	return func(cfg *providerHTTPClientConfig) {
		if cfg != nil && timeout > 0 {
			cfg.responseHeaderTimeout = timeout
		}
	}
}

// WithProviderFixedOriginAdapterTransport supports upstream adapters that
// insist on cloning a concrete *http.Transport. The returned transport keeps
// Proxy disabled and moves the origin/scheme checks into its dial hooks; the
// adapter must build every initial request from the configured base URL.
// Redirects remain subject to the client's exact-origin CheckRedirect policy.
func WithProviderFixedOriginAdapterTransport() ProviderHTTPClientOption {
	return func(cfg *providerHTTPClientConfig) {
		if cfg != nil {
			cfg.fixedOriginAdapter = true
		}
	}
}

const defaultCloudEmbeddingBaseURL = "https://api.openai.com/v1"

// NewProviderHTTPClient creates a guarded OpenAI-compatible embedding
// transport. Environment proxies are deliberately disabled so validation
// applies to the connection that will actually carry provider credentials.
func NewProviderHTTPClient(
	baseURL string,
	access config.ProviderPrivateNetworkAccess,
	options ...ProviderHTTPClientOption,
) (*http.Client, error) {
	return newProviderHTTPClient(baseURL, access, nil, nil, options...)
}

func newProviderHTTPClient(
	baseURL string,
	access config.ProviderPrivateNetworkAccess,
	lookup providerLookupIPAddr,
	dial providerDialContext,
	options ...ProviderHTTPClientOption,
) (*http.Client, error) {
	clientConfig := providerHTTPClientConfig{responseHeaderTimeout: 120 * time.Second}
	for _, option := range options {
		if option != nil {
			option(&clientConfig)
		}
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultCloudEmbeddingBaseURL
	}
	if err := config.ValidateProviderEndpointAccess(baseURL, access); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderEndpointPolicy, err)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return nil, fmt.Errorf("%w: invalid base URL", ErrProviderEndpointPolicy)
	}
	origin, err := providerOrigin(parsed)
	if err != nil {
		return nil, err
	}
	baseHost := normalizeProviderHost(parsed.Hostname())
	baseScheme := strings.ToLower(parsed.Scheme)
	baseAddr := net.JoinHostPort(baseHost, providerPort(parsed))
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: clientConfig.responseHeaderTimeout,
	}
	validatedDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil || net.JoinHostPort(normalizeProviderHost(host), port) != baseAddr {
			return nil, fmt.Errorf("%w: dial origin %q differs from configured origin", ErrProviderEndpointPolicy, addr)
		}
		candidates, lookupErr := lookupProviderCandidates(ctx, baseHost, lookup)
		if lookupErr != nil {
			return nil, lookupErr
		}
		// Validate every DNS answer before opening any socket. Failing the whole
		// resolution set prevents a public+private answer mix from becoming a
		// candidate-order or retry bypass.
		for _, candidate := range candidates {
			if policyErr := validateProviderIPTarget(baseHost, candidate.IP, access); policyErr != nil {
				return nil, policyErr
			}
			if transportErr := validateProviderTransportSecurity(
				baseScheme, baseHost, candidate.IP, access,
			); transportErr != nil {
				return nil, transportErr
			}
		}
		var lastDialErr error
		for _, candidate := range candidates {
			if !providerIPMatchesNetwork(candidate.IP, network) {
				continue
			}
			chosenIP := candidate.IP
			chosenAddr := net.JoinHostPort(chosenIP.String(), port)
			conn, dialErr := dial(ctx, network, chosenAddr)
			if dialErr != nil {
				lastDialErr = dialErr
				continue
			}
			remoteIP := providerRemoteIP(conn.RemoteAddr())
			if remoteIP == nil || !remoteIP.Equal(chosenIP) {
				_ = conn.Close()
				return nil, fmt.Errorf("%w: connected address differs from validated DNS candidate", ErrProviderEndpointPolicy)
			}
			if policyErr := validateProviderIPTarget(baseHost, remoteIP, access); policyErr != nil {
				_ = conn.Close()
				return nil, policyErr
			}
			if transportErr := validateProviderTransportSecurity(
				baseScheme, baseHost, remoteIP, access,
			); transportErr != nil {
				_ = conn.Close()
				return nil, transportErr
			}
			return conn, nil
		}
		if lastDialErr != nil {
			return nil, lastDialErr
		}
		return nil, fmt.Errorf("%w: DNS returned no address for network %q", ErrProviderEndpointPolicy, network)
	}
	if clientConfig.fixedOriginAdapter {
		configureFixedOriginAdapterDialers(transport, validatedDial, baseScheme, baseHost)
	} else {
		transport.DialContext = validatedDial
	}
	var roundTripper http.RoundTripper = &providerOriginTransport{origin: origin, transport: transport}
	if clientConfig.fixedOriginAdapter {
		roundTripper = transport
	}
	client := &http.Client{Transport: roundTripper}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("%w: too many redirects", ErrProviderEndpointPolicy)
		}
		redirectOrigin, originErr := providerOrigin(req.URL)
		if originErr != nil || redirectOrigin != origin {
			return fmt.Errorf("%w: redirect must stay on configured origin", ErrProviderEndpointPolicy)
		}
		if len(via) > 0 && providerEmbeddingRequest(via[0]) &&
			!providerRequestHeaderPresent(req.Header, "Idempotency-Key") {
			if key, ok := ProviderClientRequestKeyFromContext(req.Context()); ok {
				req.Header.Set("Idempotency-Key", key)
			}
		}
		return nil
	}
	return client, nil
}

func configureFixedOriginAdapterDialers(
	transport *http.Transport,
	validatedDial providerDialContext,
	baseScheme, baseHost string,
) {
	rejectScheme := func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("%w: request scheme differs from configured origin", ErrProviderEndpointPolicy)
	}
	if baseScheme == "http" {
		transport.DialContext = validatedDial
		transport.DialTLSContext = rejectScheme
		return
	}

	transport.DialContext = rejectScheme
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		plainConn, err := validatedDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tlsConfig := &tls.Config{ServerName: baseHost}
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
			if tlsConfig.ServerName == "" {
				tlsConfig.ServerName = baseHost
			}
		}
		if transport.ForceAttemptHTTP2 && len(tlsConfig.NextProtos) == 0 {
			tlsConfig.NextProtos = []string{"h2", "http/1.1"}
		}
		tlsConn := tls.Client(plainConn, tlsConfig)
		handshakeCtx := ctx
		cancel := func() {}
		if transport.TLSHandshakeTimeout > 0 {
			handshakeCtx, cancel = context.WithTimeout(ctx, transport.TLSHandshakeTimeout)
		}
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			_ = plainConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
}

func validateProviderTransportSecurity(
	scheme, logicalHost string,
	ip net.IP,
	access config.ProviderPrivateNetworkAccess,
) error {
	if !strings.EqualFold(scheme, "http") {
		return nil
	}
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if ip != nil && ip.IsPrivate() && access.Allowed &&
		normalizeProviderHost(access.Host) == normalizeProviderHost(logicalHost) {
		return nil
	}
	return fmt.Errorf("%w: public provider endpoint requires https", ErrProviderEndpointPolicy)
}

type providerOriginTransport struct {
	origin    string
	transport *http.Transport
}

func (t *providerOriginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.transport == nil || req == nil || req.URL == nil {
		return nil, fmt.Errorf("%w: invalid request transport", ErrProviderEndpointPolicy)
	}
	origin, err := providerOrigin(req.URL)
	if err != nil || origin != t.origin {
		return nil, fmt.Errorf("%w: request must stay on configured origin", ErrProviderEndpointPolicy)
	}
	if key, ok := ProviderClientRequestKeyFromContext(req.Context()); ok &&
		providerEmbeddingRequest(req) &&
		!providerRequestHeaderPresent(req.Header, "Idempotency-Key") {
		clone := req.Clone(req.Context())
		clone.Header = req.Header.Clone()
		clone.Header.Set("Idempotency-Key", key)
		req = clone
	}
	return t.transport.RoundTrip(req)
}

func providerEmbeddingRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(req.URL.Path, "/"), "/embeddings")
}

func providerRequestHeaderPresent(header http.Header, name string) bool {
	for key := range header {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func providerOrigin(u *url.URL) (string, error) {
	if u == nil || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%w: URL has no host", ErrProviderEndpointPolicy)
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrProviderEndpointPolicy, u.Scheme)
	}
	return scheme + "://" + net.JoinHostPort(normalizeProviderHost(u.Hostname()), providerPort(u)), nil
}

func providerPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func normalizeProviderHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	return strings.Trim(host, "[]")
}

func validateProviderDialTarget(
	logicalHost string,
	remote net.Addr,
	access config.ProviderPrivateNetworkAccess,
) error {
	ip := providerRemoteIP(remote)
	return validateProviderIPTarget(logicalHost, ip, access)
}

func validateProviderIPTarget(
	logicalHost string,
	ip net.IP,
	access config.ProviderPrivateNetworkAccess,
) error {
	if err := config.ValidateProviderResolvedEndpointAccess(logicalHost, ip, access); err != nil {
		return &providerEndpointResolutionError{
			logicalHost: normalizeProviderHost(logicalHost),
			resolvedIP:  ip.String(),
			cause:       err,
		}
	}
	return nil
}

// providerEndpointResolutionError preserves non-secret DNS evidence for
// diagnostics while keeping the established user-facing Error() text stable.
type providerEndpointResolutionError struct {
	logicalHost string
	resolvedIP  string
	cause       error
}

func (e *providerEndpointResolutionError) Error() string {
	return fmt.Sprintf("%v: %v", ErrProviderEndpointPolicy, e.cause)
}

func (e *providerEndpointResolutionError) Unwrap() error {
	return ErrProviderEndpointPolicy
}

func (e *providerEndpointResolutionError) ProviderEndpointLogicalHost() string {
	return e.logicalHost
}

func (e *providerEndpointResolutionError) ProviderEndpointResolvedIP() string {
	return e.resolvedIP
}

func lookupProviderCandidates(
	ctx context.Context,
	host string,
	lookup providerLookupIPAddr,
) ([]net.IPAddr, error) {
	if literal := net.ParseIP(host); literal != nil {
		return []net.IPAddr{{IP: literal}}, nil
	}
	candidates, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: DNS returned no addresses", ErrProviderEndpointPolicy)
	}
	return candidates, nil
}

func providerIPMatchesNetwork(ip net.IP, network string) bool {
	switch network {
	case "tcp4":
		return ip.To4() != nil
	case "tcp6":
		return ip.To4() == nil
	default:
		return true
	}
}

func providerRemoteIP(remote net.Addr) net.IP {
	if remote == nil {
		return nil
	}
	if tcp, ok := remote.(*net.TCPAddr); ok {
		return tcp.IP
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		host = remote.String()
	}
	return net.ParseIP(normalizeProviderHost(host))
}
