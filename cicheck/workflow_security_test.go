package cicheck

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var immutableActionRef = regexp.MustCompile(`^[^\s#]+@[0-9a-f]{40}(?:\s*#.*)?$`)
var mutableRunnerImage = regexp.MustCompile(`\b(?:ubuntu|macos|windows)-latest\b`)

func TestWorkflowDependenciesAreImmutable(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), ".."))
	workflowFiles, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(workflowFiles) == 0 {
		t.Fatal("no GitHub Actions workflows found")
	}

	for _, workflowFile := range workflowFiles {
		body, err := os.ReadFile(workflowFile)
		if err != nil {
			t.Fatalf("read %s: %v", workflowFile, err)
		}
		for lineNumber, rawLine := range strings.Split(string(body), "\n") {
			line := strings.TrimSpace(rawLine)
			switch {
			case strings.HasPrefix(line, "uses:") || strings.HasPrefix(line, "- uses:"):
				ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "uses:"))
				if strings.HasPrefix(ref, "./") {
					continue
				}
				if !immutableActionRef.MatchString(ref) {
					t.Errorf("%s:%d mutable action reference %q", workflowFile, lineNumber+1, ref)
				}
			case !strings.HasPrefix(line, "#") && mutableRunnerImage.MatchString(line):
				t.Errorf("%s:%d mutable runner image %q", workflowFile, lineNumber+1, line)
			case strings.Contains(line, "go install ") && strings.Contains(line, "@latest"):
				t.Errorf("%s:%d mutable Go tool version %q", workflowFile, lineNumber+1, line)
			}
		}
	}
}
