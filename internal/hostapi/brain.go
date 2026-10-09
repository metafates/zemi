// brain.go builds the # Brain section of the context message and the
// path-triggered pointers (DESIGN §10.7).

package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/perm"
)

// brainLoop is the loop text every project's # Brain opens with (DESIGN §10.7);
// it carries the seeding instruction, so it renders where no partition
// exists yet.
const brainLoop = `- The maps below are derived from file frontmatter; bodies are not injected.
  Read a brain file when its Read If matches; zemi also points at one when
  a file you read or edit matches its paths. Consult the brain before you
  build, update it after.
- When you learn the codebase's shape, write or update a private map file
  for it so the next session starts oriented.
- Update what changed: private-partition maps via the memory tool (path and
  content), user-partition files with the file tools (those writes ask),
  one-liners via the memory tool. Keep frontmatter honest; a wrong Read If
  hides the file.
- One-liners cut by the budget leave this list; the memory tool still
  resolves their ids for replaces and forget.
- Standing instructions live in the AGENTS.md chains and the user's own
  AGENTS.md, not here; the user's messages win over every file. Never
  hand-edit memories.md; use the memory tool.`

// brainParts are the host's two brain partitions (DESIGN §7): the private one
// keyed by this repository's TrustRoot, and the user's cross-project one.
func (h *Host) brainParts() (user, private brain.Partition) {
	return brain.UserPartition(h.Dirs.DataHome, h.Dirs.StateHome),
		brain.PrivatePartition(h.Dirs.StateHome, h.trustRoot())
}

// RekeyProject re-derives the trust-root-keyed state: a test harness that
// sets Dirs.ProjectDir after boot needs the private partition to follow.
func (h *Host) RekeyProject() { h.trustRoot = sync.OnceValue(h.Dirs.TrustRoot) }

// reconcileBrain imports a partition's frozen-table rows into memories.md
// (DESIGN §7). Best effort: a failed import is a lint finding and the next
// open retries.
func (h *Host) reconcileBrain(p brain.Partition) {
	if p.Zero() {
		return
	}
	db := h.memoryDB(p.Kind.Scope())
	if db == nil {
		return
	}
	rows, err := db.ListMemory(p.Kind.Scope(), false)
	if err != nil {
		h.Log.Warn("brain reconcile skipped", "origin", "brain", "error", err)
		return
	}
	if err := brain.Reconcile(p, rows, h.now()); err != nil {
		h.Log.Warn("brain reconcile failed", "origin", "brain", "error", err)
	}
}

// brainFor builds the # Brain section and the glob table its pointers match
// (DESIGN §10.7). The user partition loads in every project; the private one's
// catalog reaches a trusted render only, while its memory lines inject in
// every project like the digest they replaced (DESIGN §10.7).
func (h *Host) brainFor() (string, []brainGlob) {
	user, private := h.brainParts()
	parts := make([]brain.Loaded, 0, 2)
	var globs []brainGlob
	if !user.Zero() {
		l := brain.Load(user)
		parts = append(parts, l)
		globs = brainGlobs(l)
	}
	if !private.Zero() {
		l := brain.Load(private)
		if !h.ProjectTrusted() {
			// the lines inject untrusted; the catalog, its scan ceiling and
			// the dir's error name private paths a pull must not see (DESIGN §10.7)
			l.Entries, l.Over, l.DirErr = nil, false, nil
		} else {
			globs = append(globs, brainGlobs(l)...)
		}
		parts = append(parts, l)
	}
	return brain.Render(h.brainLoopText(user, private), parts, time.Now()), globs
}

// brainLoopText is the loop text headed by the partition roots that exist on
// disk, so file-mode paths resolve without guessing (DESIGN §10.7). The roots are
// zemi's own paths, so naming them untrusted leaks nothing.
func (h *Host) brainLoopText(user, private brain.Partition) string {
	var b strings.Builder
	for _, root := range []struct {
		p    brain.Partition
		what string
	}{
		{private, "Private brain for this repository"},
		{user, "User brain, for every project"},
	} {
		if root.p.Zero() {
			continue
		}
		if fi, err := os.Stat(root.p.Dir); err == nil && fi.IsDir() {
			fmt.Fprintf(&b, "%s: %s\n", root.what, root.p.Dir)
		}
	}
	return b.String() + brainLoop
}

// brainGlob is one armed paths: glob (DESIGN §10.7) with the brain file its pointer
// names and the Read If it quotes.
type brainGlob struct {
	match  func(rel string) bool
	name   string
	readIf string
}

// brainGlobs arms a loaded partition's globs. A file that cannot serve -
// unreadable, missing frontmatter, or a bad glob - triggers nothing (DESIGN §10.7).
func brainGlobs(l brain.Loaded) []brainGlob {
	var out []brainGlob
	for _, e := range l.Entries {
		if e.Err != nil || e.FM.Description == "" || e.FM.ReadIf == "" {
			continue
		}
		matchers, err := e.FM.Matchers()
		if err != nil {
			continue
		}
		read := brain.Normalize(e.FM.ReadIf)
		for _, match := range matchers {
			out = append(out, brainGlob{match: match, name: brain.Normalize(e.Name), readIf: read})
		}
	}
	return out
}

// brainPointer is the path-triggered pointer (DESIGN §10.7): the first read of a path
// a brain glob maps earns one line naming the file and its Read If, the
// first write or edit the check-the-map line. At most three attach to one
// result and further matches collapse into one aggregate line, so a result
// cannot earn unbounded text; each fires once per conversation, deduped in
// the map the instruction files use.
func (h *Host) brainPointer(path string, wrote bool) string {
	ag := h.agents
	ag.mu.Lock()
	globs := ag.brainGlobs
	ag.mu.Unlock()
	if len(globs) == 0 {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	// a glob names paths as the model reads them: absolute, or relative to
	// the repository the private partition maps
	forms := []string{abs}
	if root := h.Dirs.RepoRoot(); root != "" {
		if rel, ok := perm.Under(root, abs); ok {
			forms = append(forms, rel)
		}
	}
	ag.readMu.Lock()
	defer ag.readMu.Unlock()
	var lines []string
	more := map[string]bool{}
	for _, g := range globs {
		if !slices.ContainsFunc(forms, g.match) {
			continue
		}
		line := fmt.Sprintf("zemi: you edited a file mapped by %s (Read If: %s); check the map still holds.", g.name, g.readIf)
		if !wrote {
			line = fmt.Sprintf("zemi: %s in the brain maps this path (Read If: %s); read it before you build on this.", g.name, g.readIf)
		}
		if ag.instr[line] {
			continue
		}
		if len(lines) == 3 {
			more[g.name] = true
			continue
		}
		if ag.instr == nil {
			ag.instr = map[string]bool{}
		}
		ag.instr[line] = true
		lines = append(lines, line)
	}
	if len(more) > 0 {
		lines = append(lines, fmt.Sprintf("zemi: %d more brain files map this path; the catalog names them.", len(more)))
	}
	return strings.Join(lines, "\n")
}
