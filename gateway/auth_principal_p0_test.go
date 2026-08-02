package gateway

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestAuthPrincipalP0_TrustedContextIsSingleSourceOfIdentity(t *testing.T) {
	layer := NewAuthLayer(config.AuthConfig{Enabled: true, Method: "token"})
	ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
	if err := layer.Check(ctx, &adapter.Message{Platform: adapter.PlatformAPI, UserID: "owner-1"}); err != nil {
		t.Fatalf("matching authenticated principal rejected: %v", err)
	}

	err := layer.Check(ctx, &adapter.Message{Platform: adapter.PlatformDesktop, UserID: "victim"})
	if err == nil {
		t.Fatal("message user_id mismatch bypassed authenticated principal")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok || gwErr.Code != "principal_mismatch" {
		t.Fatalf("mismatch error=%T %v, want principal_mismatch", err, err)
	}
}
