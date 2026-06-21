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
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/net/httpx"
	// randx 为 toolkit 的加密安全随机串生成包；本文件同时直接使用标准库
	// crypto/rand（PKCE verifier），故以别名 randx 区分，避免与标准库 rand 冲突。
	randx "github.com/hexagon-codes/toolkit/util/rand"
)

// oauthHTTPClient 复用 toolkit httpx：带 ResponseHeaderTimeout 防"连上但不发数据"挂死，
// 取代裸 http.DefaultClient（无任何超时）。OAuth token 交换是短请求，30s 首字节超时足够。
var oauthHTTPClient = httpx.RawClient(httpx.WithResponseHeaderTimeout(30 * time.Second))

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
	mu      sync.Mutex
	credDir string // ~/.hexclaw/credentials/
	tokens  map[string]*OAuthToken
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

	state, err := generateRandomString(16)
	if err != nil {
		return nil, fmt.Errorf("generate OAuth state: %w", err)
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if !oauthStateValid(state, r.URL.Query().Get("state")) {
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
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read token response body: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request returned %d: %s", resp.StatusCode, truncateForErr(string(body), 256))
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
	safeName := filepath.Base(serverName) // strips directory components to prevent path traversal
	path := filepath.Join(m.credDir, safeName+".json")
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
	safeName := filepath.Base(serverName) // strips directory components to prevent path traversal
	path := filepath.Join(m.credDir, safeName+".json")
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

// truncateForErr caps an upstream body included in an error message so a verbose
// OAuth-server response cannot flood logs or surface large internal payloads.
func truncateForErr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(truncated)"
}

// oauthStateValid reports whether the OAuth callback's state parameter matches
// the one we issued. An empty expected state never validates: if state
// generation failed and left it empty, a callback carrying no state must not be
// able to pass the CSRF guard via "" == "".
func oauthStateValid(expected, got string) bool {
	return expected != "" && got == expected
}

// generateRandomString 生成长度为 n 的加密安全随机字符串，用于 OAuth CSRF state
// 等不可信任场景下的一次性随机 nonce。
//
// 该 state 仅作为不透明随机值参与 oauthStateValid 的精确相等比较，对字符集无要求，
// 只要求加密安全且具备足够熵。因此底层实现下沉到 toolkit 的 rand.TryToken：
//   - 错误传播：熵源（crypto/rand）失败时返回 error 而非 panic，保留本调用点
//     "生成失败即中止授权流程"的语义（避免 state="" 时 "" == "" 击穿 CSRF 守卫）；
//   - 字符集：rand.TryToken 输出 [0-9A-Za-z] 的 AlphaNumeric 串（n 字符约 5.95n 比特
//     熵），对 CSRF nonce 而言安全裕度充足，与历史 base64url 串等价可换。
//
// PKCE code verifier 因 RFC 7636 对字符集与长度有强约束，仍保留本地 generateCodeVerifier。
func generateRandomString(n int) (string, error) {
	return randx.TryToken(n)
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
