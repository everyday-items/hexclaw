package apihttp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
)

type accumulationRouteContextDeriver struct {
	routedAgent string
}

func (d *accumulationRouteContextDeriver) DeriveAccumulationMetadata(
	ctx context.Context,
	_ string,
) (k12.AccumulationDerivedMetadata, error) {
	d.routedAgent = skill.RoutedAgentName(ctx)
	return k12.AccumulationDerivedMetadata{
		Subject: "英语", EntryType: "词汇积累",
		SubjectProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
		EntryTypeProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
	}, nil
}

func TestAddAccumulationCarriesTutorAgentToMetadataDeriver(t *testing.T) {
	deriver := &accumulationRouteContextDeriver{}
	h := newServerWithSolver(
		t, fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(deriver),
	)
	rec, _ := doCurrent(
		t,
		h,
		http.MethodPost,
		"/accumulation?agent=mingming",
		`{"content":"apple"}`,
		map[string]string{"Idempotency-Key": "accumulation-route-context"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("create accumulation status=%d body=%s", rec.Code, rec.Body.String())
	}
	if deriver.routedAgent != "mingming" {
		t.Fatalf("metadata deriver routed agent=%q, want mingming", deriver.routedAgent)
	}
}
