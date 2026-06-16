package builtin

// BUG-20260616: the shell allowlist trusted "find" as a read-only command, but
// find's action primaries (-exec/-delete/-fprintf...) run arbitrary commands and
// delete/write files, bypassing the only safety control on the shell skill.

import "testing"

func TestBug20260616_ShellFindActionPrimitivesBlocked(t *testing.T) {
	for _, cmd := range []string{
		"find . -name x -exec rm -rf {} +",
		"find . -delete",
	} {
		if reason := checkAllowed(cmd); reason == "" {
			t.Errorf("dangerous find command was allowed: %q", cmd)
		}
	}
	// A plain read-only find must still pass.
	if reason := checkAllowed("find . -name *.go"); reason != "" {
		t.Errorf("read-only find should remain allowed, got %q", reason)
	}
}
