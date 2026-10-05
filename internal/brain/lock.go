package brain

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// LockTimeout is how long a writer waits for the partition lock before
// failing with ErrLocked (D1): two windows on one project are a designed
// topology, and the blocked one fails the way ClaimForDelete's UX does.
const LockTimeout = 2 * time.Second

// ErrLocked is the lock another zemi holds.
var ErrLocked = errors.New("another zemi is writing the memory store")

// Lock takes the partition's exclusive memories.lock for the whole of a
// write, waiting up to LockTimeout. The lock is one flock on one fd: it
// survives no process, so a writer that dies releases it.
func Lock(p Partition) (unlock func(), err error) {
	if p.Zero() {
		return nil, errors.New("brain: no partition to lock")
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("brain: create %s: %w", p.Dir, err)
	}
	f, err := os.OpenFile(filepath.Join(p.Dir, "memories.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("brain: open the lock: %w", err)
	}
	deadline := time.Now().Add(LockTimeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, p.Dir)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// LockAll takes several partitions' locks in the design's fixed order,
// user then private, so two cross-partition writers cannot deadlock (D1).
func LockAll(ps ...Partition) (func(), error) {
	order := slices.Clone(ps)
	slices.SortFunc(order, func(a, b Partition) int { return cmp.Compare(a.Kind, b.Kind) })
	var unlocks []func()
	for _, p := range order {
		unlock, err := Lock(p)
		if err != nil {
			for _, done := range unlocks {
				done()
			}
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}, nil
}
