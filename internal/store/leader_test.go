package store

import (
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// plantBeat ages the holder's beat, so a test plays a stale lease without
// waiting out the TTL, and returns the row a survivor would read.
func plantBeat(t *testing.T, db *DB, at time.Time) Leader {
	t.Helper()
	if _, err := db.db.Exec(`UPDATE leader SET beat = ? WHERE id = 1`, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReadLeader()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestLeaderLease: the lease is one CAS row — the first claim wins, a
// takeover swaps the exact row its reader saw, the holder renews a stale row
// back to life, and a busy store is an error, never a silent loss. The cases
// run in order: each one is the row the last one left.
func TestLeaderLease(t *testing.T) {
	db := openTest(t)

	won, err := db.ClaimLeader("a", "a.sock", Leader{})
	if err != nil || !won {
		t.Fatalf("claim on the absent row: won=%v err=%v", won, err)
	}
	stale := plantBeat(t, db, time.Now().Add(-time.Minute))

	claims := []struct {
		name     string
		who      string
		observed Leader
		want     bool
	}{
		{"a boot that saw no row loses to the holder", "b", Leader{}, false},
		{"a takeover of the row its reader saw wins", "b", stale, true},
		{"a takeover of a row that moved loses", "c", stale, false},
	}
	for _, c := range claims {
		t.Run(c.name, func(t *testing.T) {
			won, err := db.ClaimLeader(c.who, c.who+".sock", c.observed)
			if err != nil {
				t.Fatal(err)
			}
			if won != c.want {
				t.Errorf("ClaimLeader(%q) = %v, want %v", c.who, won, c.want)
			}
		})
	}

	if ok, err := db.RenewLeader("b"); err != nil || !ok {
		t.Errorf("the holder renews: ok=%v err=%v", ok, err)
	}
	if ok, err := db.RenewLeader("a"); err != nil || ok {
		t.Errorf("a stranger's renew: ok=%v err=%v", ok, err)
	}
	if l := plantBeat(t, db, time.Now().Add(-time.Hour)); !l.Stale(time.Minute) {
		t.Fatalf("a planted hour-old beat reads fresh")
	}
	if ok, err := db.RenewLeader("b"); err != nil || !ok {
		t.Errorf("the holder renews its stale-but-untaken lease back to life: ok=%v err=%v", ok, err)
	}

	if ok, err := db.ReleaseLeader("a"); err != nil || ok {
		t.Errorf("a stranger's release: ok=%v err=%v", ok, err)
	}
	if ok, err := db.ReleaseLeader("b"); err != nil || !ok {
		t.Errorf("the holder releases: ok=%v err=%v", ok, err)
	}
	if _, err := db.ReadLeader(); !errors.Is(err, ErrNotFound) {
		t.Errorf("the released lease reads %v, want ErrNotFound", err)
	}
	if won, err := db.ClaimLeader("c", "c.sock", Leader{}); err != nil || !won {
		t.Errorf("a claim over the released row: won=%v err=%v", won, err)
	}

	l, err := ReadLeaderAt(db.path)
	if err != nil || l.Origin != "c" || l.Sock != "c.sock" {
		t.Errorf("ReadLeaderAt = %+v, %v; want the holder c", l, err)
	}
	if _, err := ReadLeaderAt(filepath.Join(t.TempDir(), "gone.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadLeaderAt of a missing store = %v, want fs.ErrNotExist", err)
	}

	// a held write lock makes a renew an error, never a silent loss
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", db.path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, err := (&DB{db: other, path: db.path}).RenewLeader("c"); err == nil {
		t.Errorf("a renew behind a held write lock: ok=%v err=%v, want an error", ok, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RenewLeader("c"); err != nil || !ok {
		t.Errorf("the renew works once the lock lifts: ok=%v err=%v", ok, err)
	}
}
