package brain

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/metafates/zemi/internal/filelock"
)

// LockTimeout is how long a writer waits for the partition lock before
// failing with ErrLocked (DESIGN §7): two windows on one project are a designed
// topology, and the blocked one fails the way ClaimForDelete's UX does.
const LockTimeout = 2 * time.Second

// ErrLocked is the lock another zemi holds.
var ErrLocked = errors.New("another zemi is writing the memory store")

// Lock takes the partition's exclusive memories.lock for the whole of a
// write, waiting up to LockTimeout. The lock is held on the open fd: it
// survives no process, so a writer that dies releases it.
func Lock(p Partition) (unlock func(), err error) {
	if p.Zero() {
		return nil, errors.New("brain: no partition to lock")
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("brain: create %s: %w", p.Dir, err)
	}
	unlock, err = filelock.Lock(filepath.Join(p.Dir, "memories.lock"), LockTimeout)
	switch {
	case errors.Is(err, filelock.ErrLocked):
		return nil, fmt.Errorf("%w: %s", ErrLocked, p.Dir)
	case err != nil:
		return nil, fmt.Errorf("brain: lock: %w", err)
	}
	return unlock, nil
}

// LockAll takes several partitions' locks in the design's fixed order,
// user then private, so two cross-partition writers cannot deadlock (DESIGN §7);
// a partition with no home is nothing to lock.
func LockAll(ps ...Partition) (func(), error) {
	order := slices.Clone(ps)
	slices.SortFunc(order, func(a, b Partition) int { return cmp.Compare(a.Kind, b.Kind) })
	var unlocks []func()
	for _, p := range order {
		if p.Zero() {
			continue
		}
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
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}, nil
}
