package builtin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// readRegularFileNoFollow reads exactly the regular file observed by Lstat.
// The post-open identity check closes the Lstat/Open race where an attacker
// swaps the path to a symlink between the two operations.
func readRegularFileNoFollow(path string) ([]byte, os.FileMode, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("refusing symlink file %q", path)
	}
	if !before.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("refusing non-regular file %q", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !os.SameFile(before, after) {
		return nil, 0, fmt.Errorf("file %q changed while opening", path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	return data, before.Mode().Perm(), nil
}

// writeRegularFileAtomicNoFollow publishes data with an atomic rename. It
// refuses an existing symlink/non-regular destination; if the destination is
// swapped after the check, rename replaces the directory entry itself and
// never follows the symlink to its target.
func writeRegularFileAtomicNoFollow(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink file %q", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %q", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".hexclaw-skill-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// promoteRegularFileNoFollow atomically promotes pending to live without ever
// dereferencing pending. It first claims the directory entry under an
// unpredictable same-directory name, validates the claimed entry with Lstat,
// then renames that regular file into place.
func promoteRegularFileNoFollow(pending, live string) error {
	if info, err := os.Lstat(live); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink file %q", live)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %q", live)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	claim, err := os.CreateTemp(filepath.Dir(pending), ".hexclaw-approve-*")
	if err != nil {
		return err
	}
	claimPath := claim.Name()
	if err := claim.Close(); err != nil {
		_ = os.Remove(claimPath)
		return err
	}
	if err := os.Remove(claimPath); err != nil {
		return err
	}
	if err := os.Rename(pending, claimPath); err != nil {
		return err
	}
	claimed := true
	defer func() {
		if !claimed {
			return
		}
		// Restore only when no new pending draft appeared concurrently.
		if _, err := os.Lstat(pending); os.IsNotExist(err) {
			_ = os.Rename(claimPath, pending)
		} else {
			_ = os.Remove(claimPath)
		}
	}()

	info, err := os.Lstat(claimPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink file %q", pending)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular file %q", pending)
	}
	if err := os.Rename(claimPath, live); err != nil {
		return err
	}
	claimed = false
	return nil
}
