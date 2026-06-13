package cron

// BUG-20260613 (audit M4): the per-run timeout was resolved independently in
// two layers. The executor treated a 0 spec.TimeoutSec as a 5-minute inner
// deadline, but the scheduler granted the run only a 60-second outer budget
// (0*s + 30s, floored to 60s). The outer ctx therefore SIGKILLed any
// default-timeout script at 60s and mislabeled it a timeout. Both layers now
// route through effectiveTimeoutSec / runBudget so the outer budget always
// outlives the inner deadline.

import (
	"testing"
	"time"
)

func TestBug20260613_EffectiveTimeoutResolvesDefault(t *testing.T) {
	if got := effectiveTimeoutSec(0); got != defaultScriptTimeoutSec {
		t.Errorf("0 must resolve to default %d, got %d", defaultScriptTimeoutSec, got)
	}
	if got := effectiveTimeoutSec(-5); got != defaultScriptTimeoutSec {
		t.Errorf("negative must resolve to default %d, got %d", defaultScriptTimeoutSec, got)
	}
	if got := effectiveTimeoutSec(120); got != 120 {
		t.Errorf("explicit timeout must be honored, got %d", got)
	}
}

// The cross-layer invariant: the scheduler's outer budget must always be at
// least the executor's inner deadline, for every timeout value — otherwise the
// outer ctx can kill a run before its own deadline fires (the M4 bug).
func TestBug20260613_OuterBudgetCoversInnerDeadline(t *testing.T) {
	for _, ts := range []int{0, -1, 1, 30, 60, 120, 300, 1800} {
		inner := time.Duration(effectiveTimeoutSec(ts)) * time.Second
		outer := runBudget(ts)
		if outer < inner {
			t.Errorf("runBudget(%d)=%s must be >= inner deadline %s — outer ctx would kill the run early",
				ts, outer, inner)
		}
		// And the headroom must be real slack, not a coincidence of the floor.
		if ts > 30 && outer < inner+runCleanupHeadroom {
			t.Errorf("runBudget(%d)=%s must leave cleanup headroom over inner %s", ts, outer, inner)
		}
	}
}

// The specific regression: a 0-timeout spec must get a budget that covers the
// 300s default, not the old 60s floor that truncated it.
func TestBug20260613_ZeroTimeoutGetsFullDefaultBudget(t *testing.T) {
	outer := runBudget(0)
	want := time.Duration(defaultScriptTimeoutSec)*time.Second + runCleanupHeadroom
	if outer != want {
		t.Errorf("zero-timeout budget must be %s (default+headroom), got %s", want, outer)
	}
	if outer <= 60*time.Second {
		t.Errorf("zero-timeout budget %s must exceed the old 60s floor that caused M4", outer)
	}
}
