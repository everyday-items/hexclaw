package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileAccessSkills_AuthorizedDirectoryCanListAndRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello\nworld\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	broker := NewFileAccessBroker([]string{root})
	listSkill := NewListDirectorySkill(broker)
	readSkill := NewReadFileSkill(broker)

	listed, err := listSkill.Execute(context.Background(), map[string]any{"path": root})
	if err != nil {
		t.Fatalf("list authorized dir: %v", err)
	}
	if !strings.Contains(listed.Content, "[FILE] notes.txt") {
		t.Fatalf("list output missing file: %q", listed.Content)
	}

	read, err := readSkill.Execute(context.Background(), map[string]any{"path": filepath.Join(root, "notes.txt")})
	if err != nil {
		t.Fatalf("read authorized file: %v", err)
	}
	if read.Content != "hello\nworld\n" {
		t.Fatalf("read content = %q", read.Content)
	}
}

func TestFileAccessSkills_RejectUnauthorizedPath(t *testing.T) {
	allowed := t.TempDir()
	denied := t.TempDir()
	if err := os.WriteFile(filepath.Join(denied, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	readSkill := NewReadFileSkill(NewFileAccessBroker([]string{allowed}))
	_, err := readSkill.Execute(context.Background(), map[string]any{"path": filepath.Join(denied, "secret.txt")})
	if err == nil {
		t.Fatal("unauthorized path must be rejected")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied error, got: %v", err)
	}
}

func TestFileAccessSkills_RejectSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	link := filepath.Join(allowed, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	readSkill := NewReadFileSkill(NewFileAccessBroker([]string{allowed}))
	_, err := readSkill.Execute(context.Background(), map[string]any{"path": link})
	if err == nil {
		t.Fatal("symlink escape must be rejected")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied error, got: %v", err)
	}
}

func TestListAllowedDirectoriesSkill_ReturnsHexClawGrantStore(t *testing.T) {
	root := t.TempDir()
	s := NewListAllowedDirectoriesSkill(NewFileAccessBroker([]string{root, root + string(filepath.Separator)}))

	result, err := s.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("list allowed dirs: %v", err)
	}
	if got := strings.Count(result.Content, root); got != 1 {
		t.Fatalf("allowed dirs should be canonicalized and de-duplicated, got %d occurrences in %q", got, result.Content)
	}
}
