package builtin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type codeExecCloseSequenceSandbox struct {
	errors []error
	calls  int
	onCall func(int)
}

func (*codeExecCloseSequenceSandbox) Exec(context.Context, sandbox.Command) (*sandbox.ExecResult, error) {
	return nil, errors.New("unexpected execution")
}

func (s *codeExecCloseSequenceSandbox) Close() error {
	s.calls++
	if s.onCall != nil {
		s.onCall(s.calls)
	}
	if s.calls <= len(s.errors) {
		return s.errors[s.calls-1]
	}
	return nil
}

func TestCloseCodeExecSandboxWithPolicyRetriesToConvergence(t *testing.T) {
	firstErr := errors.New("first close failed")
	sb := &codeExecCloseSequenceSandbox{errors: []error{firstErr, nil}}
	err := closeCodeExecSandboxWithPolicy(context.Background(), sb, 3, 0)
	if err != nil {
		t.Fatalf("close returned error after convergence: %v", err)
	}
	if sb.calls != 2 {
		t.Fatalf("Close calls = %d, want 2", sb.calls)
	}
}

func TestCloseCodeExecSandboxWithPolicyExhaustionIsNotSuccess(t *testing.T) {
	closeErr := errors.New("close remained unconfirmed")
	sb := &codeExecCloseSequenceSandbox{errors: []error{closeErr, closeErr, closeErr}}
	err := closeCodeExecSandboxWithPolicy(context.Background(), sb, 3, 0)
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v, want retained close error", err)
	}
	if sb.calls != 3 {
		t.Fatalf("Close calls = %d, want 3", sb.calls)
	}
}

func TestCloseCodeExecSandboxWithPolicyCancellationStopsRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	closeErr := errors.New("first close failed")
	sb := &codeExecCloseSequenceSandbox{
		errors: []error{closeErr},
		onCall: func(call int) {
			if call == 1 {
				cancel()
			}
		},
	}
	err := closeCodeExecSandboxWithPolicy(ctx, sb, 3, time.Hour)
	if !errors.Is(err, closeErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("close error = %v, want close failure and cancellation", err)
	}
	if sb.calls != 1 {
		t.Fatalf("Close calls = %d, want 1", sb.calls)
	}
}

func TestCloseCodeExecSandboxForPlatformRetriesOnlyOnWindows(t *testing.T) {
	for _, test := range []struct {
		name      string
		goos      string
		wantCalls int
		wantError bool
	}{
		{name: "windows converges", goos: "windows", wantCalls: 2},
		{name: "darwin remains single call", goos: "darwin", wantCalls: 1, wantError: true},
		{name: "linux remains single call", goos: "linux", wantCalls: 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			closeErr := errors.New("first close failed")
			sb := &codeExecCloseSequenceSandbox{errors: []error{closeErr, nil}}
			err := closeCodeExecSandboxForPlatform(context.Background(), sb, test.goos)
			if test.wantError != (err != nil) {
				t.Fatalf("close error = %v, wantError=%t", err, test.wantError)
			}
			if sb.calls != test.wantCalls {
				t.Fatalf("Close calls = %d, want %d", sb.calls, test.wantCalls)
			}
		})
	}
}

func TestJoinCodeExecSandboxClosePreservesRunAndExhaustedCloseErrors(t *testing.T) {
	runErr := errors.New("execution failed")
	closeErr := errors.New("close remained unconfirmed")
	sb := &codeExecCloseSequenceSandbox{errors: []error{closeErr, closeErr, closeErr}}
	err := joinCodeExecSandboxCloseForPlatform(
		context.Background(),
		runErr,
		sb,
		"close test sandbox",
		"windows",
	)
	if !errors.Is(err, runErr) || !errors.Is(err, closeErr) || !errors.Is(err, errCodeExecSandboxClose) {
		t.Fatalf("joined error = %v", err)
	}
	if sb.calls != 3 {
		t.Fatalf("Close calls = %d, want 3", sb.calls)
	}
}
