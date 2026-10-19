// Package filelock takes an exclusive advisory lock on a file, shared by
// every process and handle that opens it.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked is the lock another process or handle holds.
var ErrLocked = errors.New("file is locked elsewhere")

// Lock takes path's exclusive lock, waiting up to wait for a holder to
// release it. The lock lives on the open fd, so a process that dies
// releases it.
func Lock(path string, wait time.Duration) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = lockFile(f)
		if err == nil {
			return func() {
				_ = unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if time.Until(deadline) <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
