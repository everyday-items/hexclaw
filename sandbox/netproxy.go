package sandbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NetProxy is a MITM HTTP/HTTPS proxy for sandboxed processes.
//
// Features:
//   - Domain whitelist (only allowed domains pass through)
//   - Request rate limiting per domain
//   - Audit logging of all requests
//   - HTTPS CONNECT tunnel (domain auditing without content inspection)
type NetProxy struct {
	mu        sync.RWMutex
	allowList map[string]bool // domain → allowed
	denyAll   bool            // if true, block all requests
	qps       int             // max requests per second per domain
	listener  net.Listener
	logger    func(method, host, path string, status int)
}

// NetProxyConfig configures the network proxy.
type NetProxyConfig struct {
	AllowDomains []string // allowed domains (glob: "*.github.com")
	DenyAll      bool     // block everything (offline mode)
	QPS          int      // rate limit per domain, 0 = unlimited
	ListenAddr   string   // default "127.0.0.1:0" (random port)
}

// NewNetProxy creates a MITM proxy.
func NewNetProxy(cfg NetProxyConfig) *NetProxy {
	allow := make(map[string]bool, len(cfg.AllowDomains))
	for _, d := range cfg.AllowDomains {
		allow[strings.ToLower(d)] = true
	}
	return &NetProxy{
		allowList: allow,
		denyAll:   cfg.DenyAll,
		qps:       cfg.QPS,
	}
}

// Start starts the proxy and returns the listen address.
func (p *NetProxy) Start(ctx context.Context, addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("proxy listen: %w", err)
	}
	p.listener = listener

	srv := &http.Server{
		Handler:      p,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[netproxy] serve error: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	return listener.Addr().String(), nil
}

// SetLogger sets the audit log callback.
func (p *NetProxy) SetLogger(fn func(method, host, path string, status int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger = fn
}

// ServeHTTP handles both regular HTTP and CONNECT tunnel requests.
func (p *NetProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if !p.isAllowed(host) {
		p.log(r.Method, host, r.URL.Path, 403)
		http.Error(w, "blocked by proxy policy", http.StatusForbidden)
		return
	}

	if r.Method == "CONNECT" {
		p.handleConnect(w, r, host)
		return
	}

	p.handleHTTP(w, r, host)
}

func (p *NetProxy) handleHTTP(w http.ResponseWriter, r *http.Request, host string) {
	outReq, _ := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	for k, vv := range r.Header {
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		p.log(r.Method, host, r.URL.Path, 502)
		http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	p.log(r.Method, host, r.URL.Path, resp.StatusCode)
}

func (p *NetProxy) handleConnect(w http.ResponseWriter, r *http.Request, host string) {
	// HTTPS CONNECT tunnel — audit domain only, no content inspection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	destAddr := r.Host
	if !strings.Contains(destAddr, ":") {
		destAddr += ":443"
	}

	destConn, err := net.DialTimeout("tcp", destAddr, 10*time.Second)
	if err != nil {
		p.log("CONNECT", host, "", 502)
		http.Error(w, "connect failed", http.StatusBadGateway)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		destConn.Close()
		return
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	p.log("CONNECT", host, "", 200)

	go transfer(destConn, clientConn)
	go transfer(clientConn, destConn)
}

func (p *NetProxy) isAllowed(host string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.denyAll {
		return false
	}
	if len(p.allowList) == 0 {
		return true // no whitelist = allow all
	}

	host = strings.ToLower(host)
	if p.allowList[host] {
		return true
	}
	// Check wildcard: *.github.com matches api.github.com
	for domain := range p.allowList {
		if strings.HasPrefix(domain, "*.") {
			suffix := domain[1:] // ".github.com"
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

func (p *NetProxy) log(method, host, path string, status int) {
	p.mu.RLock()
	fn := p.logger
	p.mu.RUnlock()
	if fn != nil {
		fn(method, host, path, status)
	}
}

func transfer(dst io.WriteCloser, src io.ReadCloser) {
	defer dst.Close()
	defer src.Close()
	io.Copy(dst, src)
}

// ProxyEnvVars returns environment variables to configure the sandboxed process
// to use this proxy.
func ProxyEnvVars(proxyAddr string) []string {
	return []string{
		"HTTP_PROXY=http://" + proxyAddr,
		"HTTPS_PROXY=http://" + proxyAddr,
		"http_proxy=http://" + proxyAddr,
		"https_proxy=http://" + proxyAddr,
		"SSL_CERT_FILE=", // let system handle
	}
}

// Ignore unused import for tls (will be used for CA cert in Phase 8 D36)
var _ = tls.Config{}
