package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestHandleUpdateFullConfig_SandboxPolicyPrepareFailureLeavesDiskMemoryRuntimeUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldPolicy := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/old/path"}}
	prepareErr := errors.New("candidate rejected")
	var prepared SandboxPolicy
	s := newSandboxPolicyTestServer(oldPolicy)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(_ context.Context, policy SandboxPolicy) (SandboxPolicyCandidate, error) {
			prepared = policy
			return SandboxPolicyCandidate{}, prepareErr
		},
		Snapshot: func() SandboxPolicy { return cloneSandboxPolicy(oldPolicy) },
	})

	w := performSandboxPolicyUpdate(t, s, `{"security":{"gateway_enabled":false},"sandbox":{"network_enabled":false,"allowed_paths":["/new/path"]}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Sandbox policy validation failed") {
		t.Fatalf("body = %s, want English validation error", w.Body.String())
	}
	if prepared.NetworkEnabled || !slices.Equal(prepared.ReadablePaths, []string{"/new/path"}) {
		t.Fatalf("prepared policy = %+v, want complete requested policy", prepared)
	}
	assertSandboxPolicyConfig(t, s.cfg, oldPolicy)
	if !s.cfg.Security.Auth.Enabled {
		t.Fatal("memory config changed after Prepare failure")
	}
	if _, err := os.Stat(filepath.Join(home, ".hexclaw", "hexclaw.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config file state after Prepare failure = %v, want not written", err)
	}
}

func TestHandleUpdateFullConfig_SandboxPolicyPersistFailureDiscardsCandidate(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(homeFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("create invalid HOME fixture: %v", err)
	}
	t.Setenv("HOME", homeFile)
	oldPolicy := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/old/path"}}
	var commits atomic.Int32
	var discards atomic.Int32
	s := newSandboxPolicyTestServer(oldPolicy)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(context.Context, SandboxPolicy) (SandboxPolicyCandidate, error) {
			return NewSandboxPolicyCandidate(
				func() { commits.Add(1) },
				func() { discards.Add(1) },
			), nil
		},
		Snapshot: func() SandboxPolicy { return cloneSandboxPolicy(oldPolicy) },
	})

	w := performSandboxPolicyUpdate(t, s, `{"sandbox":{"network_enabled":false,"allowed_paths":["/new/path"]}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to save configuration") {
		t.Fatalf("body = %s, want English persistence error", w.Body.String())
	}
	if got := commits.Load(); got != 0 {
		t.Fatalf("Commit calls = %d, want 0", got)
	}
	if got := discards.Load(); got != 1 {
		t.Fatalf("Discard calls = %d, want 1", got)
	}
	assertSandboxPolicyConfig(t, s.cfg, oldPolicy)
}

func TestHandleUpdateFullConfig_SandboxPolicyPersistsBeforeCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldPolicy := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/old/path"}}
	wantPolicy := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/new/path", "/other/path"}}
	var runtimePolicy = cloneSandboxPolicy(oldPolicy)
	var commits atomic.Int32
	var discards atomic.Int32
	s := newSandboxPolicyTestServer(oldPolicy)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(_ context.Context, policy SandboxPolicy) (SandboxPolicyCandidate, error) {
			if !equalSandboxPolicy(policy, wantPolicy) {
				t.Fatalf("prepared policy = %+v, want %+v", policy, wantPolicy)
			}
			return NewSandboxPolicyCandidate(func() {
				persisted, err := config.Load("")
				if err != nil {
					t.Fatalf("load persisted config during Commit: %v", err)
				}
				assertSandboxPolicyConfig(t, persisted, wantPolicy)
				commits.Add(1)
				runtimePolicy = cloneSandboxPolicy(policy)
			}, func() {
				discards.Add(1)
			}), nil
		},
		Snapshot: func() SandboxPolicy { return cloneSandboxPolicy(runtimePolicy) },
	})

	w := performSandboxPolicyUpdate(t, s, `{"sandbox":{"allowed_paths":["/new/path","/other/path"]}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if got := commits.Load(); got != 1 {
		t.Fatalf("Commit calls = %d, want 1", got)
	}
	if got := discards.Load(); got != 0 {
		t.Fatalf("Discard callback calls = %d, want 0 after Commit", got)
	}
	if !equalSandboxPolicy(runtimePolicy, wantPolicy) {
		t.Fatalf("runtime policy = %+v, want %+v", runtimePolicy, wantPolicy)
	}
	assertSandboxPolicyConfig(t, s.cfg, wantPolicy)
	if !strings.Contains(w.Body.String(), "Configuration updated") {
		t.Fatalf("body = %s, want English success message", w.Body.String())
	}
}

func TestHandleUpdateFullConfig_SandboxPolicyPartialPatchPreparesCompletePolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldPolicy := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/kept/path"}}
	var prepared SandboxPolicy
	s := newSandboxPolicyTestServer(oldPolicy)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(_ context.Context, policy SandboxPolicy) (SandboxPolicyCandidate, error) {
			prepared = cloneSandboxPolicy(policy)
			return NewSandboxPolicyCandidate(func() {}, func() {}), nil
		},
		Snapshot: func() SandboxPolicy { return cloneSandboxPolicy(oldPolicy) },
	})

	w := performSandboxPolicyUpdate(t, s, `{"sandbox":{"network_enabled":false}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	want := SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/kept/path"}}
	if !equalSandboxPolicy(prepared, want) {
		t.Fatalf("prepared policy = %+v, want complete partial-patch policy %+v", prepared, want)
	}
}

func TestHandleGetFullConfig_UsesSingleSandboxPolicySnapshot(t *testing.T) {
	s := newSandboxPolicyTestServer(SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/config/path"}})
	var calls atomic.Int32
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(context.Context, SandboxPolicy) (SandboxPolicyCandidate, error) {
			t.Fatal("GET must not prepare a policy")
			return SandboxPolicyCandidate{}, nil
		},
		Snapshot: func() SandboxPolicy {
			calls.Add(1)
			return SandboxPolicy{NetworkEnabled: false, ReadablePaths: []string{"/runtime/path"}}
		},
	})

	w := httptest.NewRecorder()
	s.handleGetFullConfig(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/config", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Snapshot calls = %d, want 1", got)
	}
	if !strings.Contains(w.Body.String(), `"network_enabled":false`) ||
		!strings.Contains(w.Body.String(), `"allowed_paths":["/runtime/path"]`) {
		t.Fatalf("response does not use one runtime policy generation: %s", w.Body.String())
	}
}

func TestHandleUpdateFullConfig_SandboxPolicyConcurrentUpdatesAreSerializable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newSandboxPolicyTestServer(SandboxPolicy{})
	firstPrepared := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondPrepared := make(chan struct{})
	var prepares atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(_ context.Context, _ SandboxPolicy) (SandboxPolicyCandidate, error) {
			call := prepares.Add(1)
			current := active.Add(1)
			for {
				observed := maxActive.Load()
				if current <= observed || maxActive.CompareAndSwap(observed, current) {
					break
				}
			}
			if call == 1 {
				close(firstPrepared)
				<-releaseFirst
			} else {
				close(secondPrepared)
			}
			active.Add(-1)
			return NewSandboxPolicyCandidate(func() {}, func() {}), nil
		},
		Snapshot: func() SandboxPolicy { return SandboxPolicy{} },
	})

	results := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		results <- performSandboxPolicyUpdate(t, s, `{"sandbox":{"allowed_paths":["/first"]}}`)
	}()
	select {
	case <-firstPrepared:
	case <-time.After(5 * time.Second):
		t.Fatal("first Prepare did not start")
	}
	go func() {
		results <- performSandboxPolicyUpdate(t, s, `{"sandbox":{"allowed_paths":["/second"]}}`)
	}()
	select {
	case <-secondPrepared:
		t.Fatal("second Prepare entered before the first transaction completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondPrepared:
	case <-time.After(5 * time.Second):
		t.Fatal("second Prepare did not start after the first transaction completed")
	}
	for range 2 {
		select {
		case result := <-results:
			if result.Code != http.StatusOK {
				t.Fatalf("concurrent update status = %d, body=%s", result.Code, result.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent update did not complete")
		}
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent Prepare calls = %d, want 1", got)
	}
}

func newSandboxPolicyTestServer(policy SandboxPolicy) *Server {
	cfg := config.DefaultConfig()
	network := policy.NetworkEnabled
	cfg.Skill.Builtin.CodeExecPolicy.Network = &network
	cfg.Skill.Sandbox.Filesystem.AllowedPaths = append([]string(nil), policy.ReadablePaths...)
	cfg.Security.Auth.Enabled = true
	return &Server{cfg: cfg, logCollector: NewLogCollector(10)}
}

func performSandboxPolicyUpdate(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/config", strings.NewReader(body))
	s.handleUpdateFullConfig(w, req)
	return w
}

func assertSandboxPolicyConfig(t *testing.T, cfg *config.Config, want SandboxPolicy) {
	t.Helper()
	if got := cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed(); got != want.NetworkEnabled {
		t.Fatalf("config network = %v, want %v", got, want.NetworkEnabled)
	}
	if got := cfg.Skill.Sandbox.Filesystem.AllowedPaths; !slices.Equal(got, want.ReadablePaths) {
		t.Fatalf("config allowed paths = %v, want %v", got, want.ReadablePaths)
	}
}

func cloneSandboxPolicy(policy SandboxPolicy) SandboxPolicy {
	policy.ReadablePaths = append([]string(nil), policy.ReadablePaths...)
	return policy
}

func equalSandboxPolicy(left, right SandboxPolicy) bool {
	return left.NetworkEnabled == right.NetworkEnabled && slices.Equal(left.ReadablePaths, right.ReadablePaths)
}
