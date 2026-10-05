// brain.go builds the # Brain section of the context message and the
// path-triggered pointers (docs/brain-design.md sections 3-4 and D10).

package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// brainLoop is the loop text every project's # Brain opens with (section 4);
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

// brainParts are the host's two brain partitions (D2): the private one
// keyed by this repository's TrustRoot, and the user's cross-project one.
func (h *Host) brainParts() (user, private brain.Partition) {
	return brain.UserPartition(h.Dirs.DataHome, h.Dirs.StateHome),
		brain.PrivatePartition(h.Dirs.StateHome, h.trustRoot())
}

// brainFor builds the # Brain section and the glob table its pointers match
// (D10). The user partition loads and lints in every project; the private
// one's catalog and findings reach a trusted render only, while its memory
// lines inject in every project like the digest they still are (D3). The
// lines come from the store rows; stage 3 repoints them at memories.md.
func (h *Host) brainFor() (string, []brainGlob) {
	user, private := h.brainParts()
	rows, _ := h.memoryList(false)
	parts := make([]brain.Loaded, 0, 2)
	var findings []brain.Finding
	var globs []brainGlob
	if !user.Zero() {
		l := brain.Load(user)
		l.Mem = memLines(rows, store.ScopeUser)
		parts = append(parts, l)
		findings = append(findings, brain.Lint(user)...)
		globs = brainGlobs(l)
	}
	if !private.Zero() && h.ProjectTrusted() {
		l := brain.Load(private)
		l.Mem = memLines(rows, store.ScopeProject)
		parts = append(parts, l)
		findings = append(findings, brain.Lint(private)...)
		globs = append(globs, brainGlobs(l)...)
	} else {
		parts = append(parts, brain.Loaded{Partition: private, Mem: memLines(rows, store.ScopeProject)})
	}
	return brain.Render(h.brainLoopText(user, private), parts, findings, time.Now()), globs
}

// memLines renders one scope's rows as memories.md bytes, newest first as
// the store listed them.
func memLines(rows []*store.Memory, scope store.Scope) []byte {
	var b strings.Builder
	for _, m := range rows {
		if m.Scope != scope {
			continue
		}
		l := brain.Line{ID: m.ID, Origin: m.Origin, Date: time.UnixMilli(m.UpdatedAt), Text: m.Text}
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// brainLoopText is the loop text headed by the partition roots that exist on
// disk, so file-mode paths resolve without guessing (D10). The roots are
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

// brainGlob is one armed paths: glob (D10) with the brain file its pointer
// names and the Read If it quotes.
type brainGlob struct {
	pattern string
	file    string // the brain file's path in its partition
	name    string
	readIf  string
}

// brainGlobs arms a loaded partition's globs. A file that cannot serve -
// unreadable, missing frontmatter, or a bad glob - triggers nothing (D6).
func brainGlobs(l brain.Loaded) []brainGlob {
	var out []brainGlob
	for _, e := range l.Entries {
		if e.Err != nil || e.FM.Description == "" || e.FM.ReadIf == "" || e.FM.BadGlob() != nil {
			continue
		}
		read := brain.Normalize(e.FM.ReadIf)
		for _, pattern := range e.FM.Paths {
			out = append(out, brainGlob{
				pattern: pattern,
				file:    filepath.Join(l.Partition.Dir, e.Name),
				name:    e.Name,
				readIf:  read,
			})
		}
	}
	return out
}

// brainPointer is the path-triggered pointer (D10): the first read of a path
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
	extra := 0
	for _, g := range globs {
		if !matchAny(g.pattern, forms) {
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
			extra++
			continue
		}
		if ag.instr == nil {
			ag.instr = map[string]bool{}
		}
		ag.instr[line] = true
		lines = append(lines, line)
	}
	if extra > 0 {
		lines = append(lines, fmt.Sprintf("zemi: %d more brain files map this path; the catalog names them.", extra))
	}
	return strings.Join(lines, "\n")
}

func matchAny(pattern string, forms []string) bool {
	for _, form := range forms {
		if ok, err := filepath.Match(pattern, form); err == nil && ok {
			return true
		}
	}
	return false
}
