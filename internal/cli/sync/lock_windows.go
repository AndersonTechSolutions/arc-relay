//go:build windows

package sync

import (
	"fmt"
	"os"
)

// stateLock on Windows relies on the exclusive-create of the lock file:
// a stale file from a crash must be removed by hand. Good enough for the
// build matrix; the watcher is not deployed on Windows today.
type stateLock struct {
	path string
}

func acquireStateLock(path string) (*stateLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) // #nosec G304 -- own config dir
	if err != nil {
		return nil, fmt.Errorf("another arc-sync memory watcher may hold %s (%v); remove it if stale", path, err)
	}
	_ = f.Close()
	return &stateLock{path: path}, nil
}

func (l *stateLock) release() {
	if l == nil || l.path == "" {
		return
	}
	_ = os.Remove(l.path)
	l.path = ""
}
