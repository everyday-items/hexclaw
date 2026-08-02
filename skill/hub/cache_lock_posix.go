//go:build !windows

package hub

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockHubCacheFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlockHubCacheFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
