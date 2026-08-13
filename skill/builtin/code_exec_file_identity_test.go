package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type codeExecSideEffectContext struct {
	context.Context
	trigger int
	calls   int
	effect  func()
}

func (c *codeExecSideEffectContext) Err() error {
	c.calls++
	if c.calls == c.trigger && c.effect != nil {
		c.effect()
	}
	return c.Context.Err()
}

func TestCodeExecOpenedFileSnapshotDetectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "payload.txt")
	writeCodeExecTestFile(t, path, "original")
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := openCodeExecRegularFileNoFollow(root, "payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := snapshotCodeExecOpenedFile(file)
	if err != nil {
		t.Fatal(err)
	}
	mutator, err := root.OpenFile("payload.txt", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := mutator.WriteString("mutated!"); writeErr != nil {
		_ = mutator.Close()
		t.Fatal(writeErr)
	}
	if closeErr := mutator.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if chtimesErr := root.Chtimes("payload.txt", before.Info.ModTime(), before.Info.ModTime()); chtimesErr != nil {
		t.Fatal(chtimesErr)
	}
	after, err := snapshotCodeExecOpenedFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if sameCodeExecOpenedFileSnapshot(before, after) {
		t.Fatal("same-size rewrite with restored mtime retained the opened-file snapshot")
	}
}

func TestCodeExecOpenedFileSnapshotReadsHardLinkCountFromHandle(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "payload.txt")
	writeCodeExecTestFile(t, path, "payload")
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := openCodeExecRegularFileNoFollow(root, "payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := snapshotCodeExecOpenedFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if before.Platform.Links != 1 {
		t.Fatalf("initial handle link count = %d, want 1", before.Platform.Links)
	}
	if linkErr := os.Link(path, filepath.Join(rootPath, "alias.txt")); linkErr != nil {
		t.Skipf("hard links are unavailable: %v", linkErr)
	}
	after, err := snapshotCodeExecOpenedFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if after.Platform.Links != 2 || sameCodeExecOpenedFileSnapshot(before, after) {
		t.Fatalf("opened handle after hard link = %#v, want link count 2 and changed snapshot", after.Platform)
	}
}

func TestCodeExecStageRejectsSameSizeSourceRewriteWithRestoredMtime(t *testing.T) {
	sourcePath := t.TempDir()
	destinationPath := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(sourcePath, "source.txt"), "original")
	sourceRoot, _, err := openCodeExecRootNoFollow(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	destinationRoot, _, err := openCodeExecRootNoFollow(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationRoot.Close()
	before, err := sourceRoot.Lstat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	var mutationErr error
	ctx := &codeExecSideEffectContext{
		Context: context.Background(),
		trigger: 1,
		effect: func() {
			file, effectErr := sourceRoot.OpenFile("source.txt", os.O_WRONLY|os.O_TRUNC, 0)
			if effectErr == nil {
				_, effectErr = file.WriteString("mutated!")
				closeErr := file.Close()
				if effectErr == nil {
					effectErr = closeErr
				}
			}
			if effectErr == nil {
				effectErr = sourceRoot.Chtimes("source.txt", before.ModTime(), before.ModTime())
			}
			mutationErr = effectErr
		},
	}
	err = copyCodeExecStageRegularFile(
		ctx,
		sourceRoot,
		"source.txt",
		before,
		destinationRoot,
		"source.txt",
		&codeExecStageCopyBudget{Max: 1024},
	)
	if mutationErr != nil {
		t.Fatalf("mutate source fixture: %v", mutationErr)
	}
	if err == nil || !strings.Contains(err.Error(), "source file changed while staging") {
		t.Fatalf("same-size source rewrite error = %v, want change-time rejection", err)
	}
}

func TestCodeExecStageRejectsDestinationHashMismatch(t *testing.T) {
	sourcePath := t.TempDir()
	destinationPath := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(sourcePath, "source.txt"), "original")
	sourceRoot, _, err := openCodeExecRootNoFollow(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	destinationRoot, _, err := openCodeExecRootNoFollow(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationRoot.Close()
	before, err := sourceRoot.Lstat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	var mutationErr error
	ctx := &codeExecSideEffectContext{
		Context: context.Background(),
		trigger: 3,
		effect: func() {
			file, effectErr := destinationRoot.OpenFile("source.txt", os.O_WRONLY|os.O_TRUNC, 0)
			if effectErr == nil {
				_, effectErr = file.WriteString("mutated!")
				closeErr := file.Close()
				if effectErr == nil {
					effectErr = closeErr
				}
			}
			mutationErr = effectErr
		},
	}
	err = copyCodeExecStageRegularFile(
		ctx,
		sourceRoot,
		"source.txt",
		before,
		destinationRoot,
		"source.txt",
		&codeExecStageCopyBudget{Max: 1024},
	)
	if mutationErr != nil && !errors.Is(mutationErr, os.ErrNotExist) {
		t.Fatalf("mutate destination fixture: %v", mutationErr)
	}
	if err == nil || !strings.Contains(err.Error(), "hash does not match source") {
		t.Fatalf("destination hash mismatch error = %v", err)
	}
}

func TestCodeExecArtifactRejectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	artifactPath := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(artifactPath, "artifact.txt"), "original")
	root, _, err := openCodeExecRootNoFollow(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Lstat("artifact.txt")
	if err != nil {
		t.Fatal(err)
	}
	var mutationErr error
	ctx := &codeExecSideEffectContext{
		Context: context.Background(),
		trigger: 1,
		effect: func() {
			file, effectErr := root.OpenFile("artifact.txt", os.O_WRONLY|os.O_TRUNC, 0)
			if effectErr == nil {
				_, effectErr = file.WriteString("mutated!")
				closeErr := file.Close()
				if effectErr == nil {
					effectErr = closeErr
				}
			}
			if effectErr == nil {
				effectErr = root.Chtimes("artifact.txt", before.ModTime(), before.ModTime())
			}
			mutationErr = effectErr
		},
	}
	_, _, err = hashCodeExecArtifact(ctx, root, "artifact.txt", before, 1024)
	if mutationErr != nil {
		t.Fatalf("mutate artifact fixture: %v", mutationErr)
	}
	if err == nil || !strings.Contains(err.Error(), "artifact changed while reading") {
		t.Fatalf("same-size artifact rewrite error = %v, want change-time rejection", err)
	}
}
