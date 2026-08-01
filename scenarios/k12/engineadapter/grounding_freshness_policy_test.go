package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type freshnessPolicyKB struct {
	policy knowledge.RetrievalFreshnessPolicy
}

func (k *freshnessPolicyKB) QueryWithFilter(ctx context.Context, _ string, _ int, _ knowledge.Filter) (string, error) {
	k.policy = knowledge.RetrievalFreshnessPolicyFromContext(ctx)
	return "教材证据", nil
}

func TestGroundingAdapter_UsesEvergreenFreshnessForTextbooks(t *testing.T) {
	kb := &freshnessPolicyKB{}
	if _, found, err := NewGroundingAdapter(kb).Ground(context.Background(), "tutor", "牛顿第一定律", "初二"); err != nil || !found {
		t.Fatalf("Ground found=%v err=%v", found, err)
	}
	if kb.policy != knowledge.RetrievalFreshnessEvergreen {
		t.Fatalf("教材检索应使用常青 freshness policy，得 %q", kb.policy)
	}
}
