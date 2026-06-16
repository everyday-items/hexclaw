package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLogger_LogAndReadBack writes entries through LogToolCall and reads them
// back via ReadLogs, asserting field-level fidelity for both success and error.
func TestLogger_LogAndReadBack(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	l.LogToolCall("srv", "alpha", 1500*time.Millisecond, nil, 10, 20)
	l.LogToolCall("srv", "beta", 0, errors.New("boom"), 1, 0)

	logs, err := l.ReadLogs("srv", 0)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("want 2 entries, got %d", len(logs))
	}

	if logs[0].Tool != "alpha" || logs[0].Status != "success" {
		t.Errorf("entry0 unexpected: %+v", logs[0])
	}
	if logs[0].DurationMs != 1500 {
		t.Errorf("want DurationMs=1500, got %d", logs[0].DurationMs)
	}
	if logs[0].InputSize != 10 || logs[0].OutputSize != 20 {
		t.Errorf("entry0 sizes wrong: %+v", logs[0])
	}
	if logs[1].Status != "error" || logs[1].Error != "boom" {
		t.Errorf("entry1 should carry error: %+v", logs[1])
	}
}

// TestLogger_ReadLogs_Limit asserts the limit slices the most recent N entries.
func TestLogger_ReadLogs_Limit(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	for i := 0; i < 5; i++ {
		l.LogToolCall("srv", fmt.Sprintf("t%d", i), 0, nil, 0, 0)
	}

	logs, err := l.ReadLogs("srv", 2)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("want 2 (limited), got %d", len(logs))
	}
	// Most recent two are t3, t4.
	if logs[0].Tool != "t3" || logs[1].Tool != "t4" {
		t.Errorf("limit should keep tail; got %s,%s", logs[0].Tool, logs[1].Tool)
	}
}

// TestLogger_ReadLogs_MissingFile returns (nil, nil) for a server with no log.
func TestLogger_ReadLogs_MissingFile(t *testing.T) {
	l := NewLogger(t.TempDir())
	defer l.Close()

	logs, err := l.ReadLogs("never-written", 0)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if logs != nil {
		t.Errorf("want nil logs for missing file, got %v", logs)
	}
}

// TestLogger_Rotation forces the size threshold low to drive rotate() and
// asserts the rotated copy (.log.1) exists and the live file is fresh.
func TestLogger_Rotation(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	// Prime the writer, then shrink its maxSize so the next write triggers rotate.
	l.LogToolCall("srv", "seed", 0, nil, 0, 0)
	l.mu.Lock()
	w := l.writers["srv"]
	if w == nil {
		l.mu.Unlock()
		t.Fatal("writer not created after first log")
	}
	w.maxSize = 1 // any further write exceeds this and rotates
	l.mu.Unlock()

	l.LogToolCall("srv", "trigger", 0, nil, 0, 0)

	rotated := filepath.Join(dir, "srv.log.1")
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected rotated file %s: %v", rotated, err)
	}

	// Live file should be truncated/fresh after rotation, size reset to 0.
	l.mu.Lock()
	freshSize := l.writers["srv"].size
	l.mu.Unlock()
	if freshSize != 0 {
		t.Errorf("post-rotation live size should be 0, got %d", freshSize)
	}
}

// TestLogger_Rotation_ShiftsCopies verifies that repeated rotations cap at
// maxRotatedFiles copies and shift them upward (.1 -> .2 -> .3).
func TestLogger_Rotation_ShiftsCopies(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	l.LogToolCall("srv", "seed", 0, nil, 0, 0)
	l.mu.Lock()
	l.writers["srv"].maxSize = 1
	l.mu.Unlock()

	// Drive several rotations.
	for i := 0; i < 5; i++ {
		l.LogToolCall("srv", fmt.Sprintf("r%d", i), 0, nil, 0, 0)
		l.mu.Lock()
		l.writers["srv"].maxSize = 1
		l.mu.Unlock()
	}

	// At most maxRotatedFiles rotated copies should accumulate; .4 must not exist.
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("srv.log.%d", maxRotatedFiles+1))); !os.IsNotExist(err) {
		t.Errorf("rotated copies should cap at %d; found .%d", maxRotatedFiles, maxRotatedFiles+1)
	}
	if _, err := os.Stat(filepath.Join(dir, "srv.log.1")); err != nil {
		t.Errorf("expected at least one rotated copy: %v", err)
	}
}

// TestLogger_MultipleServers ensures each server name gets an isolated file.
func TestLogger_MultipleServers(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	l.LogToolCall("a", "x", 0, nil, 0, 0)
	l.LogToolCall("b", "y", 0, nil, 0, 0)

	la, _ := l.ReadLogs("a", 0)
	lb, _ := l.ReadLogs("b", 0)
	if len(la) != 1 || la[0].Tool != "x" {
		t.Errorf("server a isolated log wrong: %v", la)
	}
	if len(lb) != 1 || lb[0].Tool != "y" {
		t.Errorf("server b isolated log wrong: %v", lb)
	}
}

// TestLogger_Close_Idempotentish verifies Close is safe and ReadLogs (which
// reads the file directly) still works on the persisted data afterwards.
func TestLogger_Close(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	l.LogToolCall("srv", "x", 0, nil, 0, 0)
	l.Close()

	// Data persisted to disk; ReadLogs does not need open writers.
	logs, err := l.ReadLogs("srv", 0)
	if err != nil {
		t.Fatalf("ReadLogs after Close: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 persisted entry, got %d", len(logs))
	}
}

// TestSplitLines exercises the unexported line splitter directly, including a
// trailing line without a newline and skipped empty lines.
func TestSplitLines(t *testing.T) {
	in := []byte("a\nbb\n\nccc")
	got := splitLines(in)
	want := []string{"a", "bb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("want %d lines, got %d (%q)", len(want), len(got), got)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("line %d: want %q got %q", i, want[i], got[i])
		}
	}
}

// TestLogger_ReadLogs_SkipsMalformed ensures non-JSON lines are skipped rather
// than aborting the whole read.
func TestLogger_ReadLogs_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	defer l.Close()

	l.LogToolCall("srv", "good", 0, nil, 0, 0)
	// Append a junk line directly to the file.
	path := filepath.Join(dir, "srv.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString("not-json-at-all\n")
	f.Close()

	logs, err := l.ReadLogs("srv", 0)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].Tool != "good" {
		t.Errorf("malformed line should be skipped; got %v", logs)
	}
}

// TestLogger_DirCreated confirms NewLogger creates a nested directory.
func TestLogger_DirCreated(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "deep", "mcp")
	l := NewLogger(nested)
	defer l.Close()
	if !strings.HasPrefix(l.dir, base) {
		t.Fatalf("dir not set correctly: %s", l.dir)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("NewLogger should mkdir-all the nested dir: %v", err)
	}
}
