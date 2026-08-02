//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAtomicWriteFileTempSyncFailurePreservesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("injected temp sync failure")
	replaceCalled := false
	parentSyncCalled := false
	err := atomicWriteFileWithOps(path, []byte("new"), 0o600, atomicWriteOps{
		syncTemp: func(*os.File) error { return wantErr },
		replace: func(string, string) error {
			replaceCalled = true
			return nil
		},
		syncParent: func(string) error {
			parentSyncCalled = true
			return nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("atomicWriteFileWithOps error = %v, want wrapped %v", err, wantErr)
	}
	if replaceCalled || parentSyncCalled {
		t.Fatalf("commit steps ran after temp sync failure: replace=%v parent_sync=%v", replaceCalled, parentSyncCalled)
	}
	assertAtomicWriteState(t, dir, path, "old", 0)
}

func TestAtomicWriteFileReplaceFailurePreservesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("injected replace failure")
	parentSyncCalled := false
	err := atomicWriteFileWithOps(path, []byte("new"), 0o600, atomicWriteOps{
		syncTemp: (*os.File).Sync,
		replace:  func(string, string) error { return wantErr },
		syncParent: func(string) error {
			parentSyncCalled = true
			return nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("atomicWriteFileWithOps error = %v, want wrapped %v", err, wantErr)
	}
	if parentSyncCalled {
		t.Fatal("parent directory sync ran even though replacement did not commit")
	}
	assertAtomicWriteState(t, dir, path, "old", 0)
}

func TestAtomicWriteFileParentSyncFailureReportsCommittedOutcome(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("injected parent sync failure")
	err := atomicWriteFileWithOps(path, []byte("new"), 0o600, atomicWriteOps{
		syncTemp:   (*os.File).Sync,
		replace:    os.Rename,
		syncParent: func(string) error { return wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("atomicWriteFileWithOps error = %v, want wrapped %v", err, wantErr)
	}
	var committed *PostCommitDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("parent sync failure after replace must expose committed outcome, got %T: %v", err, err)
	}
	if !committed.ReadbackVerified() {
		t.Fatalf("committed write must be verified by readback: %+v", committed)
	}
	if committed.ExpectedSHA256() == "" || committed.ExpectedSHA256() != committed.ObservedSHA256() {
		t.Fatalf("readback digests do not prove the committed bytes: expected=%q observed=%q", committed.ExpectedSHA256(), committed.ObservedSHA256())
	}
	assertAtomicWriteState(t, dir, path, "new", 0)
}

func TestAtomicWriteFileParentSyncFailureDoesNotClaimUnverifiedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	wantErr := errors.New("injected parent sync failure")

	err := atomicWriteFileWithOps(path, []byte("expected"), 0o600, atomicWriteOps{
		syncTemp: (*os.File).Sync,
		replace: func(oldPath, newPath string) error {
			if err := os.Rename(oldPath, newPath); err != nil {
				return err
			}
			return os.WriteFile(newPath, []byte("different"), 0o600)
		},
		syncParent: func(string) error { return wantErr },
	})
	var committed *PostCommitDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("parent sync failure after replace must expose committed outcome, got %T: %v", err, err)
	}
	if committed.ReadbackVerified() {
		t.Fatalf("mismatched readback must not be accepted as the intended commit: %+v", committed)
	}
	if ReconcileCommittedWrite(err) == nil {
		t.Fatal("unverified post-commit outcome must remain an error")
	}
}

func TestReconcileCommittedWriteAcceptsOnlyVerifiedPostCommitOutcome(t *testing.T) {
	err := &PostCommitDurabilityError{
		Cause:          errors.New("directory sync unavailable"),
		expectedSHA256: "same",
		observedSHA256: "same",
	}
	if got := ReconcileCommittedWrite(err); got != nil {
		t.Fatalf("verified post-commit write should reconcile as committed, got %v", got)
	}
	ordinary := errors.New("write failed before replace")
	if got := ReconcileCommittedWrite(ordinary); !errors.Is(got, ordinary) {
		t.Fatalf("ordinary write failure must be preserved, got %v", got)
	}
}

func TestAtomicWriteFileCommitOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	var events []string

	err := atomicWriteFileWithOps(path, []byte("new"), 0o600, atomicWriteOps{
		syncTemp: func(file *os.File) error {
			events = append(events, "temp-sync")
			return file.Sync()
		},
		replace: func(oldPath, newPath string) error {
			events = append(events, "replace")
			return os.Rename(oldPath, newPath)
		},
		syncParent: func(string) error {
			events = append(events, "parent-sync")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("atomicWriteFileWithOps failed: %v", err)
	}
	want := []string{"temp-sync", "replace", "parent-sync"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("commit events = %v, want %v", events, want)
	}
	assertAtomicWriteState(t, dir, path, "new", 0)
}

func assertAtomicWriteState(t *testing.T, dir, path, wantContent string, wantTemps int) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != wantContent {
		t.Fatalf("target content = %q, want %q", got, wantContent)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".hexclaw-cfg-*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(temps) != wantTemps {
		t.Fatalf("temporary files = %v, want count %d", temps, wantTemps)
	}
}
