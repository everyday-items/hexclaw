package builtin

import (
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

func TestCodeExecGoResultAccumulatorOutputBudgetBoundary(t *testing.T) {
	tests := []struct {
		name            string
		result          sandbox.ExecResult
		wantStdout      string
		wantStderr      string
		wantStdoutLimit bool
		wantStderrLimit bool
		wantExhausted   bool
	}{
		{
			name:          "stdout exactly at limit remains available",
			result:        sandbox.ExecResult{Stdout: "1234", StdoutBytes: 4},
			wantStdout:    "1234",
			wantExhausted: false,
		},
		{
			name:            "stdout beyond limit is exhausted",
			result:          sandbox.ExecResult{Stdout: "12345", StdoutBytes: 5},
			wantStdout:      "1234",
			wantStdoutLimit: true,
			wantExhausted:   true,
		},
		{
			name:          "stderr exactly at limit remains available",
			result:        sandbox.ExecResult{Stderr: "1234", StderrBytes: 4},
			wantStderr:    "1234",
			wantExhausted: false,
		},
		{
			name:            "stderr beyond limit is exhausted",
			result:          sandbox.ExecResult{Stderr: "12345", StderrBytes: 5},
			wantStderr:      "1234",
			wantStderrLimit: true,
			wantExhausted:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := codeExecRun{Config: sandbox.Config{MaxOutputBytes: 4, MaxStderrBytes: 4}}
			accumulator := newCodeExecGoResultAccumulator(run)
			accumulator.add(&tt.result, false)

			got := accumulator.value()
			if got == nil {
				t.Fatal("accumulator result is nil")
			}
			if got.Stdout != tt.wantStdout || got.Stderr != tt.wantStderr {
				t.Fatalf("output = stdout %q stderr %q, want stdout %q stderr %q", got.Stdout, got.Stderr, tt.wantStdout, tt.wantStderr)
			}
			if got.StdoutTruncated != tt.wantStdoutLimit || got.StderrTruncated != tt.wantStderrLimit {
				t.Fatalf("truncated = stdout %t stderr %t, want stdout %t stderr %t", got.StdoutTruncated, got.StderrTruncated, tt.wantStdoutLimit, tt.wantStderrLimit)
			}
			if exhausted := accumulator.exhausted(); exhausted != tt.wantExhausted {
				t.Fatalf("exhausted = %t, want %t", exhausted, tt.wantExhausted)
			}
		})
	}
}
