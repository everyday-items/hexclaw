//go:build !windows

package hub

import "os"

func replaceHubCacheFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func syncHubCacheParentDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
