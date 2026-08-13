//go:build darwin

package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

func TestCodeExecSkillExecuteGoRunUsesRealTwoPhaseDarwinBoundary(t *testing.T) {
	requireCodeExecSandbox(t)
	skill := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace:      t.TempDir(),
		Timeout:        120,
		Network:        sandbox.NetworkDisabled,
		MaxOutputBytes: 64 * 1024,
		MaxStderrBytes: 64 * 1024,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	result, err := skill.Execute(ctx, map[string]any{
		"mode":     "snippet",
		"language": "go",
		"code": `package main

import (
	"fmt"
	"os"
)

func main() {
	clean := true
	for _, key := range []string{"GOROOT", "GOCACHE", "GOTOOLCHAIN", "GOPROXY", "GOWORK"} {
		clean = clean && os.Getenv(key) == ""
	}
	fmt.Printf("REAL_TWO_PHASE_GO_OK build_env_empty=%t\n", clean)
}
`,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(result.Content, "REAL_TWO_PHASE_GO_OK build_env_empty=true") {
		t.Fatalf("real two-phase Go output is invalid: %s", result.Content)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data = %T, want codeExecReport", result.Data)
	}
	if report.Status != "success" || report.Capabilities["process_containment"] != true {
		t.Fatalf("strict execution boundary was not reported as enforced: %#v", report)
	}
}
