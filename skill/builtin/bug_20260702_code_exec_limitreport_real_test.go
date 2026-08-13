package builtin

// BUG-20260702：code_exec 的能力报告曾把 resource_limits/fail_closed 当常量硬编码，
// 且用 reflect.FieldByName 读写 sandbox.Config/ExecResult 字段（GOWORK=off 发版构建下
// 字段找不到会静默返回 0/静默跳过，限额在 release 版蒸发、报告撒谎）。
//
// 收口后：直接字段读写（编译器守契约）+ 能力位接真（读 ExecResult.Limits 逐维如实报告）
// + 文件系统隔离降级如实标注 + ErrFilesystemContainmentUnavailable/ErrStorageLimitExceeded
// 明确归类。本文件 RED→GREEN 钉死上述不变量。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// RED（旧代码）：resource_limits/fail_closed 恒 true、无逐维 key；
// GREEN（接真）：据 ExecResult.Limits 如实反映。
func TestBug20260702_BuildReport_CapabilitiesReflectLimits(t *testing.T) {
	req := codeExecRequest{Mode: "snippet", Language: "python"}
	run := codeExecRun{ID: "run_test", Config: sandbox.Config{
		MaxOutputBytes: 1024, MaxStderrBytes: 1024, MaxWorkspaceBytes: 2048,
		MaxArtifactBytes: 4096, MaxMemoryBytes: 8192, MaxProcesses: 16,
	}}

	// 场景 A：darwin 式——内存不支持、文件系统仍强隔离。
	degraded := &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
		Memory:     sandbox.LimitStatusUnsupported,
		Processes:  sandbox.LimitStatusEnforced,
		Storage:    sandbox.LimitStatusEnforced,
		Output:     sandbox.LimitStatusEnforced,
		Filesystem: sandbox.LimitStatusEnforced,
	}}
	repA := buildCodeExecReport(req, run, []string{"python3", "x.py"}, degraded, nil, nil)
	if got := repA.Capabilities["resource_limits"]; got != false {
		t.Errorf("A: 内存 unsupported 时 resource_limits 应如实为 false，得 %v", got)
	}
	if got := repA.Capabilities["fail_closed"]; got != true {
		t.Errorf("A: 文件系统 enforced 时 fail_closed 应为 true，得 %v", got)
	}
	if got := repA.Capabilities["limit_memory"]; got != "unsupported" {
		t.Errorf("A: limit_memory 应逐维如实为 unsupported，得 %v", got)
	}
	if repA.FilesystemDegraded {
		t.Errorf("A: 文件系统 enforced 不应标降级")
	}
	// 直接字段读写：限额如实落进报告（非反射静默 0）。
	if repA.MaxMemoryBytes != 8192 || repA.MaxProcesses != 16 || repA.MaxWorkspaceBytes != 2048 {
		t.Errorf("A: 限额应直接来自 Config，得 mem=%d proc=%d ws=%d",
			repA.MaxMemoryBytes, repA.MaxProcesses, repA.MaxWorkspaceBytes)
	}

	// 场景 B：文件系统隔离不可用（toolkit v0.3.0 移除 Weak 状态，降级由 Unsupported 表达）——fail_closed 应据实为 false 并标降级。
	weakFS := &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
		Memory:     sandbox.LimitStatusEnforced,
		Processes:  sandbox.LimitStatusEnforced,
		Storage:    sandbox.LimitStatusEnforced,
		Output:     sandbox.LimitStatusEnforced,
		Filesystem: sandbox.LimitStatusUnsupported,
	}}
	repB := buildCodeExecReport(req, run, []string{"python3", "x.py"}, weakFS, nil, nil)
	if got := repB.Capabilities["fail_closed"]; got != false {
		t.Errorf("B: 文件系统 unsupported 时 fail_closed 应如实为 false（不再谎报 true），得 %v", got)
	}
	if got := repB.Capabilities["filesystem_isolation"]; got != "unsupported" {
		t.Errorf("B: filesystem_isolation 应为 unsupported，得 %v", got)
	}
	if !repB.FilesystemDegraded {
		t.Errorf("B: 文件系统 unsupported 应标降级")
	}
	if repB.FilesystemIsolation != "unsupported" {
		t.Errorf("B: FilesystemIsolation 应为 unsupported，得 %q", repB.FilesystemIsolation)
	}

	// 场景 C：全维 enforced（linux bwrap / windows）——两个汇总位都为 true。
	allEnforced := &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
		Memory:     sandbox.LimitStatusEnforced,
		Processes:  sandbox.LimitStatusEnforced,
		Storage:    sandbox.LimitStatusEnforced,
		Output:     sandbox.LimitStatusEnforced,
		Filesystem: sandbox.LimitStatusEnforced,
	}}
	repC := buildCodeExecReport(req, run, nil, allEnforced, nil, nil)
	if repC.Capabilities["resource_limits"] != true || repC.Capabilities["fail_closed"] != true {
		t.Errorf("C: 全维 enforced 时两汇总位应都为 true，得 rl=%v fc=%v",
			repC.Capabilities["resource_limits"], repC.Capabilities["fail_closed"])
	}
}

// RED（旧代码）：ErrFilesystemContainmentUnavailable 被吞成通用失败（无明确文案）；
// GREEN：Execute 给出明确可读的降级/拒绝执行错误，且标注文件系统降级。
func TestBug20260702_Execute_FilesystemContainmentUnavailable(t *testing.T) {
	s := newConfiguredTestCodeExecSkill(t, nil, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			return nil, fmt.Errorf("linux backend select: %w", sandbox.ErrFilesystemContainmentUnavailable)
		}}, nil
	}

	res, err := s.Execute(context.Background(), map[string]any{"language": "python", "code": "print(1)"})
	if err != nil {
		t.Fatalf("Execute 不应返回硬错误（分类信息在 report 内）：%v", err)
	}
	if res.Metadata["status"] != "failed" {
		t.Errorf("status 应为 failed，得 %q", res.Metadata["status"])
	}
	rep, ok := res.Data.(codeExecReport)
	if !ok {
		t.Fatalf("Data 应为 codeExecReport，得 %T", res.Data)
	}
	if !strings.Contains(rep.Error, "强文件系统隔离") {
		t.Errorf("错误应明确指向文件系统隔离缺失，得 %q", rep.Error)
	}
	if !rep.FilesystemDegraded {
		t.Errorf("文件系统隔离不可用应标降级")
	}
}

// GREEN：ErrStorageLimitExceeded 归类为「产物超限」（resource_limited），而非后端不可用。
func TestBug20260702_Execute_StorageLimitExceededClassified(t *testing.T) {
	s := newConfiguredTestCodeExecSkill(t, nil, sandbox.Config{Workspace: t.TempDir(), Timeout: 30})
	s.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{execFn: func(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
			return &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
				Memory: sandbox.LimitStatusEnforced, Processes: sandbox.LimitStatusEnforced,
				Storage: sandbox.LimitStatusEnforced, Output: sandbox.LimitStatusEnforced,
				Filesystem: sandbox.LimitStatusEnforced,
			}}, fmt.Errorf("walk: %w", sandbox.ErrStorageLimitExceeded)
		}}, nil
	}

	res, err := s.Execute(context.Background(), map[string]any{"language": "python", "code": "print(1)"})
	if err != nil {
		t.Fatalf("Execute 不应返回硬错误：%v", err)
	}
	rep, ok := res.Data.(codeExecReport)
	if !ok {
		t.Fatalf("Data 应为 codeExecReport，得 %T", res.Data)
	}
	if rep.Status != "resource_limited" {
		t.Errorf("存储超限应归类 resource_limited，得 %q", rep.Status)
	}
	if !rep.WorkspaceLimited {
		t.Errorf("存储超限应置 WorkspaceLimited")
	}
	if !strings.Contains(rep.Error, "存储限额") {
		t.Errorf("错误应指向存储限额，得 %q", rep.Error)
	}
}
