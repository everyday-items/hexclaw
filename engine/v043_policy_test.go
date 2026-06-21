package engine

import "testing"

// Consequential actions (external delivery, media generation, publishing) must
// be gated behind user approval by the baseline permission policy, while benign
// tools stay allowed.
func TestBaselinePolicy_ConsequentialActionsRequireApproval(t *testing.T) {
	p := DefaultBaselinePolicy()
	for _, tool := range []string{"send_message", "media_generate", "publish_wechat"} {
		dec := p.Evaluate(&ToolCallInfo{Name: tool})
		if dec.Action != ActionRequireApproval {
			t.Fatalf("%s must require approval, got %q (rule %q)", tool, dec.Action, dec.MatchedRule)
		}
	}
	if dec := p.Evaluate(&ToolCallInfo{Name: "summary"}); dec.Action != ActionAllow {
		t.Fatalf("benign tool must stay allowed, got %q", dec.Action)
	}
}
