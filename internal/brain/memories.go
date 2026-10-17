package brain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// ScopeBudget caps the bytes one partition's lines take in the prompt (DESIGN §10.7):
// the write gate keeps a scope under it, cutting learned lines to the
// ledger, and the render cuts a hand-edited or corrupted file at the same
// line.
const ScopeBudget = 3072

// lineOverhead is what a rendered line costs beyond its text: the widest
// "- [id origin age] " prefix and the newline.
const lineOverhead = len("- [12345678 learned 365d] \n")

// LineSize is the bytes one memory takes in the render, which the budget
// is charged in.
func LineSize(text string) int { return len(text) + lineOverhead }

// Used is the bytes lines take in the render: the budget every writer, the
// lint and the render itself check.
func Used(lines []Line) int {
	used := 0
	for _, l := range lines {
		used += LineSize(l.Text)
	}
	return used
}

// Line is one memories.md line: `- [id origin YYYY-MM-DD] text`. The date
// is the line's last write - created for a new line, refreshed and
// re-prepended by an edit - so the rendered age tracks the line (DESIGN §7).
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
// dropped from the render, named by lint (DESIGN §10.7).
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

// trailQuote is the last non-empty quote the id's ledger trail carries.
func trailQuote(lg *Ledger, id string) string {
	trail, err := lg.ByID(id)
	if err != nil {
		return ""
	}
	q := ""
	for _, o := range trail {
		if o.Quote != "" {
			q = o.Quote
		}
	}
	return q
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

// Day is at's date, the granularity the line format stores.
func Day(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// TextHash is the sha256 a memories.md line op carries (DESIGN §7): lint matches
// a live line's hash against the entries recorded for its id.
func TextHash(text string) string { return FileHash([]byte(text)) }

// ErrNoMemory marks an id neither memories.md nor the ledger holds.
var ErrNoMemory = errors.New("brain: no such memory")

// ScopeFull is the refusal of a write that would push a scope past
// ScopeBudget, and of a move into one: the scope names itself and the way
// out is /memory (DESIGN §10.4).
func ScopeFull(scope store.Scope) error {
	return fmt.Errorf("%s memory is full; forget one of its oldest in /memory first", scope)
}

// UserCutError says a user-origin write cannot fit its scope: every
// learnable line is already in the ledger and the user-attested ones
// remain over budget. Nothing was written. ID names the oldest user line,
// which the refusal carries; the user is present at every caller and makes
// room in /memory (DESIGN §10.4 forbids silently dropping a user-attested
// memory).
type UserCutError struct{ ID string }

func (e UserCutError) Error() string { return "the cut would take user memory " + e.ID }

// Apply stores m as a memories.md line under the partition lock, the
// ledger entries before the file replace (DESIGN §7): the id kept when m.ID is
// set (a proposal's, so review decisions keep it), else a fresh one; the
// line replaces names moves out first, its supersede entry trailing the
// quote; then the budget cut archives the oldest learned lines, skipping
// user-attested ones, until the file fits ScopeBudget, the ids returned
// for the tool result to name. A learned write that cannot fit even then
// lives in the ledger alone (an add and an archive entry, the hot file
// unchanged); a user-origin one is UserCutError and nothing is written:
// the user is there to make room (DESIGN §10.4).
func Apply(p Partition, m store.Memory, replaces string, at time.Time) (Line, []string, error) {
	if m.Text == "" || m.Origin == "" {
		return Line{}, nil, errors.New("brain: memory needs text and origin")
	}
	l := Line{ID: m.ID, Origin: m.Origin, Date: Day(at), Text: m.Text}
	if l.ID == "" {
		l.ID = store.ShortID()
	}
	var cut []string
	err := mutate(p, func(lg *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		var ops []*Op
		// a colliding id would merge two memories under one key; the old
		// store refused it at the primary key (store.go's ShortID NOTE).
		// Settled before the supersede op is built, so its SupersededBy is
		// this memory's final id
		for {
			trail, err := lg.ByID(l.ID)
			if err != nil {
				return nil, nil, err
			}
			if len(trail) == 0 && !slices.ContainsFunc(lines, func(x Line) bool { return x.ID == l.ID }) {
				break
			}
			if m.ID != "" {
				return nil, nil, fmt.Errorf("brain: memory id %s is already in use", l.ID)
			}
			l.ID = store.ShortID()
		}
		if replaces != "" {
			op, next, err := supersedeOp(lg, lines, m.ProvenanceSessionID, replaces, l.ID, p.Kind.Scope(), at)
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, op)
			lines = next
		}
		ops = append(ops, &Op{
			Op: OpAdd, ID: l.ID, Text: m.Text, Quote: m.Quote, SessionID: m.ProvenanceSessionID,
			Hash: TextHash(m.Text), Scope: p.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
		})
		// budget as promotion (DESIGN §10.4): the oldest learned lines go to the
		// ledger, never a user line; the line just written is its own index
		// zero and never its own cut
		next := append([]Line{l}, lines...)
		used := Used(next)
		from := len(next)
		var blocked string
		for used > ScopeBudget && from > 1 {
			from--
			oldest := next[from]
			if oldest.Origin == store.OriginUser {
				if blocked == "" {
					blocked = oldest.ID
				}
				continue
			}
			used -= LineSize(oldest.Text)
			cut = append(cut, oldest.ID)
			ops = append(ops, &Op{
				Op: OpArchive, ID: oldest.ID, Text: oldest.Text, SessionID: m.ProvenanceSessionID,
				Hash: TextHash(oldest.Text), Scope: p.Kind.Scope(), Origin: oldest.Origin, TS: at.UnixMilli(),
			})
		}
		// the walk skipped user lines below from; they stay in the hot file
		hot := next[:from:from]
		for _, line := range next[from:] {
			if line.Origin == store.OriginUser {
				hot = append(hot, line)
			}
		}
		if used > ScopeBudget {
			if m.Origin == store.OriginUser {
				return nil, nil, UserCutError{ID: blocked}
			}
			// nothing learned left to cut: the trail keeps the line, and the
			// add+archive trail is what a stored-then-cut line already reads as
			ops = append(ops, &Op{
				Op: OpArchive, ID: l.ID, Text: m.Text, Quote: m.Quote, SessionID: m.ProvenanceSessionID,
				Hash: TextHash(m.Text), Scope: p.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
			})
			cut = append(cut, l.ID)
			return hot[1:], ops, nil
		}
		return hot, ops, nil
	})
	if err != nil {
		return Line{}, nil, err
	}
	return l, cut, nil
}

// supersedeOp is the supersede half of a write: the live line out, its text
// and quote trailing in the entry (DESIGN §7); an id only the ledger holds gets
// its entry alone, since the hot file holds live lines only.
func supersedeOp(lg *Ledger, lines []Line, sid, id, byID string, scope store.Scope, at time.Time) (*Op, []Line, error) {
	i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
	if i < 0 {
		trail, err := lg.ByID(id)
		if err != nil {
			return nil, nil, err
		}
		if len(trail) == 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
		}
		return &Op{Op: OpSupersede, ID: id, SupersededBy: byID, SessionID: sid, Scope: scope, TS: at.UnixMilli()}, lines, nil
	}
	prior := lines[i]
	op := &Op{
		Op: OpSupersede, ID: id, Text: prior.Text, SupersededBy: byID, SessionID: sid,
		Hash: TextHash(prior.Text), Scope: scope, Origin: prior.Origin, TS: at.UnixMilli(),
	}
	// the quote trails in the supersede entry (DESIGN §7); the add or import
	// entry is where it lives
	op.Quote = trailQuote(lg, id)
	return op, slices.Delete(lines, i, i+1), nil
}

// promotedInto names the brain file a supersede entry's SupersededBy holds
// when that entry is a promotion's (a replacing id never carries .md; memory
// ids cannot).
func promotedInto(o *Op) (string, bool) {
	if o.Op != OpSupersede || !strings.HasSuffix(o.SupersededBy, ".md") {
		return "", false
	}
	return o.SupersededBy, true
}

// PromotedInto is the brain file id's trail was promoted into whose file
// still exists, "" when none does. The caller holds the partition lock when
// the answer feeds a write.
func PromotedInto(p Partition, id string) string {
	lg, err := p.OpenLedger()
	if err != nil {
		return ""
	}
	defer func() { _ = lg.Close() }()
	trail, err := lg.ByID(id)
	if err != nil {
		return ""
	}
	for _, o := range slices.Backward(trail) {
		if file, ok := promotedInto(o); ok {
			if _, err := readSafe(p, file); err == nil {
				return file
			}
		}
	}
	return ""
}

// CheckPromote is the propose-time pre-flight, nothing written: the same
// checks Promote runs under the partition lock.
func CheckPromote(p Partition, id, name, description, readIf string) error {
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = promoteChecks(p, id, name, description, readIf)
	return err
}

// promoteChecks is Promote's pre-flight, the caller holding the partition
// lock: the target is a servable brain file name, the trail holds the id
// with neither a forget nor a serving promotion on it, the name is free and
// the catalog would render the file. The id's text comes back for the body.
func promoteChecks(p Partition, id, name, description, readIf string) (string, error) {
	if name == MemFile || !strings.HasSuffix(name, ".md") || strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator)) {
		return "", errors.New("the file name is a bare .md name in the brain dir")
	}
	if description == "" || readIf == "" {
		return "", errors.New("a promotion needs description and read_if; the catalog line cannot trigger without them")
	}
	lg, err := p.OpenLedger()
	if err != nil {
		return "", err
	}
	defer func() { _ = lg.Close() }()
	trail, err := lg.ByID(id)
	if err != nil {
		return "", err
	}
	if len(trail) == 0 {
		return "", fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	if trail[len(trail)-1].Op == OpForget {
		return "", fmt.Errorf("%s is forgotten; save it fresh instead", id)
	}
	var text string
	if lines, _, err := readMemories(p); err != nil {
		return "", err
	} else if i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id }); i >= 0 {
		// the live line is the text locateMemory shows, so the body, the
		// queued preview and the undo's bytes-equal guard all agree
		text = lines[i].Text
	}
	for _, o := range trail {
		if file, ok := promotedInto(o); ok {
			// keyed on any such op, so a later replaces of the id cannot mask
			// it; a missing file was a half-done promote, which retries
			if _, err := readSafe(p, file); err == nil {
				return "", fmt.Errorf("%s is already promoted to %s", id, file)
			}
		}
		if text == "" && o.Text != "" {
			// a ledger-only id: an import entry journals the hash, not the
			// text, so the last text-carrying op is where the trail keeps it
			text = o.Text
		}
	}
	if text == "" {
		return "", fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	if _, err := readSafe(p, name); err == nil {
		return "", fmt.Errorf("%s already exists in the %s brain; name another file", name, p.Kind)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if !CatalogAdmits(p, name, description, readIf) {
		return "", fmt.Errorf("the %s brain catalog is full; %s stays put until its brain files make room", p.Kind, id)
	}
	return text, nil
}

// Promote relocates the memory id into the brain file name, a bare .md name
// in the partition: the body is the line's text byte-for-byte, the
// frontmatter the description and read_if given (DESIGN §10.4). The supersede
// entry names the file and the paired write entry records the creation,
// ledger first under the partition lock (DESIGN §7); the id still resolves
// for replaces and forget through its trail. The composed bytes come back
// for the session's write set.
func Promote(p Partition, sid, id, name, description, readIf string, at time.Time) ([]byte, error) {
	unlock, err := Lock(p)
	if err != nil {
		return nil, err
	}
	defer unlock()
	text, err := promoteChecks(p, id, name, description, readIf)
	if err != nil {
		return nil, err
	}
	lines, raw, err := readMemories(p)
	if err != nil {
		return nil, err
	}
	lg, err := p.OpenLedger()
	if err != nil {
		return nil, err
	}
	defer func() { _ = lg.Close() }()
	op, next, err := supersedeOp(lg, lines, sid, id, name, p.Kind.Scope(), at)
	if err != nil {
		return nil, err
	}
	composed := ComposeFile(description, readIf, text)
	for _, o := range []*Op{op, {
		Op: OpWrite, ID: name, SessionID: sid, Scope: p.Kind.Scope(), TS: at.UnixMilli(),
	}} {
		if err := lg.Insert(o); err != nil {
			return nil, err
		}
	}
	if err := writeMemories(p, raw, next); err != nil {
		return nil, err
	}
	return composed, config.WriteFileAtomic(filepath.Join(p.Dir, name), composed)
}

// Supersede removes id's line with a supersede entry naming byID (DESIGN §7), for
// a replacement whose new line lands in the other partition; an id only the
// ledger holds gets its entry alone.
func Supersede(p Partition, sid, id, byID string, at time.Time) error {
	return mutate(p, func(lg *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		op, next, err := supersedeOp(lg, lines, sid, id, byID, p.Kind.Scope(), at)
		return next, []*Op{op}, err
	})
}

// Edit rewrites a line's text, refreshes its date and re-prepends it, the
// edit entry carrying the prior text and the new hash (DESIGN §7); origin is the
// line's new attestation (/memory edits are the user's own words, a
// move-merge takes the mover's).
func Edit(p Partition, sid, id, text string, origin store.Origin, at time.Time) (Line, error) {
	var out Line
	err := mutate(p, func(_ *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		next, op, err := editChange(sid, id, text, origin, p.Kind.Scope(), at, &out, lines)
		return next, []*Op{op}, err
	})
	return out, err
}

func editChange(sid, id, text string, origin store.Origin, scope store.Scope, at time.Time, out *Line, lines []Line) ([]Line, *Op, error) {
	i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
	if i < 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	prior := lines[i]
	*out = Line{ID: id, Origin: origin, Date: Day(at), Text: text}
	op := &Op{
		Op: OpEdit, ID: id, Text: text, PriorText: prior.Text, SessionID: sid,
		Hash: TextHash(text), Scope: scope, Origin: origin, TS: at.UnixMilli(),
	}
	return append([]Line{*out}, slices.Delete(lines, i, i+1)...), op, nil
}

// Forget removes the line id and writes its forget entry, so the absence
// always has an explainer (DESIGN §7). An id the file no longer holds must have a
// ledger trail; one neither holds is ErrNoMemory. A promotion into a brain
// file that still exists refuses: the forget would supersede the trail while
// the file and its catalog line keep injecting (DESIGN §10.4).
func Forget(p Partition, sid, id string, at time.Time) error {
	return mutate(p, func(l *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		trail, err := l.ByID(id)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range trail {
			if file, ok := promotedInto(o); ok {
				if _, err := readSafe(p, file); err == nil {
					return nil, nil, fmt.Errorf("%s is promoted to %s; drop the file", id, file)
				}
			}
		}
		op := &Op{Op: OpForget, ID: id, SessionID: sid, Scope: p.Kind.Scope(), TS: at.UnixMilli()}
		i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id })
		if i < 0 {
			if len(trail) == 0 {
				return nil, nil, fmt.Errorf("%w: %s", ErrNoMemory, id)
			}
			return lines, []*Op{op}, nil
		}
		prior := lines[i]
		op.Text, op.Hash, op.Origin = prior.Text, TextHash(prior.Text), prior.Origin
		return slices.Delete(lines, i, i+1), []*Op{op}, nil
	})
}

// Lines reads a partition's live memories.md lines, newest first (DESIGN §7).
func Lines(p Partition) ([]Line, error) {
	lines, _, err := readMemories(p)
	return lines, err
}

// Restore rewrites memories.md under the partition lock: the lines are
// re-prepended (undo and rewind bring back what their trail removed, ages
// kept), the ids dropped with a forget entry each (DESIGN §7 - every removal has
// an explainer). A line still live is not re-added; an id nothing holds
// still journals its forget.
func Restore(p Partition, sid string, add []Line, forget []string, at time.Time) error {
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	return RestoreLocked(p, sid, add, forget, at)
}

// RestoreLocked is Restore for a caller already holding the partition lock
// (the rewind holds every partition's while it works, DESIGN §7).
func RestoreLocked(p Partition, sid string, add []Line, forget []string, at time.Time) error {
	return mutateLocked(p, func(_ *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
		var ops []*Op
		for _, id := range forget {
			op := &Op{Op: OpForget, ID: id, SessionID: sid, Scope: p.Kind.Scope(), TS: at.UnixMilli()}
			if i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id }); i >= 0 {
				prior := lines[i]
				op.Text, op.Hash, op.Origin = prior.Text, TextHash(prior.Text), prior.Origin
				lines = slices.Delete(lines, i, i+1)
			}
			ops = append(ops, op)
		}
		for _, l := range add {
			if slices.ContainsFunc(lines, func(x Line) bool { return x.ID == l.ID }) {
				continue
			}
			lines = append([]Line{l}, lines...)
		}
		return lines, ops, nil
	})
}

// Move moves id's line to another partition (DESIGN §7): both locks in the fixed
// user-then-private order, the move-out entry in the source's ledger and,
// when no copy of the text lives there, the move-in entry in the
// destination's and the line prepended with its id and age. A copy already
// there makes the move a merge: the copy takes the mover's attestation when
// the mover is the user's (DESIGN §10.4), and its id comes back for pending
// forgets and replacements to follow.
func Move(src, dst Partition, sid, id string, at time.Time) (mergedInto string, err error) {
	if src.Zero() || dst.Zero() {
		return "", errors.New("brain: no partition to move within")
	}
	unlock, err := LockAll(src, dst)
	if err != nil {
		return "", err
	}
	defer unlock()
	srcLines, srcRaw, err := readMemories(src)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(srcLines, func(l Line) bool { return l.ID == id })
	if i < 0 {
		return "", fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	m := srcLines[i]
	dstLines, dstRaw, err := readMemories(dst)
	if err != nil {
		return "", err
	}
	if j := slices.IndexFunc(dstLines, func(l Line) bool { return strings.EqualFold(l.Text, m.Text) }); j >= 0 {
		twin := dstLines[j]
		if m.Origin == store.OriginUser && twin.Origin != store.OriginUser {
			// the copy left behind keeps the user's attestation (DESIGN §10.4)
			var out Line
			if err := mutateLocked(dst, func(_ *Ledger, _ []byte, lines []Line) ([]Line, []*Op, error) {
				next, op, err := editChange(sid, twin.ID, twin.Text, store.OriginUser, dst.Kind.Scope(), at, &out, lines)
				return next, []*Op{op}, err
			}); err != nil {
				return "", err
			}
		}
		moveOut := &Op{
			Op: OpMoveOut, ID: id, Text: m.Text, SupersededBy: twin.ID, SessionID: sid,
			Hash: TextHash(m.Text), Scope: src.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
		}
		if err := removeLine(src, moveOut, srcRaw, srcLines, i); err != nil {
			return "", err
		}
		return twin.ID, nil
	}
	used := Used(dstLines)
	// a move into a full scope is refused: the mover is there to make room
	if used+LineSize(m.Text) > ScopeBudget {
		return "", ScopeFull(dst.Kind.Scope())
	}
	lg, err := src.OpenLedger()
	if err != nil {
		return "", err
	}
	defer func() { _ = lg.Close() }()
	dl, err := dst.OpenLedger()
	if err != nil {
		return "", err
	}
	defer func() { _ = dl.Close() }()
	out := &Op{
		Op: OpMoveOut, ID: id, Text: m.Text, SessionID: sid,
		Hash: TextHash(m.Text), Scope: src.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
	}
	out.Quote = trailQuote(lg, id)
	if err := removeLine(src, out, srcRaw, srcLines, i); err != nil {
		return "", err
	}
	if err := dl.Insert(&Op{
		Op: OpMoveIn, ID: id, Text: m.Text, Quote: out.Quote, SessionID: sid,
		Hash: TextHash(m.Text), Scope: dst.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
	}); err != nil {
		return "", err
	}
	return "", writeMemories(dst, dstRaw, append([]Line{m}, dstLines...))
}

// writeMemories serializes next and replaces the file, a line that does not
// parse carried through verbatim: a write never deletes bytes it did
// not write. The caller holds the partition lock.
func writeMemories(p Partition, raw []byte, next []Line) error {
	data := serialize(next)
	if _, bad := ParseMemories(raw); len(bad) > 0 {
		for _, b := range bad {
			data = append(data, b.Text...)
			data = append(data, '\n')
		}
	}
	if cur, err := readSafe(p, MemFile); err == nil && bytes.Equal(data, cur) {
		return nil
	}
	return config.WriteFileAtomic(p.memPath(), data)
}

// removeLine takes the line at i out of lines and replaces the file with
// the op inserted first (DESIGN §7); both under the caller's held locks.
func removeLine(p Partition, op *Op, raw []byte, lines []Line, i int) error {
	lg, err := p.OpenLedger()
	if err != nil {
		return err
	}
	defer func() { _ = lg.Close() }()
	if err := lg.Insert(op); err != nil {
		return err
	}
	return writeMemories(p, raw, slices.Delete(slices.Clone(lines), i, i+1))
}

// ErrStale is the overwrite gate (DESIGN §10.4): the file's current bytes no longer
// match what the caller saw, so nothing was written and the write queues.
var ErrStale = errors.New("brain: the file changed under the write")

// FileHash is the sha256 the file-mode overwrite gate compares (DESIGN §10.4) and a
// write-set entry records.
func FileHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WriteFile replaces one freeform brain file under the partition lock, its
// write entry in the ledger first (DESIGN §7): the entry carries the replaced
// file's prior bytes, ContentCap-capped with the truncation and existed
// flags, so rewind restores them. want, when given, is the hash the caller
// believes the file holds ("" when it should not exist): a mismatch is
// ErrStale. The accepted-proposal write passes nil - the user approved it.
func WriteFile(p Partition, sid, name string, content []byte, want *string, at time.Time) error {
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(p.Dir, name)
	prior, rerr := readSafe(p, name)
	existed := rerr == nil
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return rerr
	}
	if want != nil {
		now := ""
		if existed {
			now = FileHash(prior)
		}
		if now != *want {
			return ErrStale
		}
	}
	op := &Op{Op: OpWrite, ID: name, SessionID: sid, Scope: p.Kind.Scope(), TS: at.UnixMilli(), Existed: existed}
	if existed {
		op.Prior, op.Truncated = prior, len(prior) > ContentCap
		if op.Truncated {
			op.Prior = prior[:ContentCap]
		}
	}
	lg, err := p.OpenLedger()
	if err != nil {
		return err
	}
	defer func() { _ = lg.Close() }()
	if err := lg.Insert(op); err != nil {
		return err
	}
	return config.WriteFileAtomic(path, content)
}

// Locate resolves an id over memories.md and the ledger (DESIGN §7): the live
// line when the file holds it, else the id's latest trail.
func Locate(p Partition, l *Ledger, id string) (Line, error) {
	lines, _, err := readMemories(p)
	if err != nil {
		return Line{}, err
	}
	if i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == id }); i >= 0 {
		return lines[i], nil
	}
	if l == nil {
		return Line{}, fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	ops, err := l.ByID(id)
	if err != nil {
		return Line{}, err
	}
	if len(ops) == 0 {
		return Line{}, fmt.Errorf("%w: %s", ErrNoMemory, id)
	}
	last := ops[len(ops)-1]
	return Line{ID: id, Origin: last.Origin, Date: Day(time.UnixMilli(last.TS)), Text: last.Text}, nil
}

// mutateLocked runs change under a lock the caller holds (brain.Move's
// merge branch, the reflector's batch): no lock is taken here.
func mutateLocked(p Partition, change func(l *Ledger, raw []byte, lines []Line) ([]Line, []*Op, error)) error {
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
	return writeMemories(p, raw, next)
}

// mutate runs change on the partition's memories.md under the partition
// lock, inserting its ledger entries before the atomic replace: ledger
// first, both inside the lock (DESIGN §7), so a crash between the two leaves the
// divergence finding (DESIGN §10.7), never a live line whose provenance is missing.
func mutate(p Partition, change func(l *Ledger, raw []byte, lines []Line) (next []Line, ops []*Op, err error)) error {
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	return mutateLocked(p, change)
}

// readMemories reads and parses the partition's memories.md; a missing file
// is an empty store, a planted one an error the write paths fail on.
func readMemories(p Partition) (lines []Line, raw []byte, err error) {
	raw, err = readSafe(p, MemFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	lines, _ = ParseMemories(raw)
	return lines, raw, nil
}
