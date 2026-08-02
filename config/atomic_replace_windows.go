//go:build windows

package config

import "golang.org/x/sys/windows"

func replaceFile(oldPath, newPath string) error {
	oldPathUTF16, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPathUTF16, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		oldPathUTF16,
		newPathUTF16,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH does not return until the move has
// been flushed to disk. Windows does not expose POSIX directory fsync semantics.
func syncParentDirectory(string) error {
	return nil
}
