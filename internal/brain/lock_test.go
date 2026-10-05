package brain

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLockContention(t *testing.T) {
	p := testPartition(t)
	openStore(t, p)
	// eight windows write concurrently; the flock serializes them and no
	// line and no ledger entry is lost (D1)
	at := time.Now()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			sid := "s" + strconv.Itoa(i)
			if _, err := Add(p, sid, "from "+sid, "", "learned", at); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	lines, _, err := readMemories(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 8 {
		t.Fatalf("lines = %+v", lines)
	}
	texts := map[string]bool{}
	for _, l := range lines {
		texts[l.Text] = true
	}
	if len(texts) != 8 {
		t.Fatalf("texts = %v", texts)
	}
	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	ops, err := lg.ops()
	if err != nil {
		t.Fatal(err)
	}
	adds := map[string]bool{}
	for _, op := range ops {
		if op.Op == OpAdd {
			adds[op.ID] = true
		}
	}
	if len(adds) != 8 {
		t.Fatalf("add entries = %v", adds)
	}
}

func TestLockTimesOut(t *testing.T) {
	p := testPartition(t)
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Lock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v", err)
	}
	// the lock outlives no process: release hands it to the next waiter
	unlock()
	unlock2, err := Lock(p)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestLockOrderIsUserThenPrivate(t *testing.T) {
	user, private := testPartitions(t)
	// a holder of the user lock alone blocks a dual lock, which must not
	// have taken the private one yet (D1's fixed order)
	unlock, err := Lock(user)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan func(), 1)
	go func() {
		unlock2, err := LockAll(private, user) // argument order must not matter
		if err != nil {
			done <- nil
			return
		}
		done <- unlock2
	}()
	// while the dual lock waits for the user lock, the private lock is free
	free, err := Lock(private)
	if err != nil {
		t.Fatalf("private lock taken while user was still held: %v", err)
	}
	free()
	unlock()
	unlock2, ok := <-done
	if !ok || unlock2 == nil {
		t.Fatal("dual lock failed")
	}
	defer unlock2()
	// both are held now: another dual lock waits and fails
	if _, err := LockAll(user, private); !errors.Is(err, ErrLocked) {
		t.Fatalf("second dual lock = %v", err)
	}
}

func TestLockCreatesThePartition(t *testing.T) {
	p := testPartition(t)
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fi, err := os.Stat(filepath.Join(p.Dir, "memories.lock"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("lock file: %v", err)
	}
	if _, err := Lock(Partition{Kind: Private}); err == nil || !strings.Contains(err.Error(), "no partition") {
		t.Fatalf("zero partition: %v", err)
	}
}
