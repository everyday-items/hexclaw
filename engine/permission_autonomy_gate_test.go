package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

// fakeGrants 以 "source|taskRef|tool" 为键的可控 grant 检查器。
type fakeGrants struct {
	allow map[string]bool
}

func (f *fakeGrants) GrantAllows(source, taskRef, toolName string) bool {
	return f.allow[source+"|"+taskRef+"|"+toolName]
}

// captureRecorder 记录权限决策供断言。
type captureRecorder struct {
	mu   sync.Mutex
	decs []PermissionDecision
}

func (r *captureRecorder) RecordPermissionDecision(_ context.Context, d PermissionDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decs = append(r.decs, d)
}

func (r *captureRecorder) last(t *testing.T) PermissionDecision {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.decs) == 0 {
		t.Fatal("没有记录任何权限决策")
	}
	return r.decs[len(r.decs)-1]
}

func newAutonomyGateHook(grants TaskGrantChecker, rec PermissionDecisionRecorder) *PermissionHook {
	return NewPermissionHook(NewPermissionHub(0),
		WithPolicy(DefaultBaselinePolicy()),
		WithSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: "function_first"})),
		WithTaskGrants(grants),
		WithPermissionDecisionRecorder(rec),
	)
}

func webhookDispatchCtx(taskRef string) context.Context {
	ctx := withSystemDispatch(context.Background(), "webhook")
	return skill.WithSystemDispatchTask(ctx, taskRef)
}

func TestUnattendedConnectorToolRequiresExplicitAuthorization(t *testing.T) {
	rec := &captureRecorder{}
	hook := newAutonomyGateHook(&fakeGrants{}, rec)

	call := &ToolCallInfo{Name: "github.issues.write_label", Source: "mcp"}
	err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-1"), call)
	if err == nil {
		t.Fatal("无人值守下未授权的连接器工具应被拦下")
	}
	if !strings.Contains(err.Error(), "explicit authorization") {
		t.Fatalf("错误信息应指向显式授权：%v", err)
	}
	d := rec.last(t)
	if d.Decision != "pending" || d.Source != "webhook" || d.TaskRef != "webhook:wh-1" {
		t.Fatalf("决策记录不符: %+v", d)
	}
}

func TestUnattendedConnectorToolAllowedByTaskGrant(t *testing.T) {
	rec := &captureRecorder{}
	grants := &fakeGrants{allow: map[string]bool{
		"webhook|webhook:wh-1|github.issues.write_label": true,
	}}
	hook := newAutonomyGateHook(grants, rec)

	call := &ToolCallInfo{Name: "github.issues.write_label", Source: "mcp"}
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-1"), call); err != nil {
		t.Fatalf("任务级授权应放行连接器工具: %v", err)
	}
	d := rec.last(t)
	if d.Decision != "allow" || d.Via != "task_grant" {
		t.Fatalf("决策记录不符: %+v", d)
	}

	// 授权只属于该任务：换 task_ref 应仍被拦。
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-2"), call); err == nil {
		t.Fatal("授权不应泄漏到其他任务")
	}
}

func TestInteractiveConnectorToolKeepsDefaultAllow(t *testing.T) {
	hook := newAutonomyGateHook(&fakeGrants{}, &captureRecorder{})
	// 交互会话（无 dispatch source）保持基线默认放行，手感不变。
	call := &ToolCallInfo{Name: "github.issues.write_label", Source: "mcp"}
	if err := hook.BeforeToolCall(context.Background(), call); err != nil {
		t.Fatalf("交互会话不应经连接器收口: %v", err)
	}
}

func TestUnattendedSkillToolNotGatedByConnectorRule(t *testing.T) {
	hook := newAutonomyGateHook(&fakeGrants{}, &captureRecorder{})
	// 未归类的 skill/builtin 工具不经连接器闸（有 policy 规则治理）。
	call := &ToolCallInfo{Name: "memory_search", Source: "skill"}
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-1"), call); err != nil {
		t.Fatalf("skill 工具不应被连接器闸拦下: %v", err)
	}
}

func TestTaskGrantBeatsMatrixForApprovalTools(t *testing.T) {
	rec := &captureRecorder{}
	grants := &fakeGrants{allow: map[string]bool{
		"webhook|webhook:wh-1|shell": true,
	}}
	hook := newAutonomyGateHook(grants, rec)

	// function_first 下 webhook 源宿主执行（shell）转审批；任务级授权应赢过矩阵。
	call := &ToolCallInfo{Name: "shell", Source: "skill"}
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-1"), call); err != nil {
		t.Fatalf("任务级授权应放行 shell: %v", err)
	}
	d := rec.last(t)
	if d.Via != "task_grant" || d.Decision != "allow" {
		t.Fatalf("决策记录不符: %+v", d)
	}

	// 无授权任务照旧转待审批并留档 pending。
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-2"), call); err == nil {
		t.Fatal("无授权的 shell 应转审批")
	}
	d = rec.last(t)
	if d.Decision != "pending" || d.Capability != "exec_host" {
		t.Fatalf("pending 决策记录不符: %+v", d)
	}
}

func TestMatrixAllowRecordsDecision(t *testing.T) {
	rec := &captureRecorder{}
	hook := newAutonomyGateHook(&fakeGrants{}, rec)

	// function_first 下 webhook 源沙箱执行自动放行，且留档 allow/matrix。
	call := &ToolCallInfo{Name: "code_exec", Source: "skill"}
	if err := hook.BeforeToolCall(webhookDispatchCtx("webhook:wh-1"), call); err != nil {
		t.Fatalf("沙箱执行应自动放行: %v", err)
	}
	d := rec.last(t)
	if d.Decision != "allow" || d.Via != "matrix" || d.Capability != "exec_sandboxed" {
		t.Fatalf("决策记录不符: %+v", d)
	}
}

func TestSetSystemDispatchPolicyHotSwap(t *testing.T) {
	hook := newAutonomyGateHook(&fakeGrants{}, &captureRecorder{})
	call := &ToolCallInfo{Name: "shell", Source: "skill"}
	ctx := webhookDispatchCtx("webhook:wh-1")

	if err := hook.BeforeToolCall(ctx, call); err == nil {
		t.Fatal("function_first 下 webhook 源 shell 应转审批")
	}

	// 运行时切到 full_access：无需重启即生效。
	hook.SetSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: "full_access"}))
	if err := hook.BeforeToolCall(ctx, call); err != nil {
		t.Fatalf("full_access 热更后 shell 应放行: %v", err)
	}
	if got := hook.DispatchPolicy().Profile(); got != SystemDispatchProfileFullAccess {
		t.Fatalf("DispatchPolicy 应反映热更后 profile，得到 %q", got)
	}

	// 再切回严格档：立即收紧。
	hook.SetSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: "strict"}))
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "code_exec", Source: "skill"}); err == nil {
		t.Fatal("strict 热更后 code_exec 应转审批")
	}
}

func interactiveAutonomyContext() context.Context {
	ctx := skill.WithAuthenticatedUser(context.Background(), "interactive-owner")
	return context.WithValue(ctx, ctxKeySessionID, "interactive-session")
}

func newInteractiveAutonomyProfileHook(profile string) (*PermissionHook, *scriptedPermissionSender) {
	hub := NewPermissionHub(0)
	sender := &scriptedPermissionSender{hub: hub}
	hub.SetSender(sender)
	return NewPermissionHook(hub,
		WithPolicy(DefaultBaselinePolicy()),
		WithSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: profile})),
	), sender
}

func TestFullAccessInteractiveApprovalToolsNeverCreateApprovalRequest(t *testing.T) {
	for _, toolName := range []string{
		"browser", "code_exec", "shell", "code", "file_edit", "create_skill",
		"manage_skill", "manage_mcp_server", "patch_skill", "manage_skill_pending",
		"send_message", "app_heal", "media_generate", "publish_wechat",
	} {
		t.Run(toolName, func(t *testing.T) {
			hook, sender := newInteractiveAutonomyProfileHook(SystemDispatchProfileFullAccess)
			err := hook.BeforeToolCall(interactiveAutonomyContext(), &ToolCallInfo{Name: toolName, Source: "skill"})
			if err != nil {
				t.Fatalf("full_access 交互工具不应要求审批: %v", err)
			}
			if got := sender.callCount(); got != 0 {
				t.Fatalf("full_access 不得创建审批请求，得到 %d 次", got)
			}
		})
	}
}

func TestFunctionFirstInteractiveBasicToolsNeverCreateApprovalRequest(t *testing.T) {
	for _, toolName := range []string{"browser", "code_exec"} {
		t.Run(toolName, func(t *testing.T) {
			hook, sender := newInteractiveAutonomyProfileHook(SystemDispatchProfileFunctionFirst)
			err := hook.BeforeToolCall(interactiveAutonomyContext(), &ToolCallInfo{Name: toolName, Source: "skill"})
			if err != nil {
				t.Fatalf("function_first 的基础工具不应要求审批: %v", err)
			}
			if got := sender.callCount(); got != 0 {
				t.Fatalf("function_first 的基础工具不得创建审批请求，得到 %d 次", got)
			}
		})
	}

	hook, sender := newInteractiveAutonomyProfileHook(SystemDispatchProfileFunctionFirst)
	if err := hook.BeforeToolCall(interactiveAutonomyContext(), &ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Fatal("function_first 不应自动放行 shell")
	}
	if got := sender.callCount(); got != 1 {
		t.Fatalf("function_first 的 shell 应创建一次审批请求，得到 %d 次", got)
	}
}

func TestStrictInteractiveBasicToolsStillRequireApproval(t *testing.T) {
	for _, toolName := range []string{"browser", "code_exec"} {
		t.Run(toolName, func(t *testing.T) {
			hook, sender := newInteractiveAutonomyProfileHook(SystemDispatchProfileStrict)
			if err := hook.BeforeToolCall(interactiveAutonomyContext(), &ToolCallInfo{Name: toolName, Source: "skill"}); err == nil {
				t.Fatalf("strict 的 %s 不应自动放行", toolName)
			}
			if got := sender.callCount(); got != 1 {
				t.Fatalf("strict 的 %s 应创建一次审批请求，得到 %d 次", toolName, got)
			}
		})
	}
}

func TestFullAccessStaticDenyStillSkipsApprovalRequest(t *testing.T) {
	hub := NewPermissionHub(0)
	sender := &scriptedPermissionSender{hub: hub}
	hub.SetSender(sender)
	hook := NewPermissionHook(hub,
		WithPolicy(NewPermissionPolicy(ActionAllow, PolicyRule{
			Name: "deny-browser", ToolPattern: "browser", Action: ActionDeny, Reason: "blocked statically",
		})),
		WithSystemDispatchPolicy(FullAccessSystemDispatchPolicy()),
	)
	if err := hook.BeforeToolCall(interactiveAutonomyContext(), &ToolCallInfo{Name: "browser", Source: "skill"}); err == nil {
		t.Fatal("static deny 不得被 full_access 绕过")
	}
	if got := sender.callCount(); got != 0 {
		t.Fatalf("static deny 不得创建审批请求，得到 %d 次", got)
	}
}

// TestInteractiveAutonomyProfilesGateExecutorBoundary 走真实 ToolExecutor hook 链，
// 但用 no-op 闭包代替浏览器、代码执行和宿主命令，避免任何外部副作用。
func TestInteractiveAutonomyProfilesGateExecutorBoundary(t *testing.T) {
	staticDenyPolicy := NewPermissionPolicy(ActionAllow, PolicyRule{
		Name: "deny-browser", ToolPattern: "browser", Action: ActionDeny, Reason: "blocked statically",
	})
	tests := []struct {
		name          string
		profile       string
		toolName      string
		policy        *PermissionPolicy
		wantExecute   int
		wantApprovals int
		wantBlocked   bool
	}{
		{
			name: "full_access trusted interactive shell executes once without approval", profile: SystemDispatchProfileFullAccess,
			toolName: "shell", wantExecute: 1, wantApprovals: 0,
		},
		{
			name: "function_first browser executes once without approval", profile: SystemDispatchProfileFunctionFirst,
			toolName: "browser", wantExecute: 1, wantApprovals: 0,
		},
		{
			name: "function_first code_exec executes once without approval", profile: SystemDispatchProfileFunctionFirst,
			toolName: "code_exec", wantExecute: 1, wantApprovals: 0,
		},
		{
			name: "strict browser remains pending approval", profile: SystemDispatchProfileStrict,
			toolName: "browser", wantExecute: 0, wantApprovals: 1, wantBlocked: true,
		},
		{
			name: "strict code_exec remains pending approval", profile: SystemDispatchProfileStrict,
			toolName: "code_exec", wantExecute: 0, wantApprovals: 1, wantBlocked: true,
		},
		{
			name: "static deny blocks before approval or execution", profile: SystemDispatchProfileFullAccess,
			toolName: "browser", policy: staticDenyPolicy, wantExecute: 0, wantApprovals: 0, wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook, sender := newInteractiveAutonomyProfileHook(tt.profile)
			if tt.policy != nil {
				hub := NewPermissionHub(0)
				sender = &scriptedPermissionSender{hub: hub}
				hub.SetSender(sender)
				hook = NewPermissionHook(hub,
					WithPolicy(tt.policy),
					WithSystemDispatchPolicy(NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: tt.profile})),
				)
			}

			executor := NewToolExecutor(nil, nil)
			executor.AddHook(hook)
			executed := 0
			result, err := executor.executeWithHooks(interactiveAutonomyContext(), &ToolCallInfo{
				Name: tt.toolName, Source: "skill",
			}, func(context.Context) (string, error) {
				executed++
				return "no-op", nil
			})
			if tt.wantBlocked {
				if err == nil {
					t.Fatal("工具调用应在执行边界被拦下")
				}
			} else if err != nil {
				t.Fatalf("工具调用不应被拦下: %v", err)
			}
			if result != "" && tt.wantBlocked {
				t.Fatalf("被拦下的工具不应返回执行结果，得到 %q", result)
			}
			if result != "no-op" && !tt.wantBlocked {
				t.Fatalf("no-op executor result = %q, want %q", result, "no-op")
			}
			if executed != tt.wantExecute {
				t.Fatalf("executor calls = %d, want %d", executed, tt.wantExecute)
			}
			if got := sender.callCount(); got != tt.wantApprovals {
				t.Fatalf("approval sender calls = %d, want %d", got, tt.wantApprovals)
			}
		})
	}
}

func TestInteractiveDecisionsNotRecorded(t *testing.T) {
	rec := &captureRecorder{}
	hook := newAutonomyGateHook(&fakeGrants{}, rec)
	// 交互会话的审批不属于无人值守审计范围（有自己的会话审批 UX）。
	_ = hook.BeforeToolCall(context.Background(), &ToolCallInfo{Name: "search", Source: "skill"})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.decs) != 0 {
		t.Fatalf("交互会话不应写入无人值守审计: %+v", rec.decs)
	}
}
