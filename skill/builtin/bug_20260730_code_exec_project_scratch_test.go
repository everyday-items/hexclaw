package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// BUG-20260730-008: project-mode tests used the production-wide /tmp scratch
// root, so testing.T could not reclaim staged projects after each test.
func TestBug20260730CodeExecProjectTestScratchFollowsTestingLifecycle(t *testing.T) {
	globalBefore := codeExecGlobalScratchEntries(t)

	var (
		scratch       string
		testWorkspace string
	)
	t.Run("project run", func(t *testing.T) {
		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("fixture"), 0600); err != nil {
			t.Fatalf("write project fixture: %v", err)
		}

		s := newTestCodeExecSkill(t)
		testWorkspace = codeExecConfigForTest(s).Workspace
		result, err := s.Execute(context.Background(), map[string]any{
			"mode":         "project",
			"project_root": project,
			"command": []string{
				"sh",
				"-c",
				`test -f source.txt`,
			},
			"artifacts": false,
		})
		if err != nil {
			t.Fatalf("execute project fixture: %v", err)
		}
		report, ok := result.Data.(codeExecReport)
		if !ok {
			t.Fatalf("report type = %T, want codeExecReport", result.Data)
		}
		if report.Status != "success" {
			t.Fatalf("project run status = %q, output:\n%s", report.Status, result.Content)
		}
		scratch = report.Paths["workspace"]
		if scratch == "" {
			t.Fatal("project report workspace is empty")
		}
		if _, err := os.Stat(scratch); err != nil {
			t.Fatalf("scratch must remain readable while the test is active: %v", err)
		}
		if !pathWithinBase(scratch, testWorkspace) {
			t.Errorf("test project scratch %q escaped testing workspace %q", scratch, testWorkspace)
		}
	})

	// RED cleanup is deliberately exact and recoverable within this regression:
	// never leave the bug reproduction behind in the shared production scratch.
	defer func() {
		if scratch != "" {
			_ = os.RemoveAll(scratch)
		}
	}()

	if scratch == "" {
		t.Fatal("subtest did not return a scratch path")
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("test-owned project scratch survived testing.T cleanup: stat err=%v", err)
	}
	if _, err := os.Stat(testWorkspace); !os.IsNotExist(err) {
		t.Errorf("testing workspace survived subtest cleanup: stat err=%v", err)
	}

	globalAfter := codeExecGlobalScratchEntries(t)
	for name := range globalAfter {
		if !globalBefore[name] {
			t.Errorf("test leaked project scratch into global production root: %s", name)
		}
	}
}

func TestBug20260730CodeExecProductionProjectScratchDefaultIsUnchanged(t *testing.T) {
	project := t.TempDir()
	cfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30}
	s := NewCodeExecSkill(&mockSandbox{}, cfg)
	s.sandboxFactory = func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{}, nil
	}

	result, err := s.Execute(context.Background(), map[string]any{
		"mode":         "project",
		"project_root": project,
		"command":      []string{"sh", "-c", "true"},
	})
	if err != nil {
		t.Fatalf("execute production-default project fixture: %v", err)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("report type = %T, want codeExecReport", result.Data)
	}
	scratch := report.Paths["workspace"]
	if scratch == "" {
		t.Fatal("production-default project report workspace is empty")
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	globalRoot := filepath.Join(resolveRealPath(codeExecScratchBase()), "hexclaw-sandbox-runs")
	if !pathWithinBase(scratch, globalRoot) {
		t.Fatalf("production-default scratch %q left existing global root %q", scratch, globalRoot)
	}
	if pathWithinBase(scratch, cfg.Workspace) {
		t.Fatalf("production-default scratch unexpectedly moved into test workspace %q", cfg.Workspace)
	}
}

func codeExecGlobalScratchEntries(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join(resolveRealPath(codeExecScratchBase()), "hexclaw-sandbox-runs")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return map[string]bool{}
	}
	if err != nil {
		t.Fatalf("read global code_exec scratch root: %v", err)
	}
	result := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			result[entry.Name()] = true
		}
	}
	return result
}

func pathWithinBase(path, base string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	return err == nil &&
		!filepath.IsAbs(rel) &&
		rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
