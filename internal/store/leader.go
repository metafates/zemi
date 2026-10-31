package store

import (
	"database/sql"
	"errors"
	"time"
)

// Leader is the lease row: the one window that may write this project's chat.
type Leader struct {
	Origin string
	Sock   string
	Beat   int64 // unix millis of the holder's last renewal
}

// Stale reports whether the holder stopped renewing past ttl, so a survivor
// may take the lease over.
func (l Leader) Stale(ttl time.Duration) bool {
	return time.Since(time.UnixMilli(l.Beat)) > ttl
}

// ReadLeader returns the lease's holder; ErrNotFound when no window holds it.
func (s *DB) ReadLeader() (Leader, error) {
	var l Leader
	err := s.db.QueryRow(`SELECT origin, sock, beat FROM leader WHERE id = 1`).Scan(&l.Origin, &l.Sock, &l.Beat)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// ClaimLeader takes the lease when the row is absent or still the observed
// one, reporting whether this window won. A row that moved in between — the
// holder renewed, another survivor claimed — loses, so two racers serialize
// under SQLite's write lock and one of them holds the lease.
func (s *DB) ClaimLeader(origin, sock string, observed Leader) (bool, error) {
	return s.changed(`INSERT INTO leader (id, origin, sock, beat) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET origin = excluded.origin, sock = excluded.sock, beat = excluded.beat
		WHERE leader.origin = ? AND leader.beat = ?`,
		origin, sock, now(), observed.Origin, observed.Beat)
}

// RenewLeader stamps the holder's beat, reporting whether this window still
// holds the lease. No freshness clause: a beat that went stale but untaken
// belongs to a holder that is alive (sleep, a stalled tick) and keeps it.
func (s *DB) RenewLeader(origin string) (bool, error) {
	return s.changed(`UPDATE leader SET beat = ? WHERE id = 1 AND origin = ?`, now(), origin)
}

// ReleaseLeader drops the lease on a clean close, so a survivor promotes in
// one tick instead of one TTL; only this window's own row goes.
func (s *DB) ReleaseLeader(origin string) (bool, error) {
	return s.changed(`DELETE FROM leader WHERE id = 1 AND origin = ?`, origin)
}

// ReadLeaderAt reads the lease through a read-only open, for a CLI that must
// wait on no lock a running zemi holds.
func ReadLeaderAt(path string) (Leader, error) {
	db, err := OpenRead(path)
	if err != nil {
		return Leader{}, err
	}
	defer db.Close()
	return db.ReadLeader()
}
