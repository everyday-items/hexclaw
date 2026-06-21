package engine

// BUG-F1 (review 2026-06-21): manage_skill — the SkillInstaller tool that
// search/install/removes arbitrary marketplace skills (= installing new code =
// capability injection) — was ABSENT from DefaultBaselinePolicy. The policy
// defaults to ActionAllow, and BeforeToolCall returns nil on ActionAllow BEFORE
// the unattended-dispatch deny logic runs, so manage_skill auto-executed from an
// externally-triggered webhook/spawn dispatch (and silently, without approval, in
// interactive sessions). manage_skill IS in cronRecursiveToolDenylist (stripped
// for cron) — proving it is recognized as an escalation vector — but webhook/spawn
// are not cron, and nothing else gated it.
//
// These tests enumerate ALL capability-mutation tools (granularity matches the
// bug: adding a new capability-mutation tool without a baseline rule must FAIL),
// and lock the unattended-deny behavior.

import (
	"context"
	"testing"
	"time"
)

// capabilityMutationTools are the tools that acquire/alter agent capability or
// schedule. Each MUST require approval in the baseline policy — none may default
// to allow. Enumerated (not sampled) so a future addition that forgets a rule
// trips this test.
var capabilityMutationTools = []string{
	"manage_skill",         // install/remove marketplace skills (BUG-F1)
	"create_skill",         // author a new skill
	"patch_skill",          // modify an existing skill
	"manage_skill_pending", // approve a skill draft
	"manage_mcp_server",    // register/manage MCP servers
}

func TestBUGF1_CapabilityMutationToolsGatedByBaselinePolicy(t *testing.T) {
	p := DefaultBaselinePolicy()
	for _, tool := range capabilityMutationTools {
		dec := p.Evaluate(&ToolCallInfo{Name: tool})
		if dec.Action != ActionRequireApproval {
			t.Errorf("BUG-F1: capability-mutation tool %q must require approval, got %q (matched rule %q)",
				tool, dec.Action, dec.MatchedRule)
		}
	}
}

func TestBUGF1_ManageSkillDeniedForUnattendedWebhookDispatch(t *testing.T) {
	// No reviewer + not in the read-collect auto-approve allowlist → an unattended
	// webhook dispatch must be DENIED (fail-closed). Before the fix this returned
	// nil (allowed) because the policy treated manage_skill as ActionAllow.
	hook := NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(DefaultBaselinePolicy()))
	ctx := withSystemDispatch(context.Background(), "webhook")
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{
		Name:      "manage_skill",
		Source:    "skill",
		Arguments: map[string]any{"action": "install", "keyword": "evil"},
	})
	if err == nil {
		t.Fatal("BUG-F1: manage_skill must NOT auto-run from a webhook dispatch (privilege escalation / capability injection)")
	}
}
