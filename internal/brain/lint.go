package brain

import (
	"fmt"

	"github.com/metafates/zemi/internal/learn"
)

// Finding is one lint result (D6): single-line, control-character-free,
// naming the path and the problem and nothing more. Kind is the partition
// it names, for the untrusted filter (D3): an untrusted project hears
// only user-partition findings.
type Finding struct {
	Kind Kind
	Text string
}

// Lint checks one partition against D6's findings: the freeform files'
// frontmatter, the memories.md lines, and the file-versus-ledger
// divergence. An absent partition is clean; a ledger that will not open
// costs no findings, since the render already degrades without it.
func Lint(p Partition) []Finding {
	var out []Finding
	add := func(format string, args ...any) {
		out = append(out, Finding{Kind: p.Kind, Text: strip(fmt.Sprintf(format, args...))})
	}
	l := Load(p)
	if l.DirErr != nil {
		add("%s: %s", p.Dir, errLine(l.DirErr))
	}
	for _, e := range l.Entries {
		switch {
		case e.Err != nil:
			add("%s: %s", e.Name, errLine(e.Err))
		case e.FM.Description == "":
			add("%s: no description", e.Name)
		case e.FM.ReadIf == "":
			add("%s: no read_if", e.Name)
		}
		if err := e.FM.BadGlob(); err != nil {
			add("%s: %s", e.Name, err)
		}
		if n := len([]rune(e.FM.Description)); n > FieldCap {
			add("%s: description is %d characters, over the %d the render caps", e.Name, n, FieldCap)
		}
		if n := len([]rune(e.FM.ReadIf)); n > FieldCap {
			add("%s: read_if is %d characters, over the %d the render caps", e.Name, n, FieldCap)
		}
	}
	if l.Over {
		add("%s: catalog scan past its ceiling (%d files, %d bytes of frontmatter)", p.Dir, MaxCatalogFiles, MaxFrontBytes)
	}
	lines, bad := ParseMemories(l.Mem)
	if l.MemErr != nil {
		add("%s: %s", MemFile, l.MemErr)
	}
	for _, b := range bad {
		add("%s: line %d is not a memory line", MemFile, b.N)
	}
	used := 0
	for _, ln := range lines {
		used += LineSize(ln.Text)
	}
	if used > ScopeBudget {
		add("%s: memory lines render past the %d-byte budget", p.memPath(), ScopeBudget)
	}
	for _, ln := range lines {
		if learn.InstructionShaped(ln.Text) {
			add("memory %s reads like an instruction (detection only; the line still injects)", ln.ID)
		}
	}
	lg, err := p.OpenLedger()
	if err != nil {
		add("%s: %s", p.DB, errLine(err))
		return out
	}
	defer func() { _ = lg.Close() }()
	ops, err := lg.ops()
	if err != nil {
		add("%s: %s", p.DB, errLine(err))
		return out
	}
	lintLedger(ops, lines, add)
	return out
}

// lintLedger replays the ops oldest first (D6): an add or import whose id
// is missing from the file with no later removal explaining it is the
// divergence - a crash between the ledger insert and the file replace, a
// lost update between windows, or a hand edit - and a live line whose text
// hash matches no entry for its id is an unjournaled rewrite.
func lintLedger(ops []*Op, lines []Line, add func(string, ...any)) {
	type trail struct {
		added     bool
		explained bool
		hashes    map[string]bool
	}
	t := map[string]*trail{}
	var ids []string
	for _, op := range ops {
		d := t[op.ID]
		if d == nil {
			d = &trail{hashes: map[string]bool{}}
			t[op.ID] = d
			ids = append(ids, op.ID)
		}
		switch op.Op {
		case OpAdd, OpImport, OpMoveIn:
			d.added, d.explained = true, false
		case OpSupersede, OpForget, OpArchive, OpMoveOut:
			d.explained = true
		}
		if op.Hash != "" {
			d.hashes[op.Hash] = true
		}
	}
	live := map[string]bool{}
	flagged := map[string]bool{}
	for _, ln := range lines {
		live[ln.ID] = true
		if flagged[ln.ID] {
			continue
		}
		if d := t[ln.ID]; d == nil || !d.hashes[textHash(ln.Text)] {
			add("memory %s was rewritten outside the ledger (a hand edit)", ln.ID)
			flagged[ln.ID] = true
		}
	}
	for _, id := range ids {
		if d := t[id]; d.added && !d.explained && !live[id] {
			add("memory %s is in the ledger, gone from %s (a crash between the ledger and the file, a lost update, or a hand edit)", id, MemFile)
		}
	}
}
