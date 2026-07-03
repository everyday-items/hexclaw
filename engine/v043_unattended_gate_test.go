package engine

// 功能优先不等于全默认打开：无人值守系统派发没有交互审批人，命中
// require_approval 的工具先查 profile + 显式开关矩阵；矩阵允许才自动放行。

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

func TestUnattendedGate_MatrixAllowedIgnoresReviewer(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(sendPolicy()), WithUnattendedReviewer(fakeUnattended{low: false}))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "send_message", Source: "skill"}); err != nil {
		t.Fatalf("cron delivery is in function_first matrix and must be allowed: %v", err)
	}
}

func TestUnattendedGate_MatrixDeniedDoesNotUseReviewerOverride(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(DefaultBaselinePolicy()), WithUnattendedReviewer(fakeUnattended{low: true}))
	ctx := withSystemDispatch(context.Background(), "webhook")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "manage_skill", Source: "skill"}); err == nil {
		t.Fatal("webhook capability mutation must require an explicit autonomy switch")
	}
}

func TestUnattendedGate_NoReviewerAllowedByMatrix(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(sendPolicy()))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "send_message", Source: "skill"}); err != nil {
		t.Fatalf("matrix-allowed unattended send must not require reviewer: %v", err)
	}
}

func TestUnattendedGate_FunctionFirstToolsAutoApprove(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(NewPermissionPolicy(ActionAllow,
			PolicyRule{Name: "all-approve", ToolPattern: "*", Action: ActionRequireApproval, Risk: "sensitive"})),
	)
	for _, tool := range []string{"web_search", "code_exec", "shell", "file_edit", "send_message", "media_generate", "app_heal"} {
		ctx := withSystemDispatch(context.Background(), "workflow")
		if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool, Source: "skill"}); err != nil {
			t.Fatalf("function_first workflow dispatch should auto-approve %s: %v", tool, err)
		}
	}
}

func TestUnattendedGate_FullAccessProfileExplicitlyAllowsCapability(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(DefaultBaselinePolicy()),
		WithSystemDispatchPolicy(FullAccessSystemDispatchPolicy()))
	ctx := withSystemDispatch(context.Background(), "webhook")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "manage_skill", Source: "skill"}); err != nil {
		t.Fatalf("full_access profile should explicitly allow manage_skill: %v", err)
	}
}
