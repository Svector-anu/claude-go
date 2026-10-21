//go:build unix

package fsx

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrLockTimeout means another process held the lock for the whole wait.
var ErrLockTimeout = errors.New("timed out waiting for lock")

const lockPollInterval = 20 * time.Millisecond

// Lock is an exclusive advisory lock on a file. The kernel drops it if the
// holder crashes, so a stale lock cannot wedge later runs.
type Lock struct{ f *os.File }

// AcquireLock takes an exclusive flock on path (created 0600), waiting up to timeout.
func AcquireLock(path string, timeout time.Duration) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, path)
		}
		time.Sleep(lockPollInterval)
	}
}

// Release drops the lock. Closing the descriptor releases the flock.
func (l *Lock) Release() error {
	return l.f.Close()
}
