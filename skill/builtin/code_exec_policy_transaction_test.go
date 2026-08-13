package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type codeExecPolicySandbox struct {
	codeExecCloseTrackingSandbox
	capabilities    sandbox.CapabilitySet
	capabilitiesErr error
}

func (s *codeExecPolicySandbox) Capabilities(context.Context) (sandbox.CapabilitySet, error) {
	return s.capabilities, s.capabilitiesErr
}

func TestCodeExecSkill_PrepareSandboxPolicyDoesNotMutateBeforeCommit(t *testing.T) {
	oldPath := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	newPath := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, broker := newCodeExecPolicyTestSkill(t, SandboxPolicy{
		NetworkEnabled: false,
		ReadablePaths:  []string{oldPath},
	})
	s.sandboxFactory = codeExecPolicyValidationFactory(nil)
	requestedPaths := []string{newPath}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{
		ReadablePaths: requestedPaths,
	})
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy returned error: %v", err)
	}
	requestedPaths[0] = oldPath
	assertBuiltinSandboxPolicy(t, s.SandboxPolicy(), SandboxPolicy{
		NetworkEnabled: false,
		ReadablePaths:  []string{oldPath},
	})
	if got := broker.AllowedDirectories(); !slices.Equal(got, []string{oldPath}) {
		t.Fatalf("broker paths before Commit = %v, want old generation", got)
	}

	candidate.Commit()
	candidate.Commit()
	candidate.Discard()
	assertBuiltinSandboxPolicy(t, s.SandboxPolicy(), SandboxPolicy{
		ReadablePaths: []string{newPath},
	})
	if got := broker.AllowedDirectories(); !slices.Equal(got, []string{newPath}) {
		t.Fatalf("broker paths after Commit = %v, want new generation", got)
	}
}

func TestCodeExecSkill_PrepareSandboxPolicyBuildFailurePreservesState(t *testing.T) {
	oldPath := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{ReadablePaths: []string{oldPath}})
	buildErr := errors.New("build failed")
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return nil, buildErr
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if candidate != nil {
		t.Fatal("candidate must be nil after build failure")
	}
	if !errors.Is(err, buildErr) {
		t.Fatalf("PrepareSandboxPolicy error = %v, want build error", err)
	}
	assertBuiltinSandboxPolicy(t, s.SandboxPolicy(), SandboxPolicy{ReadablePaths: []string{oldPath}})

	s.sandboxFactory = codeExecPolicyValidationFactory(nil)
	next, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if err != nil {
		t.Fatalf("writer lock was not released after build failure: %v", err)
	}
	next.Discard()
}

func TestCodeExecSkill_PrepareSandboxPolicyCapabilityFailureJoinsCloseError(t *testing.T) {
	oldPath := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{ReadablePaths: []string{oldPath}})
	closeErr := errors.New("close failed")
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecPolicySandbox{
			codeExecCloseTrackingSandbox: codeExecCloseTrackingSandbox{closeErr: closeErr},
			capabilities:                 sandbox.UntrustedCodeIsolationCapabilities &^ sandbox.CapabilityNetwork,
		}, nil
	}

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if candidate != nil {
		t.Fatal("candidate must be nil when required capabilities are missing")
	}
	if !errors.Is(err, sandbox.ErrRequiredCapabilitiesUnavailable) || !errors.Is(err, closeErr) {
		t.Fatalf("PrepareSandboxPolicy error = %v, want capability and Close errors", err)
	}
	assertBuiltinSandboxPolicy(t, s.SandboxPolicy(), SandboxPolicy{ReadablePaths: []string{oldPath}})
}

func TestCodeExecSkill_PrepareSandboxPolicyCloseFailureRejectsCandidate(t *testing.T) {
	oldPath := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{ReadablePaths: []string{oldPath}})
	closeErr := errors.New("close failed")
	s.sandboxFactory = codeExecPolicyValidationFactory(closeErr)

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if candidate != nil {
		t.Fatal("candidate must be nil after validation Sandbox Close failure")
	}
	if !errors.Is(err, closeErr) || !errors.Is(err, errCodeExecSandboxClose) {
		t.Fatalf("PrepareSandboxPolicy error = %v, want observable Close failure", err)
	}
	assertBuiltinSandboxPolicy(t, s.SandboxPolicy(), SandboxPolicy{ReadablePaths: []string{oldPath}})
}

func TestCodeExecSkill_PrepareSandboxPolicyDiscardReleasesWriter(t *testing.T) {
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{})
	var factoryCalls atomic.Int32
	secondEntered := make(chan struct{})
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		if factoryCalls.Add(1) == 2 {
			close(secondEntered)
		}
		return &codeExecPolicySandbox{capabilities: sandbox.UntrustedCodeIsolationCapabilities}, nil
	}

	first, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{})
	if err != nil {
		t.Fatalf("first PrepareSandboxPolicy: %v", err)
	}
	secondResult := make(chan *SandboxPolicyCandidate, 1)
	secondErr := make(chan error, 1)
	go func() {
		candidate, prepareErr := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{ReadablePaths: []string{t.TempDir()}})
		secondResult <- candidate
		secondErr <- prepareErr
	}()
	select {
	case <-secondEntered:
		t.Fatal("second writer entered while first candidate was unresolved")
	case <-time.After(100 * time.Millisecond):
	}
	first.Discard()
	first.Discard()
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second writer did not enter after Discard")
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second PrepareSandboxPolicy: %v", err)
	}
	(<-secondResult).Discard()
}

func TestCodeExecSkill_SandboxPolicyReturnsDefensiveCopy(t *testing.T) {
	path := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{ReadablePaths: []string{path}})

	first := s.SandboxPolicy()
	first.ReadablePaths[0] = t.TempDir()
	second := s.SandboxPolicy()

	assertBuiltinSandboxPolicy(t, second, SandboxPolicy{ReadablePaths: []string{path}})
}

func TestCodeExecSkill_SandboxPolicyCommitUsesPreparedFileAccessState(t *testing.T) {
	oldTarget := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	newTarget := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "authorized")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatalf("create policy symlink: %v", err)
	}
	s, runtimeBroker := newCodeExecPolicyTestSkill(t, SandboxPolicy{})
	s.sandboxFactory = codeExecPolicyValidationFactory(nil)

	candidate, err := s.PrepareSandboxPolicy(context.Background(), SandboxPolicy{
		ReadablePaths: []string{link},
	})
	if err != nil {
		t.Fatalf("PrepareSandboxPolicy returned error: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove policy symlink: %v", err)
	}
	if err := os.Symlink(newTarget, link); err != nil {
		t.Fatalf("retarget policy symlink: %v", err)
	}
	candidate.Commit()

	if got := runtimeBroker.AllowedDirectories(); !slices.Equal(got, []string{oldTarget}) {
		t.Fatalf("runtime broker paths = %v, want Prepare-time target %v", got, []string{oldTarget})
	}
	_, executionBroker, _, _, _, _, _, _ := s.snapshot()
	if got := executionBroker.AllowedDirectories(); !slices.Equal(got, []string{oldTarget}) {
		t.Fatalf("execution broker paths = %v, want same prepared generation %v", got, []string{oldTarget})
	}
}

func TestCodeExecSkill_ConcurrentExecuteNeverObservesHybridPolicy(t *testing.T) {
	projectRoot := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	oldMarker := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	newMarker := normalizedCodeExecPolicyTestPath(t, t.TempDir())
	s, _ := newCodeExecPolicyTestSkill(t, SandboxPolicy{
		NetworkEnabled: false,
		ReadablePaths:  []string{projectRoot, oldMarker},
	})
	s.scratchBase = t.TempDir()
	var hybrid atomic.Bool
	s.projectStager = func(
		_ context.Context,
		_ string,
		stageRoot string,
		_ codeExecExecutionPlan,
		broker *FileAccessBroker,
		cfg sandbox.Config,
	) (string, string, bool, error) {
		paths := broker.AllowedDirectories()
		offlineGeneration := cfg.Network == sandbox.NetworkDisabled &&
			slices.Contains(cfg.ReadablePaths, oldMarker) && !slices.Contains(cfg.ReadablePaths, newMarker) &&
			slices.Contains(paths, oldMarker) && !slices.Contains(paths, newMarker)
		newGeneration := cfg.Network == sandbox.NetworkDisabled &&
			slices.Contains(cfg.ReadablePaths, newMarker) && !slices.Contains(cfg.ReadablePaths, oldMarker) &&
			slices.Contains(paths, newMarker) && !slices.Contains(paths, oldMarker)
		if !offlineGeneration && !newGeneration {
			hybrid.Store(true)
		}
		if err := os.MkdirAll(stageRoot, 0o755); err != nil {
			return "", "", false, err
		}
		return stageRoot, "", false, nil
	}
	s.sandboxFactory = codeExecPolicyValidationFactory(nil)
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for index := range 24 {
			policy := SandboxPolicy{
				ReadablePaths: []string{projectRoot, oldMarker},
			}
			if index%2 == 0 {
				policy.ReadablePaths = []string{projectRoot, newMarker}
			}
			candidate, prepareErr := s.PrepareSandboxPolicy(context.Background(), policy)
			if prepareErr != nil {
				errs <- prepareErr
				return
			}
			candidate.Commit()
		}
	}()
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, executeErr := s.Execute(context.Background(), map[string]any{
				"mode":         "project",
				"project_root": projectRoot,
				"command":      []string{executable},
			})
			if executeErr != nil {
				errs <- executeErr
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent policy operation: %v", err)
	}
	if hybrid.Load() {
		t.Fatal("Execute observed a hybrid Network/ReadablePaths policy generation")
	}
}

func newCodeExecPolicyTestSkill(t *testing.T, policy SandboxPolicy) (*CodeExecSkill, *FileAccessBroker) {
	t.Helper()
	cfg := sandbox.Config{
		Workspace:            t.TempDir(),
		Timeout:              30,
		Network:              sandbox.NetworkDisabled,
		ReadablePaths:        append([]string(nil), policy.ReadablePaths...),
		RequiredCapabilities: sandbox.UntrustedCodeIsolationCapabilities,
	}
	s := NewCodeExecSkill(nil, cfg)
	broker := NewFileAccessBroker(policy.ReadablePaths)
	s.SetFileAccess(broker)
	return s, broker
}

func codeExecPolicyValidationFactory(closeErr error) func(sandbox.Config) (sandbox.Sandbox, error) {
	return func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecPolicySandbox{
			codeExecCloseTrackingSandbox: codeExecCloseTrackingSandbox{
				result:   &sandbox.ExecResult{ExitCode: 0},
				closeErr: closeErr,
			},
			capabilities: sandbox.UntrustedCodeIsolationCapabilities,
		}, nil
	}
}

func assertBuiltinSandboxPolicy(t *testing.T, got, want SandboxPolicy) {
	t.Helper()
	if got.NetworkEnabled != want.NetworkEnabled || !slices.Equal(got.ReadablePaths, want.ReadablePaths) {
		t.Fatalf("SandboxPolicy = %+v, want %+v", got, want)
	}
}

func normalizedCodeExecPolicyTestPath(t *testing.T, path string) string {
	t.Helper()
	paths := normalizeAllowedPaths([]string{path})
	if len(paths) != 1 {
		t.Fatalf("normalize policy test path %q = %v", path, paths)
	}
	return paths[0]
}
