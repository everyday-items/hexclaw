package engine

// 功能优先回归：unattended system dispatches are user-configured automation
// paths, but they must not silently become "approve all". Explicit
// PermissionPolicy ActionDeny remains authoritative; ActionRequireApproval is
// auto-approved only when the autonomy matrix allows {source, tool}.

import (
	"context"
	"testing"
	"time"
)

func TestFunctionFirstSystemDispatchAutoApprovesCoreExecutionTools(t *testing.T) {
	// BUG-20260702：exec 拆宿主/沙箱后，外部/无人值守源不再自动批准宿主直执行
	// shell/code（exec_host），只自动批准沙箱执行 code_exec（exec_sandboxed）；
	// 内部编排源 workflow/spawn 仍自动批准宿主 shell/code。
	cases := []struct {
		source string
		tools  []string
	}{
		{"webhook", []string{"code_exec", "file_edit", "browser", "send_message", "media_generate"}},
		{"spawn", []string{"shell", "code", "code_exec", "file_edit"}},
		{"cron", []string{"code_exec", "file_edit", "browser", "send_message", "media_generate"}},
		{"heartbeat", []string{"code_exec", "file_edit", "browser", "send_message"}},
		{"workflow", []string{"shell", "code", "code_exec", "file_edit", "browser", "send_message", "media_generate", "app_heal"}},
	}
	for _, c := range cases {
		hook := NewPermissionHook(NewPermissionHub(time.Second),
			WithPolicy(DefaultBaselinePolicy()),
			WithUnattendedReviewer(fakeUnattended{low: false}))
		ctx := withSystemDispatch(context.Background(), c.source)
		for _, tool := range c.tools {
			err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool, Source: "skill"})
			if err != nil {
				t.Errorf("function_first dispatch should auto-approve tool %q from %s: %v", tool, c.source, err)
			}
		}
	}
}

// TestFunctionFirstExternalSourcesDenyHostExecByDefault 钉死不变量（永久“锁”）：
// 外部/无人值守源(webhook/cron/heartbeat)默认不自动批准宿主直执行 shell/code；
// 而它们对沙箱执行 code_exec 照旧自动批准；内部编排源 workflow/spawn 仍放行宿主 shell。
func TestFunctionFirstExternalSourcesDenyHostExecByDefault(t *testing.T) {
	// all-approve 策略：放行与否完全交给自动批准矩阵裁决。
	newHook := func() *PermissionHook {
		return NewPermissionHook(NewPermissionHub(time.Second),
			WithPolicy(NewPermissionPolicy(ActionAllow,
				PolicyRule{Name: "all-approve", ToolPattern: "*", Action: ActionRequireApproval, Risk: "sensitive"})),
			WithUnattendedReviewer(fakeUnattended{low: true}))
	}
	for _, src := range []string{"webhook", "cron", "heartbeat"} {
		ctx := withSystemDispatch(context.Background(), src)
		for _, host := range []string{"shell", "code"} {
			if err := newHook().BeforeToolCall(ctx, &ToolCallInfo{Name: host, Source: "skill"}); err == nil {
				t.Errorf("external source %s must NOT auto-approve host-exec tool %q (needs approval)", src, host)
			}
		}
		if err := newHook().BeforeToolCall(ctx, &ToolCallInfo{Name: "code_exec", Source: "skill"}); err != nil {
			t.Errorf("external source %s must still auto-approve sandboxed code_exec: %v", src, err)
		}
	}
	for _, src := range []string{"workflow", "spawn"} {
		ctx := withSystemDispatch(context.Background(), src)
		if err := newHook().BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"}); err != nil {
			t.Errorf("internal orchestration source %s must still auto-approve host-exec shell: %v", src, err)
		}
	}
}

func TestFunctionFirstSystemDispatchDeniesCapabilityMutationByDefault(t *testing.T) {
	for _, src := range []string{"webhook", "spawn", "cron", "heartbeat", "workflow"} {
		hook := NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(DefaultBaselinePolicy()))
		ctx := withSystemDispatch(context.Background(), src)
		for _, tool := range []string{"create_skill", "manage_skill", "patch_skill", "manage_skill_pending", "manage_mcp_server"} {
			err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool, Source: "skill"})
			if err == nil {
				t.Errorf("function_first dispatch must deny capability tool %q from %s without explicit switch", tool, src)
			}
		}
	}
}

func TestFunctionalSystemDispatchExplicitPolicyDenyStillBlocks(t *testing.T) {
	hook := NewPermissionHook(NewPermissionHub(time.Second),
		WithPolicy(NewPermissionPolicy(ActionAllow,
			PolicyRule{Name: "deny-shell", ToolPattern: "shell", Action: ActionDeny, Reason: "operator override"})))
	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Fatal("explicit ActionDeny must still block system dispatch")
	}
}

// TestPermissionMatrix_UnattendedDispatch locks the function-first matrix
// behavior across {tool} x {source} x {reviewer verdict}. It deliberately uses
// an all-approval policy so every result is produced by the autonomy matrix,
// not by DefaultBaselinePolicy's ActionAllow fallback.
func TestPermissionMatrix_UnattendedDispatch(t *testing.T) {
	const allow, deny = "allow", "deny"
	type want struct{ noReviewer, low, high string }
	cases := []struct {
		tool, source string
		w            want
	}{
		// read/collect and core execution categories.
		{"web_search", "webhook", want{allow, allow, allow}},
		{"browser", "webhook", want{allow, allow, allow}},
		{"knowledge_ingest", "spawn", want{allow, allow, allow}},
		{"search", "heartbeat", want{allow, allow, allow}},
		// GO-9（BUG-20260703）：cron 源自建 cron 是自我复制回路，转审批；
		// 仅 workflow 编排源保留 cron_task 自动放行。
		{"cron_task", "workflow", want{allow, allow, allow}},
		{"cron_task", "cron", want{deny, deny, deny}},
		{"cron_task", "webhook", want{deny, deny, deny}},
		{"cron_task", "spawn", want{deny, deny, deny}},
		{"manage_skill", "webhook", want{deny, deny, deny}},
		{"create_skill", "webhook", want{deny, deny, deny}},
		{"manage_mcp_server", "spawn", want{deny, deny, deny}},
		{"patch_skill", "cron", want{deny, deny, deny}},
		{"file_edit", "webhook", want{allow, allow, allow}},
		// exec 拆分(BUG-20260702)：外部源宿主直执行需审批，沙箱执行仍自动批准；
		// 内部编排源宿主直执行仍自动批准。
		{"shell", "cron", want{deny, deny, deny}},
		{"code", "webhook", want{deny, deny, deny}},
		{"shell", "workflow", want{allow, allow, allow}},
		{"shell", "spawn", want{allow, allow, allow}},
		{"code_exec", "webhook", want{allow, allow, allow}},
		{"send_message", "cron", want{allow, allow, allow}},
		{"media_generate", "webhook", want{allow, allow, allow}},
		{"publish_wechat", "webhook", want{deny, deny, deny}},
		{"app_heal", "workflow", want{allow, allow, allow}},
		{"app_heal", "cron", want{deny, deny, deny}},
		// unmatched tool under all-approval policy is denied by matrix.
		{"summarize", "webhook", want{deny, deny, deny}},
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
			opts := []PermissionHookOption{WithPolicy(NewPermissionPolicy(ActionAllow,
				PolicyRule{Name: "all-approve", ToolPattern: "*", Action: ActionRequireApproval, Risk: "sensitive"}))}
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
