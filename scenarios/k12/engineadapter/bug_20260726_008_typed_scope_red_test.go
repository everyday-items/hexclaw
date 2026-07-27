package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type bug20260726008IncompleteScopeKB struct {
	queryCalls int
}

func (k *bug20260726008IncompleteScopeKB) ActiveSemanticRevision(
	context.Context,
) (string, bool, error) {
	return "revision-active", true, nil
}

func (k *bug20260726008IncompleteScopeKB) QueryWithFilter(
	context.Context, string, int, knowledge.Filter,
) (string, error) {
	k.queryCalls++
	return "synthetic source must not be trusted", nil
}

func TestBUG20260726008_IncompleteTypedBindingScopeFailsClosedBeforeKnowledgeQuery(t *testing.T) {
	ctx := context.Background()
	kb := &bug20260726008IncompleteScopeKB{}
	adapter := NewGroundingAdapter(kb)
	frozen, err := adapter.FreezeGroundingSnapshot(ctx, usecase.GroundingSnapshot{
		AgentName: "mingming", LearnerID: "mingming", Subject: "数学",
		TextbookBindingID: "binding-math", Edition: "人教版", Volume: "下册",
	})
	if err != nil {
		if kb.queryCalls != 0 {
			t.Fatalf("BUG-20260726-008: freeze failure dispatched %d queries", kb.queryCalls)
		}
		return
	}
	text, found, _ := adapter.GroundSnapshot(ctx, frozen, "同分母分数加法", "五年级下")
	if kb.queryCalls != 0 || found || text != "" {
		t.Fatalf("BUG-20260726-008 RED: incomplete binding scope queried synthetic source: calls=%d found=%v text=%q", kb.queryCalls, found, text)
	}
}
