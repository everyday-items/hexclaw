package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OAuthConfig holds OAuth 2.0 configuration for an MCP server.
type OAuthConfig struct {
	ClientID    string   `yaml:"client_id" json:"client_id"`
	AuthURL     string   `yaml:"auth_url" json:"auth_url"`
	TokenURL    string   `yaml:"token_url" json:"token_url"`
	Scopes      []string `yaml:"scopes" json:"scopes"`
	RedirectURI string   `yaml:"redirect_uri,omitempty" json:"redirect_uri,omitempty"`
}

// OAuthToken stored credentials.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// IsExpired checks if the token needs refresh.
func (t *OAuthToken) IsExpired() bool {
	return time.Now().After(t.ExpiresAt.Add(-5 * time.Minute)) // refresh 5 min early
}

// OAuthManager handles OAuth flows for MCP servers.
type OAuthManager struct {
	mu       sync.Mutex
	credDir  string // ~/.hexclaw/credentials/
	tokens   map[string]*OAuthToken
}

// NewOAuthManager creates an OAuth manager.
func NewOAuthManager(credDir string) *OAuthManager {
	os.MkdirAll(credDir, 0700)
	return &OAuthManager{
		credDir: credDir,
		tokens:  make(map[string]*OAuthToken),
	}
}

// GetToken returns a valid token for the server, refreshing if needed.
func (m *OAuthManager) GetToken(ctx context.Context, serverName string, cfg *OAuthConfig) (*OAuthToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Try cached
	if token, ok := m.tokens[serverName]; ok && !token.IsExpired() {
		return token, nil
	}

	// Try file
	token, err := m.loadToken(serverName)
	if err == nil && !token.IsExpired() {
		m.tokens[serverName] = token
		return token, nil
	}

	// Try refresh
	if token != nil && token.RefreshToken != "" {
		refreshed, err := m.refreshToken(ctx, cfg, token.RefreshToken)
		if err == nil {
			m.tokens[serverName] = refreshed
			m.saveToken(serverName, refreshed)
			return refreshed, nil
		}
	}

	// Need new authorization
	return nil, fmt.Errorf("OAuth token for %q expired or missing — re-authorization needed", serverName)
}

// Authorize runs the full OAuth 2.0 + PKCE flow.
// Starts a local HTTP server to receive the callback.
func (m *OAuthManager) Authorize(ctx context.Context, serverName string, cfg *OAuthConfig) (*OAuthToken, error) {
	// Generate PKCE code verifier + challenge
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	challenge := codeChallenge(verifier)

	// Start local callback server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state, _ := generateRandomString(16)
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("state mismatch")
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			errCh <- fmt.Errorf("OAuth error: %s", errMsg)
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorization code")
			http.Error(w, "No code", http.StatusBadRequest)
			return
		}
		codeCh <- code
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h2>Authorization successful!</h2><p>You can close this window.</p><script>window.close()</script></body></html>`)
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Close()

	// Build authorization URL
	authURL := fmt.Sprintf("%s?response_type=code&client_id=%s&redirect_uri=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		cfg.AuthURL,
		url.QueryEscape(cfg.ClientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(state),
		url.QueryEscape(challenge),
	)
	if len(cfg.Scopes) > 0 {
		authURL += "&scope=" + url.QueryEscape(joinScopes(cfg.Scopes))
	}

	// Return the auth URL — caller (frontend) should open it in browser
	// For now, we wait for the callback
	select {
	case code := <-codeCh:
		token, err := m.exchangeCode(ctx, cfg, code, verifier, redirectURI)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.tokens[serverName] = token
		m.saveToken(serverName, token)
		m.mu.Unlock()
		return token, nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("OAuth authorization timed out (5 minutes)")
	}
}

// GetAuthURL returns the authorization URL without blocking.
// The caller opens it in the browser; the callback is handled by Authorize().
func (m *OAuthManager) GetAuthURL(serverName string, cfg *OAuthConfig) string {
	return fmt.Sprintf("%s?response_type=code&client_id=%s&scope=%s",
		cfg.AuthURL, url.QueryEscape(cfg.ClientID), url.QueryEscape(joinScopes(cfg.Scopes)))
}

func (m *OAuthManager) exchangeCode(ctx context.Context, cfg *OAuthConfig, code, verifier, redirectURI string) (*OAuthToken, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {cfg.ClientID},
		"code_verifier": {verifier},
	}

	return m.tokenRequest(ctx, cfg.TokenURL, data)
}

func (m *OAuthManager) refreshToken(ctx context.Context, cfg *OAuthConfig, refreshToken string) (*OAuthToken, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {cfg.ClientID},
	}

	return m.tokenRequest(ctx, cfg.TokenURL, data)
}

func (m *OAuthManager) tokenRequest(ctx context.Context, tokenURL string, data url.Values) (*OAuthToken, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = data.Encode()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	return &OAuthToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}, nil
}

func (m *OAuthManager) loadToken(serverName string) (*OAuthToken, error) {
	path := filepath.Join(m.credDir, serverName+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var token OAuthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

func (m *OAuthManager) saveToken(serverName string, token *OAuthToken) error {
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.credDir, serverName+".json")
	return os.WriteFile(path, data, 0600)
}

// PKCE helpers

func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func codeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateRandomString(n int) (string, error) {
	// Generate enough bytes to ensure n characters after base64 encoding
	byteLen := (n*3 + 3) / 4 // base64 expands 3:4
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

func joinScopes(scopes []string) string {
	result := ""
	for i, s := range scopes {
		if i > 0 {
			result += " "
		}
		result += s
	}
	return result
}
