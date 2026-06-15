package gateway

// Coverage for the auth layer — the 0%-tested authz gate flagged by the audit.
// Exercises every branch of Check + validateToken: anonymous, platform identity,
// preconfigured token list, HMAC-signed tokens (valid/tampered/expired/future),
// and the deny-by-default path when nothing is configured.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func signedToken(secret, ts string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	return ts + "." + hex.EncodeToString(mac.Sum(nil))
}

func apiMsg(token string) *adapter.Message {
	return &adapter.Message{Platform: adapter.PlatformAPI, Metadata: map[string]string{"auth_token": token}}
}

func TestAuthLayer_Check(t *testing.T) {
	ctx := context.Background()

	// anonymous mode lets API messages through
	if err := NewAuthLayer(config.AuthConfig{AllowAnonymous: true}).Check(ctx, &adapter.Message{Platform: adapter.PlatformAPI}); err != nil {
		t.Errorf("anonymous API must pass: %v", err)
	}

	plat := NewAuthLayer(config.AuthConfig{})
	// platform message: identity established by the adapter
	if err := plat.Check(ctx, &adapter.Message{Platform: adapter.PlatformTelegram, UserID: "u1"}); err != nil {
		t.Errorf("platform msg with user id must pass: %v", err)
	}
	if err := plat.Check(ctx, &adapter.Message{Platform: adapter.PlatformTelegram}); err == nil {
		t.Error("platform msg without user id must be rejected")
	}
	// API message with no token
	if err := plat.Check(ctx, &adapter.Message{Platform: adapter.PlatformAPI}); err == nil {
		t.Error("API msg without token must be rejected")
	}

	// preconfigured token list
	list := NewAuthLayer(config.AuthConfig{Tokens: []string{"good"}})
	if err := list.Check(ctx, apiMsg("good")); err != nil {
		t.Errorf("listed token must pass: %v", err)
	}
	if err := list.Check(ctx, apiMsg("bad")); err == nil {
		t.Error("unlisted token must be rejected")
	}

	// HMAC-signed tokens
	const secret = "s3cr3t"
	hm := NewAuthLayer(config.AuthConfig{Secret: secret})
	now := time.Now().UTC().Format(time.RFC3339)
	if err := hm.Check(ctx, apiMsg(signedToken(secret, now))); err != nil {
		t.Errorf("valid HMAC token must pass: %v", err)
	}
	if err := hm.Check(ctx, apiMsg(now+".deadbeef")); err == nil {
		t.Error("tampered signature must be rejected")
	}
	expired := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	if err := hm.Check(ctx, apiMsg(signedToken(secret, expired))); err == nil {
		t.Error("expired token must be rejected")
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := hm.Check(ctx, apiMsg(signedToken(secret, future))); err == nil {
		t.Error("future-dated token must be rejected")
	}
	if err := hm.Check(ctx, apiMsg("no-dot-token")); err == nil {
		t.Error("malformed token (no separator) must be rejected")
	}

	// nothing configured → deny by default
	if err := NewAuthLayer(config.AuthConfig{}).Check(ctx, apiMsg("anything")); err == nil {
		t.Error("with no tokens and no secret, all API requests must be denied")
	}
}
