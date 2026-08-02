package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

func TestKnowledgePrincipalP0_UsesAuthenticatedOwnerAndIgnoresUserQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/knowledge/documents?user_id=attacker", nil)
	req = req.WithContext(skill.WithAuthenticatedUser(context.Background(), "authenticated-owner"))
	if got := knowledgePrincipalID(req); got != "authenticated-owner" {
		t.Fatalf("knowledge principal=%q, want authenticated-owner", got)
	}
}
