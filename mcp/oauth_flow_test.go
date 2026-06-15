package mcp

// Coverage for the OAuth token flow — the entirely-0% OAuth manager flagged by
// the audit. Covers expiry, on-disk save/load round-trip (incl. path-traversal
// hardening), the token endpoint (exchange + refresh via httptest), GetToken's
// cached / expired / refresh branches, and the PKCE helpers.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOAuthToken_IsExpired(t *testing.T) {
	if !(&OAuthToken{ExpiresAt: time.Now().Add(time.Minute)}).IsExpired() {
		t.Error("a token inside the 5-minute early-refresh window must report expired")
	}
	if (&OAuthToken{ExpiresAt: time.Now().Add(time.Hour)}).IsExpired() {
		t.Error("a token valid for an hour must not report expired")
	}
}

func TestOAuthManager_SaveLoadRoundTrip(t *testing.T) {
	m := NewOAuthManager(t.TempDir())
	want := &OAuthToken{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}
	if err := m.saveToken("srv", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := m.loadToken("srv")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" || got.TokenType != "Bearer" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// path-traversal: a name with directory components is reduced to its base.
	if err := m.saveToken("../evil", want); err != nil {
		t.Fatalf("save traversal name: %v", err)
	}
	if _, err := m.loadToken("evil"); err != nil {
		t.Errorf("base-named credential must load: %v", err)
	}
}

func tokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestOAuthManager_TokenEndpoint(t *testing.T) {
	ok := tokenServer(t, 200, `{"access_token":"AT","refresh_token":"RT","token_type":"Bearer","expires_in":3600}`)
	defer ok.Close()
	m := NewOAuthManager(t.TempDir())

	tok, err := m.exchangeCode(context.Background(), &OAuthConfig{ClientID: "c", TokenURL: ok.URL}, "code", "verifier", "http://127.0.0.1/cb")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Errorf("parsed token wrong: %+v", tok)
	}
	if !tok.ExpiresAt.After(time.Now().Add(time.Minute)) {
		t.Error("expires_in must translate to a future ExpiresAt")
	}

	bad := tokenServer(t, 400, "invalid_grant")
	defer bad.Close()
	if _, err := m.refreshToken(context.Background(), &OAuthConfig{ClientID: "c", TokenURL: bad.URL}, "rt"); err == nil {
		t.Error("a non-200 token response must be an error")
	}
}

func TestOAuthManager_GetToken_Branches(t *testing.T) {
	ctx := context.Background()

	// valid on-disk token is returned without a network call
	m1 := NewOAuthManager(t.TempDir())
	if err := m1.saveToken("s1", &OAuthToken{AccessToken: "V", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := m1.GetToken(ctx, "s1", &OAuthConfig{}); err != nil || got.AccessToken != "V" {
		t.Fatalf("valid token must be returned: %v %+v", err, got)
	}

	// expired token with no refresh token → re-auth required
	m2 := NewOAuthManager(t.TempDir())
	if err := m2.saveToken("s2", &OAuthToken{AccessToken: "X", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.GetToken(ctx, "s2", &OAuthConfig{}); err == nil {
		t.Error("expired token without a refresh token must require re-authorization")
	}

	// expired token WITH a refresh token → refreshes via the token endpoint
	srv := tokenServer(t, 200, `{"access_token":"NEW","token_type":"Bearer","expires_in":3600}`)
	defer srv.Close()
	m3 := NewOAuthManager(t.TempDir())
	if err := m3.saveToken("s3", &OAuthToken{AccessToken: "OLD", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got3, err := m3.GetToken(ctx, "s3", &OAuthConfig{ClientID: "c", TokenURL: srv.URL})
	if err != nil || got3.AccessToken != "NEW" {
		t.Fatalf("refresh branch must return the new token: %v %+v", err, got3)
	}
}

func TestOAuthPKCEHelpers(t *testing.T) {
	v, err := generateCodeVerifier()
	if err != nil || len(v) < 16 {
		t.Fatalf("verifier: %v %q", err, v)
	}
	ch := codeChallenge(v)
	if ch == "" || ch == v {
		t.Error("code challenge must be a non-trivial hash of the verifier")
	}
	s, err := generateRandomString(16)
	if err != nil || len(s) != 16 {
		t.Errorf("random string length: %v %q", err, s)
	}
}
