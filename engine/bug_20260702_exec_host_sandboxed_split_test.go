package engine

// BUG-20260702：exec 类别原本把「宿主直执行(shell/code)」与「沙箱执行(code_exec)」
// 塞进同一个开关，function_first 对 webhook/cron/heartbeat 自动批准 exec = 连宿主
// shell 一起放行给外部不可信触发源。拆成 exec_sandboxed / exec_host 后：
//   - 外部/无人值守源(webhook/cron/heartbeat)：只自动批准沙箱执行 code_exec，
//     宿主直执行 shell/code 转为需审批（不出现在自动批准集）。
//   - 内部编排源(workflow/spawn)：仍自动批准宿主直执行 shell/code（保留今日行为）。
//   - solve 仍空集。
//
// 本文件是钉死该不变量的“锁”，永久保留。RED（拆分前，exec 单开关）下
// webhook 自动批准 shell（=true）；GREEN（拆分后）webhook shell 需审批。

import (
	"context"
	"testing"
	"time"
)

// allApprovePolicy：把每个工具都置为 require_approval，让放行/拒绝完全由自动批准矩阵
// 决定，而非 baseline 的 ActionAllow 兜底。
func allApprovePolicyForSplit() *PermissionPolicy {
	return NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "all-approve", ToolPattern: "*", Action: ActionRequireApproval, Risk: "sensitive"})
}

func TestBug20260702_ExecSplit_ExternalSourcesSandboxedOnly(t *testing.T) {
	p := DefaultSystemDispatchPolicy()

	// 外部/无人值守源：沙箱执行 code_exec 自动批准；宿主直执行 shell/code 需审批。
	for _, src := range []string{webhookDispatchSource, cronDispatchSource, heartbeatDispatchSource} {
		if !p.Allows(src, "code_exec") {
			t.Errorf("%s 应自动批准沙箱执行 code_exec(exec_sandboxed)", src)
		}
		if p.Allows(src, "shell") {
			t.Errorf("%s 不应自动批准宿主直执行 shell(exec_host，需审批)", src)
		}
		if p.Allows(src, "code") {
			t.Errorf("%s 不应自动批准宿主直执行 code(exec_host，需审批)", src)
		}
	}

	// 内部编排源：宿主直执行 shell/code 仍自动批准，沙箱执行也放行。
	for _, src := range []string{workflowDispatchSource, spawnDispatchSource} {
		for _, tool := range []string{"shell", "code", "code_exec"} {
			if !p.Allows(src, tool) {
				t.Errorf("内部编排源 %s 应仍自动批准 %s", src, tool)
			}
		}
	}

	// solve 仍空集：连沙箱执行都不由矩阵自动放行（走不可伪造 grant 另说）。
	if p.Allows(solveDispatchSource, "code_exec") {
		t.Fatal("solve 源仍应是空集，不经矩阵自动批准 code_exec")
	}
}

// 经完整 permission hook 端到端复核同一不变量（all-approve 策略，无交互审批人）。
func TestBug20260702_ExecSplit_HookEndToEnd(t *testing.T) {
	newHook := func() *PermissionHook {
		return NewPermissionHook(NewPermissionHub(time.Second), WithPolicy(allApprovePolicyForSplit()))
	}

	// webhook：code_exec 自动放行，shell 需审批（无审批人 → 拒绝）。
	whCtx := withSystemDispatch(context.Background(), webhookDispatchSource)
	if err := newHook().BeforeToolCall(whCtx, &ToolCallInfo{Name: "code_exec", Source: "skill"}); err != nil {
		t.Errorf("webhook 应自动批准 code_exec，得 err=%v", err)
	}
	if err := newHook().BeforeToolCall(whCtx, &ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Error("webhook 宿主 shell 应需审批（无审批人时被拒），不应自动放行")
	}

	// workflow：宿主 shell 仍自动放行。
	wfCtx := withSystemDispatch(context.Background(), workflowDispatchSource)
	if err := newHook().BeforeToolCall(wfCtx, &ToolCallInfo{Name: "shell", Source: "skill"}); err != nil {
		t.Errorf("workflow 应仍自动批准宿主 shell，得 err=%v", err)
	}
}
