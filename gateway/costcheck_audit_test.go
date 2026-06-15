package gateway

// Coverage for the cost-check (budget gate) and audit layers — both 0% per audit.
// costStore stubs the two cost queries the layer uses; everything else on the
// storage.Store interface is unused (embedded nil, never called).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
)

type costStore struct {
	storage.Store
	userCost, globalCost float64
	userErr, globalErr   error
}

func (s *costStore) GetUserCost(context.Context, string, time.Time) (float64, error) {
	return s.userCost, s.userErr
}
func (s *costStore) GetGlobalCost(context.Context, time.Time) (float64, error) {
	return s.globalCost, s.globalErr
}

func TestCostCheckLayer_Check(t *testing.T) {
	ctx := context.Background()
	msg := &adapter.Message{UserID: "u1"}
	cfg := config.CostConfig{BudgetPerUser: 10, BudgetGlobal: 100}

	// under budget → pass
	if err := NewCostCheckLayer(cfg, &costStore{userCost: 5, globalCost: 50}).Check(ctx, msg); err != nil {
		t.Errorf("under budget must pass: %v", err)
	}
	// user over budget → reject
	if err := NewCostCheckLayer(cfg, &costStore{userCost: 10, globalCost: 0}).Check(ctx, msg); err == nil {
		t.Error("user over budget must be rejected")
	}
	// global over budget → reject
	if err := NewCostCheckLayer(cfg, &costStore{userCost: 0, globalCost: 100}).Check(ctx, msg); err == nil {
		t.Error("global over budget must be rejected")
	}
	// user-cost query error → surfaced as gateway error
	if err := NewCostCheckLayer(cfg, &costStore{userErr: errors.New("db down")}).Check(ctx, msg); err == nil {
		t.Error("a cost-query error must surface")
	}
	// global query error
	if err := NewCostCheckLayer(cfg, &costStore{globalErr: errors.New("db down")}).Check(ctx, msg); err == nil {
		t.Error("a global cost-query error must surface")
	}
	// no budgets configured → always pass (no store calls)
	if err := NewCostCheckLayer(config.CostConfig{}, &costStore{}).Check(ctx, msg); err != nil {
		t.Errorf("no budget configured must pass: %v", err)
	}
}

func TestAuditLayer_Check(t *testing.T) {
	// Audit never rejects — it only records.
	err := NewAuditLayer().Check(context.Background(), &adapter.Message{Platform: adapter.PlatformAPI, UserID: "u", Content: "hi"})
	if err != nil {
		t.Errorf("audit layer must always pass, got %v", err)
	}
}
