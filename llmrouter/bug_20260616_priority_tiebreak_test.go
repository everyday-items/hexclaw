package llmrouter

// BUG-20260616: selectByPriority built its candidate slice by iterating the
// providers map (random order) and sorted only by priority with a non-stable
// sort.Slice. Providers sharing a priority (e.g. several relays all defaulting
// to 999) therefore resolved to a random winner across calls/restarts. A name
// tie-break makes the choice deterministic.

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
)

func TestBug20260616_SelectByPriorityTieBreak(t *testing.T) {
	r := &Selector{providers: map[string]hexagon.Provider{
		"relayA": nil,
		"relayB": nil,
		"relayC": nil,
	}}
	prio := map[string]int{} // none mapped -> all rank 999 -> tie

	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		seen[r.selectByPriority(prio)]++
	}
	if len(seen) != 1 {
		t.Fatalf("selectByPriority non-deterministic on equal priority: %v", seen)
	}
	if _, ok := seen["relayA"]; !ok {
		t.Fatalf("tie must resolve to smallest name 'relayA', got %v", seen)
	}
}
