package engine

// §11.10 统一安全闸：无人值守（系统派发、无交互会话）下，命中 require_approval 的
// consequential 动作改问 LLM 风险顾问，仅 low 放行，medium/high/无顾问一律 fail-closed。

import (
	"context"
	"testing"
	"time"
)

type fakeUnattended struct{ low bool }

func (f fakeUnattended) AssessLowRisk(context.Context, string, string) bool { return f.low }

func sendPolicy() *PermissionPolicy {
	return NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "send-approve", ToolPattern: "send_message", Action: ActionRequireApproval, Risk: "sensitive"},
	)
}

func TestUnattendedGate_LowAllowed(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(sendPolicy()), WithUnattendedReviewer(fakeUnattended{low: true}))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "send_message", Source: "skill"}); err != nil {
		t.Fatalf("low-risk unattended send must be allowed: %v", err)
	}
}

func TestUnattendedGate_HighDenied(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(sendPolicy()), WithUnattendedReviewer(fakeUnattended{low: false}))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "send_message", Source: "skill"}); err == nil {
		t.Fatal("non-low unattended send must be denied (fail-closed)")
	}
}

func TestUnattendedGate_NoReviewer_FailsClosed(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(sendPolicy()))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "send_message", Source: "skill"}); err == nil {
		t.Fatal("no reviewer must fail-closed (deny)")
	}
}

// 良性读取类工具（在 systemDispatchAutoApprove 白名单）无需顾问即放行。
func TestUnattendedGate_AllowlistedReadTool(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(NewPermissionPolicy(ActionAllow,
			PolicyRule{Name: "search-approve", ToolPattern: "web_search", Action: ActionRequireApproval, Risk: "sensitive"})),
	)
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "web_search", Source: "skill"}); err != nil {
		t.Fatalf("allowlisted read tool should auto-approve for system dispatch: %v", err)
	}
}
