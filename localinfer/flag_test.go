package localinfer

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestCoordinatorFeatureFlagDefaultsOnAndSupportsExplicitRollback(t *testing.T) {
	registered, ok := featureflag.Lookup(FlagCoordinatorV1)
	if !ok || !registered.Default || registered.Stage != featureflag.StageBeta {
		t.Fatalf("coordinator flag=%+v registered=%v", registered, ok)
	}
	defaults := featureflag.NewStatic(featureflag.Registered(), nil)
	if !defaults.IsEnabled(FlagCoordinatorV1) {
		t.Fatal("coordinator must default on")
	}
	rollback := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagCoordinatorV1: false})
	if rollback.IsEnabled(FlagCoordinatorV1) {
		t.Fatal("explicit false must provide restart-scoped rollback")
	}
}
