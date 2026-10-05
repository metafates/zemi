package brain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// ScopeBudget caps the bytes one partition's lines take in the prompt (§3):
// the write gate keeps a scope under it, cutting learned lines to the
// ledger, and the render cuts a hand-edited or corrupted file at the same
// line. learn.ScopeBudget's successor; stage 5 deletes that one.
const ScopeBudget = 3072

// lineOverhead is what a rendered line costs beyond its text: the widest
// "- [id origin age] " prefix and the newline, learn/digest.go's measure.
const lineOverhead = len("- [12345678 learned 365d] \n")

// LineSize is the bytes one memory takes in the render, which the budget
// is charged in.
func LineSize(text string) int { return len(text) + lineOverhead }

// Line is one memories.md line: `- [id origin YYYY-MM-DD] text`. The date
// is the line's last write - created for a new line, refreshed and
// re-prepended by an edit - so the rendered age tracks the line (D1).
type Line struct {
	ID     string
	Origin store.Origin
	Date   time.Time
	Text   string
}

// String is the stored form.
func (l Line) String() string {
	return fmt.Sprintf("- [%s %s %s] %s", l.ID, l.Origin, l.Date.Format(time.DateOnly), l.Text)
}

// BadLine is a line that does not parse, at its number in the file:
// dropped from the render, named by lint (D6).
type BadLine struct {
	N    int // 1-based, counting every line of the file
	Text string
}

// ParseMemories reads a memories.md: live lines in file order, newest
// first, and the lines that do not parse.
func ParseMemories(content []byte) (lines []Line, bad []BadLine) {
	for i, ln := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if l, ok := parseLine(ln); ok {
			lines = append(lines, l)
			continue
		}
		bad = append(bad, BadLine{N: i + 1, Text: ln})
	}
	return lines, bad
}

func parseLine(s string) (Line, bool) {
	rest, ok := strings.CutPrefix(s, "- [")
	if !ok {
		return Line{}, false
	}
	id, rest, ok := strings.Cut(rest, " ")
	if !ok || !store.IDShape.MatchString(id) {
		return Line{}, false
	}
	origin, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return Line{}, false
	}
	date, text, ok := strings.Cut(rest, "] ")
	if !ok {
		return Line{}, false
	}
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return Line{}, false
	}
	o := store.Origin(origin)
	if !slices.Contains(store.Origins, o) {
		return Line{}, false
	}
	return Line{ID: id, Origin: o, Date: t, Text: text}, true
}

// serialize is the whole file: one line each, in the order given.
func serialize(lines []Line) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// day is at's date, the granularity the line format stores.
func day(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// textHash is the sha256 a memories.md line op carries (D7): lint matches
// a live line's hash against the entries recorded for its id.
func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ErrNoMemory marks an id neither memories.md nor the ledger holds.
var ErrNoMemory = errors.New("brain: no such memory")

// ErrUserCut marks a budget overflow whose cut set would include an
// origin=user line: refused, nothing written, the caller queues the write
// (D1; DESIGN.md forbids silently dropping a user-attested memory).
var ErrUserCut = errors.New("brain: the cut would take a user memory")

// Add prepends a new line, its add entry in the ledger first, under the
// partition lock (D1).
func Add(p Partition, sid, text, quote string, origin store.Origin, at time.Time) (Line, error) {
	l := Line{ID: store.ShortID(), Origin: origin, Date: day(at), Text: text}
	err := mutate(p, func(lg *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		// a colliding id would merge two memories under one key; the old
		// store refused it at the primary key (store.go's ShortID NOTE)
		for {
			trail, err := lg.ByID(l.ID)
			if err != nil {
				return nil, nil, err
			}
			if len(trail) == 0 && !slices.ContainsFunc(lines, func(x Line) bool { return x.ID == l.ID }) {
				break
			}
			l.ID = store.ShortID()
		}
		op := &Op{
			Op: OpAdd, ID: l.ID, Text: text, Quote: quote, SessionID: sid,
			Hash: textHash(text), Scope: p.Kind.Scope(), Origin: origin, TS: at.UnixMilli(),
		}
		return append([]Line{l}, lines...), []*Op{op}, nil
	})
	return l, err
}

// Edit rewrites a line's text, refreshes its date and re-prepends it
// (/memory edit, the AfterWrite scrub); the edit entry carries the prior
// text and the new hash (D7).
func Edit(p Partition, sid, id, text string, at time.Time) (Line, error) {
	var out Line
	err := mutate(p, func(_ *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
		if i < 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
		}
		prior := lines[i]
		out = Line{ID: id, Origin: prior.Origin, Date: day(at), Text: text}
		op := &Op{
			Op: OpEdit, ID: id, Text: text, PriorText: prior.Text, SessionID: sid,
			Hash: textHash(text), Scope: p.Kind.Scope(), Origin: prior.Origin, TS: at.UnixMilli(),
		}
		return append([]Line{out}, slices.Delete(lines, i, i+1)...), []*Op{op}, nil
	})
	return out, err
}

// Supersede removes the line id, its text and the superseding id trailing
// in the supersede entry (D1); the hot file holds live lines only.
func Supersede(p Partition, sid, id, byID string, at time.Time) error {
	return mutate(p, func(lg *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
		if i < 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
		}
		prior := lines[i]
		op := &Op{
			Op: OpSupersede, ID: id, Text: prior.Text, SupersededBy: byID, SessionID: sid,
			Hash: textHash(prior.Text), Scope: p.Kind.Scope(), Origin: prior.Origin, TS: at.UnixMilli(),
		}
		// the quote trails in the supersede entry (D1); the add or import
		// entry is where it lives
		if trail, err := lg.ByID(id); err == nil {
			for _, o := range trail {
				if o.Quote != "" {
					op.Quote = o.Quote
				}
			}
		}
		return slices.Delete(lines, i, i+1), []*Op{op}, nil
	})
}

// Forget removes the line id and writes its forget entry, so the absence
// always has an explainer (D1). An id the file no longer holds must have a
// ledger trail; one neither holds is ErrNoMemory.
func Forget(p Partition, sid, id string, at time.Time) error {
	return mutate(p, func(l *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		op := &Op{Op: OpForget, ID: id, SessionID: sid, Scope: p.Kind.Scope(), TS: at.UnixMilli()}
		i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
		if i < 0 {
			trail, err := l.ByID(id)
			if err != nil {
				return nil, nil, err
			}
			if len(trail) == 0 {
				return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
			}
			return lines, []*Op{op}, nil
		}
		prior := lines[i]
		op.Text, op.Hash, op.Origin = prior.Text, textHash(prior.Text), prior.Origin
		return slices.Delete(lines, i, i+1), []*Op{op}, nil
	})
}

// Cut archives the partition's oldest learned lines until its lines plus
// extra fit ScopeBudget, one archive entry per line (D1): the tool result
// names the ids, whose text, quote and provenance live on in the ledger.
func Cut(p Partition, sid string, extra int, at time.Time) ([]string, error) {
	var cut []string
	err := mutate(p, func(_ *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		used := extra
		for _, l := range lines {
			used += LineSize(l.Text)
		}
		var ops []*Op
		i := len(lines) // oldest last: lines are newest first
		for used > ScopeBudget && i > 0 {
			i--
			oldest := lines[i]
			if oldest.Origin == store.OriginUser {
				return nil, nil, fmt.Errorf("%w: %s", ErrUserCut, oldest.ID)
			}
			used -= LineSize(oldest.Text)
			cut = append(cut, oldest.ID)
			ops = append(ops, &Op{
				Op: OpArchive, ID: oldest.ID, Text: oldest.Text, SessionID: sid,
				Hash: textHash(oldest.Text), Scope: p.Kind.Scope(), Origin: oldest.Origin, TS: at.UnixMilli(),
			})
		}
		if len(cut) == 0 {
			return lines, nil, nil
		}
		return lines[:i], ops, nil
	})
	return cut, err
}

// Locate resolves an id over memories.md and the ledger (D1): the live
// line when the file holds it, else the id's latest trail.
func Locate(p Partition, l *Ledger, id string) (line Line, live bool, err error) {
	lines, _, err := readMemories(p)
	if err != nil {
		return Line{}, false, err
	}
	if i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id }); i >= 0 {
		return lines[i], true, nil
	}
	if l == nil {
		return Line{}, false, fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	ops, err := l.ByID(id)
	if err != nil {
		return Line{}, false, err
	}
	if len(ops) == 0 {
		return Line{}, false, fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	last := ops[len(ops)-1]
	return Line{ID: id, Origin: last.Origin, Date: day(time.UnixMilli(last.TS)), Text: last.Text}, false, nil
}

// mutate runs change on the partition's memories.md under the partition
// lock, inserting its ledger entries before the atomic replace: ledger
// first, both inside the lock (D1), so a crash between the two leaves the
// D6 divergence finding, never a live line whose provenance is missing.
func mutate(p Partition, change func(l *Ledger, raw []byte, lines []Line) (next []Line, ops []*Op, err error)) error {
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	lines, raw, err := readMemories(p)
	if err != nil {
		return err
	}
	l, err := p.OpenLedger()
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	next, ops, err := change(l, raw, lines)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := l.Insert(op); err != nil {
			return err
		}
	}
	data := serialize(next)
	// a line that does not parse is no op's to own (D6): carried through
	// verbatim, file order kept, so a write never deletes bytes it did not
	// write - dropped from the render, never from the file
	if _, bad := ParseMemories(raw); len(bad) > 0 {
		for _, b := range bad {
			data = append(data, b.Text...)
			data = append(data, '\n')
		}
	}
	if bytes.Equal(data, raw) {
		return nil
	}
	return config.WriteFileAtomic(p.memPath(), data, 0o600)
}

// readMemories reads and parses the partition's memories.md; a missing
// file is an empty store.
func readMemories(p Partition) (lines []Line, raw []byte, err error) {
	raw, err = os.ReadFile(p.memPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	lines, _ = ParseMemories(raw)
	return lines, raw, nil
}
