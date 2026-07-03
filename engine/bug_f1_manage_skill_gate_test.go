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
// bug: adding a new capability-mutation tool without a baseline rule must FAIL).
// Under the function-first automation profile, unattended system dispatches
// still deny capability mutation unless an explicit autonomy switch enables it.

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

func TestBUGF1_ManageSkillDeniedForUnattendedWebhookDispatchByDefault(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(DefaultBaselinePolicy()))
	ctx := withSystemDispatch(context.Background(), "webhook")
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{
		Name:      "manage_skill",
		Source:    "skill",
		Arguments: map[string]any{"action": "install", "keyword": "evil"},
	})
	if err == nil {
		t.Fatal("function_first webhook dispatch must deny manage_skill unless explicitly enabled")
	}
}

func TestBUGF1_ManageSkillAllowedWhenFullAccessProfileExplicit(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(DefaultBaselinePolicy()),
		WithSystemDispatchPolicy(FullAccessSystemDispatchPolicy()))
	ctx := withSystemDispatch(context.Background(), "webhook")
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{
		Name:      "manage_skill",
		Source:    "skill",
		Arguments: map[string]any{"action": "install", "keyword": "trusted"},
	})
	if err != nil {
		t.Fatalf("full_access profile should allow manage_skill automation: %v", err)
	}
}
