package engine

// BUG-F5 (review 2026-06-21, found by scenario-matrix expansion): for an
// unattended (system-dispatch) tool call, requestApproval sent EVERY
// require_approval tool to the one-shot LLM risk reviewer and allowed it on a
// "low" verdict — including arbitrary-code-execution (shell/code/code_exec) and
// capability/host-mutation tools (create_skill/manage_skill/patch_skill/
// manage_skill_pending/manage_mcp_server/file_edit). That contradicts the
// documented intent in requestApproval ("never the capability-mutating ones ...
// or dangerous ones ...: those still require a human, which with no session means
// deny"). A single LLM "low" verdict could thus greenlight `manage_skill install`
// or `shell` from an externally-triggered webhook. BUG-F1's policy rule was
// necessary but insufficient: with the production reviewer present, the reviewer
// could still override it.
//
// Fix: an explicit unattended hard-deny set (exec + capability/host mutation) is
// rejected BEFORE the reviewer is consulted; the reviewer only gates lower-risk
// delivery actions (send_message / media_generate / publish_*).

import (
	"context"
	"testing"
	"time"
)

// TestBUGF5_ReviewerCannotOverrideHardDenyTools is the focused regression: even
// with a reviewer that returns low, exec + capability-mutation tools must be
// denied for an unattended dispatch.
func TestBUGF5_ReviewerCannotOverrideHardDenyTools(t *testing.T) {
	hardDeny := []string{
		"shell", "code", "code_exec", // arbitrary execution
		"create_skill", "manage_skill", "patch_skill", "manage_skill_pending", "manage_mcp_server", // capability mutation
		"file_edit", // host filesystem mutation
	}
	for _, src := range []string{"webhook", "spawn", "cron", "heartbeat"} {
		hook := NewPermissionHook(NewPermissionHub(time.Second),
			WithPolicy(DefaultBaselinePolicy()),
			WithUnattendedReviewer(fakeUnattended{low: true})) // reviewer says LOW
		ctx := withSystemDispatch(context.Background(), src)
		for _, tool := range hardDeny {
			err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool, Source: "skill"})
			if err == nil {
				t.Errorf("BUG-F5: exec/capability tool %q must hard-deny for unattended %s dispatch even on a low reviewer verdict", tool, src)
			}
		}
	}
}

// TestPermissionMatrix_UnattendedDispatch locks the FULL unattended authorization
// behavior across {tool} × {source} × {reviewer verdict}. Expected values are the
// post-fix contract; on the buggy code the hard-deny rows under reviewer-low flip
// to "allow" and fail.
func TestPermissionMatrix_UnattendedDispatch(t *testing.T) {
	const allow, deny = "allow", "deny"
	type want struct{ noReviewer, low, high string }
	cases := []struct {
		tool, source string
		w            want
	}{
		// read/collect allowlist → auto-approved for all sources, reviewer irrelevant
		{"web_search", "webhook", want{allow, allow, allow}},
		{"browser", "webhook", want{allow, allow, allow}},
		{"knowledge_ingest", "spawn", want{allow, allow, allow}},
		{"search", "heartbeat", want{allow, allow, allow}},
		// cron_task → only cron (cronOnly guard, pre-policy); others denied
		{"cron_task", "cron", want{allow, allow, allow}},
		{"cron_task", "webhook", want{deny, deny, deny}},
		{"cron_task", "spawn", want{deny, deny, deny}},
		// capability mutation → HARD DENY regardless of reviewer (BUG-F5)
		{"manage_skill", "webhook", want{deny, deny, deny}},
		{"create_skill", "webhook", want{deny, deny, deny}},
		{"manage_mcp_server", "spawn", want{deny, deny, deny}},
		{"patch_skill", "cron", want{deny, deny, deny}},
		{"file_edit", "webhook", want{deny, deny, deny}},
		// arbitrary execution → HARD DENY regardless of reviewer (BUG-F5)
		{"shell", "cron", want{deny, deny, deny}},
		{"code_exec", "webhook", want{deny, deny, deny}},
		// delivery actions → reviewer-gated (low allows, else deny)
		{"send_message", "cron", want{deny, allow, deny}},
		{"media_generate", "webhook", want{deny, allow, deny}},
		{"publish_wechat", "webhook", want{deny, allow, deny}},
		// unmatched tool → default allow for everyone
		{"summarize", "webhook", want{allow, allow, allow}},
	}
	reviewers := []struct {
		name string
		r    UnattendedReviewer
	}{
		{"no-reviewer", nil},
		{"reviewer-low", fakeUnattended{low: true}},
		{"reviewer-high", fakeUnattended{low: false}},
	}
	pick := func(w want, name string) string {
		switch name {
		case "no-reviewer":
			return w.noReviewer
		case "reviewer-low":
			return w.low
		default:
			return w.high
		}
	}
	for _, rv := range reviewers {
		for _, c := range cases {
			opts := []PermissionHookOption{WithPolicy(DefaultBaselinePolicy())}
			if rv.r != nil {
				opts = append(opts, WithUnattendedReviewer(rv.r))
			}
			hook := NewPermissionHook(NewPermissionHub(time.Second), opts...)
			ctx := withSystemDispatch(context.Background(), c.source)
			err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: c.tool, Source: "skill"})
			got := allow
			if err != nil {
				got = deny
			}
			if exp := pick(c.w, rv.name); got != exp {
				t.Errorf("[%s] tool=%s source=%s: got %s, want %s (err=%v)",
					rv.name, c.tool, c.source, got, exp, err)
			}
		}
	}
}
