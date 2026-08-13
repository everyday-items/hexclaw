package builtin

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type codeExecCloseTrackingSandbox struct {
	result     *sandbox.ExecResult
	execErr    error
	closeErr   error
	closeCalls atomic.Int32
}

func (s *codeExecCloseTrackingSandbox) Exec(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
	return s.result, s.execErr
}

func (s *codeExecCloseTrackingSandbox) Close() error {
	s.closeCalls.Add(1)
	return s.closeErr
}

func (s *codeExecCloseTrackingSandbox) Capabilities(context.Context) (sandbox.CapabilitySet, error) {
	return sandbox.UntrustedCodeIsolationCapabilities |
		sandbox.CapabilityMemory |
		sandbox.CapabilityProcesses |
		sandbox.CapabilityStorage, nil
}

func TestToolkitSandboxMigration_DefaultBudgetsDoNotRequestUnsupportedHardLimits(t *testing.T) {
	workspace := t.TempDir()
	var captured sandbox.Config
	runSandbox := &codeExecCloseTrackingSandbox{result: &sandbox.ExecResult{
		ExitCode: 0,
		Limits: sandbox.LimitReport{
			Filesystem:         sandbox.LimitStatusEnforced,
			ProcessContainment: sandbox.LimitStatusEnforced,
			Output:             sandbox.LimitStatusEnforced,
		},
	}}
	skill := NewCodeExecSkill(nil, sandbox.Config{Workspace: workspace, Timeout: 30})
	skill.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return runSandbox, nil
	}

	result, err := skill.Execute(context.Background(), map[string]any{
		"language": "python",
		"code":     "print('ok')",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if captured.MaxMemoryBytes != 0 || captured.MaxProcesses != 0 ||
		captured.MaxWorkspaceBytes != 0 || captured.MaxArtifactBytes != 0 {
		t.Fatalf("default sandbox hard limits = memory:%d processes:%d workspace:%d artifact:%d, want all zero",
			captured.MaxMemoryBytes,
			captured.MaxProcesses,
			captured.MaxWorkspaceBytes,
			captured.MaxArtifactBytes,
		)
	}
	if captured.RequiredCapabilities != sandbox.UntrustedCodeIsolationCapabilities {
		t.Fatalf("RequiredCapabilities = %s, want %s", captured.RequiredCapabilities, sandbox.UntrustedCodeIsolationCapabilities)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data type = %T, want codeExecReport", result.Data)
	}
	if report.MaxWorkspaceBytes <= 0 || report.MaxArtifactBytes <= 0 {
		t.Fatalf("application budgets = workspace:%d artifact:%d, want positive bounded defaults",
			report.MaxWorkspaceBytes,
			report.MaxArtifactBytes,
		)
	}
	if got := runSandbox.closeCalls.Load(); got != 1 {
		t.Fatalf("run sandbox Close calls = %d, want 1", got)
	}
}

func TestToolkitSandboxMigration_StaleResourceCapabilitiesAreRebuiltFromLimits(t *testing.T) {
	want := sandbox.UntrustedCodeIsolationCapabilities | sandbox.CapabilityMemory
	cfg := withCodeExecRequiredCapabilities(sandbox.Config{
		RequiredCapabilities: sandbox.CapabilityMemory |
			sandbox.CapabilityProcesses |
			sandbox.CapabilityStorage,
		MaxMemoryBytes: 1,
	})
	if cfg.RequiredCapabilities != want {
		t.Fatalf("RequiredCapabilities = %s, want %s", cfg.RequiredCapabilities, want)
	}
}

func TestToolkitSandboxMigration_GoHelperClosesSandboxAndPreservesErrors(t *testing.T) {
	workspace := t.TempDir()
	goBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	execErr := errors.New("helper execution failed")
	closeErr := errors.New("helper close failed")
	helperSandbox := &codeExecCloseTrackingSandbox{execErr: execErr, closeErr: closeErr}
	var captured sandbox.Config
	helper := codeExecGoHelper{Factory: func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return helperSandbox, nil
	}}

	_, err = helper.Run(
		context.Background(),
		sandbox.Config{Workspace: workspace, Timeout: 30},
		workspace,
		workspace,
		nil,
		goBinary,
		[]string{"version"},
		nil,
	)
	if !errors.Is(err, execErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Go helper error = %v, want joined execution and Close errors", err)
	}
	if got := helperSandbox.closeCalls.Load(); got != 1 {
		t.Fatalf("Go helper sandbox Close calls = %d, want 1", got)
	}
	if captured.MaxMemoryBytes != 0 || captured.MaxProcesses != 0 ||
		captured.MaxWorkspaceBytes != 0 || captured.MaxArtifactBytes != 0 {
		t.Fatalf("Go helper default hard limits = memory:%d processes:%d workspace:%d artifact:%d, want all zero",
			captured.MaxMemoryBytes,
			captured.MaxProcesses,
			captured.MaxWorkspaceBytes,
			captured.MaxArtifactBytes,
		)
	}
	if captured.RequiredCapabilities != sandbox.TrustedBuildIsolationCapabilities {
		t.Fatalf("Go helper RequiredCapabilities = %s, want %s",
			captured.RequiredCapabilities,
			sandbox.TrustedBuildIsolationCapabilities,
		)
	}
}

func TestToolkitSandboxMigration_GoHelperCloseFailureCannotBeMaskedBySuccess(t *testing.T) {
	workspace := t.TempDir()
	goBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	closeErr := errors.New("helper close failed")
	helperSandbox := &codeExecCloseTrackingSandbox{
		result:   &sandbox.ExecResult{ExitCode: 0},
		closeErr: closeErr,
	}
	helper := codeExecGoHelper{Factory: func(sandbox.Config) (sandbox.Sandbox, error) {
		return helperSandbox, nil
	}}

	result, err := helper.Run(
		context.Background(),
		sandbox.Config{Workspace: workspace},
		workspace,
		workspace,
		nil,
		goBinary,
		[]string{"version"},
		nil,
	)
	if !errors.Is(err, closeErr) || !errors.Is(err, errCodeExecSandboxClose) {
		t.Fatalf("Go helper error = %v, want observable Close failure", err)
	}
	if codeExecGoHelperResultSucceeded(result, err) {
		t.Fatal("successful process exit must not mask Go helper Close failure")
	}
}

func TestToolkitSandboxMigration_RunSandboxPreservesExecutionAndCloseErrors(t *testing.T) {
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	execErr := errors.New("run execution failed")
	closeErr := errors.New("run close failed")
	runSandbox := &codeExecCloseTrackingSandbox{execErr: execErr, closeErr: closeErr}
	run := codeExecRun{
		ID:          "close-contract",
		Workspace:   workspace,
		Scratch:     workspace,
		ArtifactDir: workspace,
		CacheDir:    workspace,
		Config:      sandbox.Config{Network: sandbox.NetworkDisabled},
	}

	_, err, _ = runCodeExecWithOwnedSandbox(
		context.Background(),
		runSandbox,
		run,
		codeExecRequest{Language: "python"},
		[]string{executable},
	)
	if !errors.Is(err, execErr) || !errors.Is(err, closeErr) {
		t.Fatalf("run error = %v, want joined execution and Close errors", err)
	}
	if got := runSandbox.closeCalls.Load(); got != 1 {
		t.Fatalf("run sandbox Close calls = %d, want 1", got)
	}
}

func TestToolkitSandboxMigration_ValidationSandboxesAreNotRetained(t *testing.T) {
	initial := &codeExecCloseTrackingSandbox{}
	skill := NewCodeExecSkill(initial, sandbox.Config{Workspace: t.TempDir(), Network: sandbox.NetworkDisabled})
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("initial validation sandbox Close calls = %d, want 1", got)
	}

	candidate := &codeExecCloseTrackingSandbox{}
	skill.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return candidate, nil
	}
	candidateUpdate, err := skill.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy returned error: %v", err)
	}
	candidateUpdate.Commit()
	if got := candidate.closeCalls.Load(); got != 1 {
		t.Fatalf("candidate validation sandbox Close calls = %d, want 1", got)
	}
}

func TestToolkitSandboxMigration_ValidationCloseErrorsRemainObservable(t *testing.T) {
	initialCloseErr := errors.New("initial close failed")
	initial := &codeExecCloseTrackingSandbox{closeErr: initialCloseErr}
	skill := NewCodeExecSkill(initial, sandbox.Config{Workspace: t.TempDir(), Network: sandbox.NetworkDisabled})
	if _, err := skill.Execute(context.Background(), nil); !errors.Is(err, initialCloseErr) {
		t.Fatalf("Execute error = %v, want initial Close error", err)
	}

	candidateCloseErr := errors.New("candidate close failed")
	candidate := &codeExecCloseTrackingSandbox{closeErr: candidateCloseErr}
	skill = NewCodeExecSkill(nil, sandbox.Config{Workspace: t.TempDir(), Network: sandbox.NetworkDisabled})
	skill.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return candidate, nil
	}
	candidateUpdate, err := skill.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if candidateUpdate != nil {
		t.Fatal("candidate must be nil after validation Close failure")
	}
	if !errors.Is(err, candidateCloseErr) {
		t.Fatalf("PrepareSandboxPolicy error = %v, want candidate Close error", err)
	}
	if skill.SandboxPolicy().NetworkEnabled {
		t.Fatal("network policy changed after candidate Close failure")
	}
}

func TestToolkitSandboxMigration_ReportUsesProcessContainmentOnly(t *testing.T) {
	capabilities := buildCodeExecCapabilities(sandbox.LimitReport{
		ProcessContainment: sandbox.LimitStatusEnforced,
	})
	if got, ok := capabilities["process_containment"].(bool); !ok || !got {
		t.Fatalf("process_containment = %#v, want true", capabilities["process_containment"])
	}
	if _, exists := capabilities["process_tree_kill"]; exists {
		t.Fatal("legacy process_tree_kill capability must not be reported")
	}
}
