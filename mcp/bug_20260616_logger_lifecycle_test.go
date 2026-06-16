package mcp

// BUG-20260616: two logger lifecycle defects.
//   M1 rotate's reopen-failure branch deleted l.writers[filepath.Base(path)]
//      ("srv.log") but the map is keyed by bare server name ("srv"), so a writer
//      holding an already-closed fd was never evicted -> that server's logs went
//      permanently silent.
//   M2 Close set l.writers = nil; a late Log -> getWriter then assigned into the
//      nil map -> "assignment to entry in nil map" panic during shutdown.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBug20260616_RotateReopenFailEvictsWriter(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)

	stub, err := os.CreateTemp(dir, "stub")
	if err != nil {
		t.Fatal(err)
	}
	// Path under a missing subdir so rotate's O_CREATE reopen fails (ENOENT),
	// exercising the error branch. Key matches getWriter's convention: bare name.
	badPath := filepath.Join(dir, "missing", "srv.log")
	w := &logWriter{file: stub, path: badPath, size: 999, maxSize: 1}
	l.writers["srv"] = w

	l.rotate(w)

	if _, ok := l.writers["srv"]; ok {
		t.Fatalf("reopen failed but the stale closed-fd writer for 'srv' was not evicted (deleted wrong key)")
	}
}

func TestBug20260616_LogAfterCloseNoPanic(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	l.LogToolCall("srv", "tool", time.Millisecond, nil, 1, 1) // open a writer

	l.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Log after Close must drop the entry, not panic: %v", r)
		}
	}()
	l.LogToolCall("late", "tool", time.Millisecond, nil, 1, 1)
}
