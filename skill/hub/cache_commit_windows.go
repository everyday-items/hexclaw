//go:build windows

package hub

import "golang.org/x/sys/windows"

func replaceHubCacheFile(oldPath, newPath string) error {
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

// MOVEFILE_WRITE_THROUGH supplies the Windows durability boundary. Windows
// does not expose POSIX parent-directory fsync semantics.
func syncHubCacheParentDirectory(string) error {
	return nil
}
