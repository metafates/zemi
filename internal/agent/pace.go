// pace.go is the cross-process half of a rate limiter: one bucket state per
// (api key, limiter key) under <state>/pace, shared by every zemi process of
// the user, so N windows pace the provider's whole quota together instead of
// each assuming it. The file holds the buckets' tokens and the refusal hold;
// every access takes an exclusive flock on a lock twin, so a reader never
// sees a half-written state. A state nothing can read starts full again and
// a result that cannot be written is dropped: the pace is best effort, and
// the gateway's refusals - and the hold they arm - stay the backstop.

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/metafates/zemi/internal/filelock"
)

// paceFile is one shared bucket: <dir>/<hash>.json renamed atomically while
// <dir>/<hash>.lock is held, so a crashed writer leaves no half state.
type paceFile struct {
	path string
	lock string
}

func newPaceFile(dir, apiKey, key string) *paceFile {
	sum := sha256.Sum256([]byte(apiKey + "\x00" + key))
	p := filepath.Join(dir, hex.EncodeToString(sum[:12]))
	_ = os.MkdirAll(dir, 0o700)
	return &paceFile{path: p, lock: p + ".lock"}
}

// paceDTO is the file's shape: the requests metronome's last admission, the
// token bucket's state, and the refusal hold's deadline. A process without a
// token quota leaves the file's token state as it found it, since its own
// charges record nothing.
type paceDTO struct {
	RL   int64   `json:"rl"`
	TT   float64 `json:"tt,omitempty"`
	TL   int64   `json:"tl,omitempty"`
	Hold int64   `json:"hold,omitempty"`
}

// _paceLockWait bounds one update's wait for the file's lock: the holders
// keep it for microseconds, so a wait this long means a machine in trouble,
// and the call paces per process rather than stall.
const _paceLockWait = 5 * time.Second

// update runs fn on the shared state under the file's lock.
func (p *paceFile) update(l *rateLimiter, fn func(st *paceState)) {
	st := l.freshState()
	unlock, err := filelock.Lock(p.lock, _paceLockWait)
	if err != nil {
		fn(st) // unlocked and dropped: fail open
		return
	}
	defer unlock()
	var dto paceDTO
	if raw, err := os.ReadFile(p.path); err == nil && json.Unmarshal(raw, &dto) == nil {
		st.last = time.Unix(0, dto.RL)
		if st.toks != nil && dto.TL != 0 {
			st.toks.tokens, st.toks.last = dto.TT, time.Unix(0, dto.TL)
		}
		if dto.Hold != 0 {
			st.hold = time.Unix(0, dto.Hold)
		}
	}
	fn(st)
	out := paceDTO{RL: st.last.UnixNano(), TT: dto.TT, TL: dto.TL}
	if st.toks != nil {
		out.TT, out.TL = st.toks.tokens, st.toks.last.UnixNano()
	}
	if !st.hold.IsZero() {
		out.Hold = st.hold.UnixNano()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return
	}
	if os.WriteFile(p.path+".tmp", raw, 0o600) == nil {
		_ = os.Rename(p.path+".tmp", p.path)
	}
}
