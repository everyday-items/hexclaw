package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	fileutil "github.com/hexagon-codes/toolkit/util/file"
)

const (
	hubCacheLockTimeout = 5 * time.Second
	hubCacheLockRetry   = 10 * time.Millisecond
)

var hubCacheProcessLocks sync.Map

type hubCacheFileLock struct {
	file        *os.File
	processGate chan struct{}
}

// acquireHubCacheFileLock combines a process-local path gate with an OS file
// lock. The local gate normalizes flock semantics across platforms; the OS lock
// coordinates independent CLI/Desktop processes and is released after a crash.
func acquireHubCacheFileLock(ctx context.Context, dir string) (*hubCacheFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, hubCacheLockTimeout)
	defer cancel()
	if err := waitCtx.Err(); err != nil {
		return nil, err
	}
	if err := fileutil.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("create hub cache directory: %w", err)
	}

	lockPath, err := filepath.Abs(filepath.Join(dir, ".hub-catalog.lock"))
	if err != nil {
		return nil, fmt.Errorf("resolve hub cache lock path: %w", err)
	}
	gateValue, _ := hubCacheProcessLocks.LoadOrStore(lockPath, newHubCacheProcessGate())
	gate := gateValue.(chan struct{})
	select {
	case <-gate:
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	}

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		gate <- struct{}{}
		return nil, fmt.Errorf("open hub cache lock file: %w", err)
	}
	ticker := time.NewTicker(hubCacheLockRetry)
	defer ticker.Stop()
	for {
		locked, lockErr := tryLockHubCacheFile(file)
		if lockErr != nil {
			_ = file.Close()
			gate <- struct{}{}
			return nil, fmt.Errorf("lock hub cache file: %w", lockErr)
		}
		if locked {
			return &hubCacheFileLock{file: file, processGate: gate}, nil
		}
		select {
		case <-waitCtx.Done():
			closeErr := file.Close()
			gate <- struct{}{}
			return nil, errors.Join(waitCtx.Err(), closeErr)
		case <-ticker.C:
		}
	}
}

func newHubCacheProcessGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func (l *hubCacheFileLock) Close() error {
	if l == nil {
		return nil
	}
	var unlockErr, closeErr error
	if l.file != nil {
		unlockErr = unlockHubCacheFile(l.file)
		closeErr = l.file.Close()
		l.file = nil
	}
	if l.processGate != nil {
		l.processGate <- struct{}{}
		l.processGate = nil
	}
	return errors.Join(unlockErr, closeErr)
}
