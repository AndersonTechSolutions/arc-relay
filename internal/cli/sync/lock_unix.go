//go:build !windows

package sync

import (
	"fmt"
	"os"
	"syscall"
)

// stateLock is an advisory single-writer lock on the watcher state. Two
// watchers advancing the same watermarks would replay or skip transcript
// bytes, so a second `watch`/`--once` must fail fast instead of racing.
type stateLock struct {
	f *os.File
}

func acquireStateLock(path string) (*stateLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path is our own config dir
	if err != nil {
		return nil, fmt.Errorf("open state lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another arc-sync memory watcher holds %s (%v); stop it or use a different config dir", path, err)
	}
	return &stateLock{f: f}, nil
}

func (l *stateLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
