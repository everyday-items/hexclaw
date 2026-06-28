package recall

import (
	"testing"
	"time"
)

func TestLifecycle_Evaluate(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	p := DefaultLifecyclePolicy()
	old := now.AddDate(0, 0, -200)
	yesterday := now.AddDate(0, 0, -1)

	cases := []struct {
		name string
		e    Entry
		want Transition
	}{
		{"高频检索条→晋升", Entry{Type: TypeFact, Content: "x", RecallCount: 3, AccessedAt: now}, Promote},
		{"陈旧低值检索条→归档", Entry{Type: TypeFact, Content: "x", AccessedAt: old}, Archive},
		{"pinned→不动", Entry{Type: TypeFact, Content: "x", Pinned: true, AccessedAt: old}, Stay},
		{"被取代→归档", Entry{Type: TypeFact, Content: "x", AccessedAt: now, ValidTo: &yesterday}, Archive},
		{"硬规则→不动", Entry{Type: TypeRule, Content: "x", AccessedAt: old}, Stay},
		{"冷偏好→降级", Entry{Type: TypePreference, Content: "x", RecallCount: 0, AccessedAt: old}, Demote},
		{"新检索条→不动", Entry{Type: TypeFact, Content: "x", AccessedAt: now}, Stay},
	}
	for _, c := range cases {
		if got := p.Evaluate(c.e, now); got != c.want {
			t.Errorf("%s：期望 %s，得 %s", c.name, c.want, got)
		}
	}
}
