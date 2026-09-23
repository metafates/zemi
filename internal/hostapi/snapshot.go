// snapshot.go shows what a bash call changed: a git tree of the working
// directory before and after it, and the diff between the two.
package hostapi

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/perm"
)

const (
	snapSlow     = 300 * time.Millisecond
	snapFiles    = 5       // files a row's diff shows
	snapBytes    = 4 << 10 // and the bytes they may take
	snapNoteMax  = 10      // paths the model's line names
	snapOverlap  = "may include changes from concurrent commands"
	snapSecretAs = " (content hidden: secret-shaped name)"
)

type snapshots struct {
	mu   sync.Mutex // one snapshot at a time: they share the warm index
	slow int        // slow snapshots in a row; two turn snapshots off

	state sync.Mutex
	open  int                 // bash calls between their two snapshots
	mixed bool                // two of them overlapped
	wrote map[string][32]byte // what file tools wrote meanwhile, by content
}

// snapshot writes the working tree of dir's repository as a git tree, through
// a temp index of its own, so the user's index and stash are never touched.
// The index stays warm per repository: a later snapshot hashes only what
// changed. "" when there is no repository or snapshots are off.
func (h *Host) snapshot(dir string) (root, tree string) {
	s := &h.snaps
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slow >= 2 {
		return "", ""
	}
	tmp, err := h.TempDir()
	if err != nil {
		return "", ""
	}
	start := time.Now()
	out, err := h.git(dir, "rev-parse", "--show-toplevel", "--git-path", "index")
	root, index, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if err != nil || index == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(root))
	warm := filepath.Join(tmp, fmt.Sprintf("snap-%x.index", sum[:8]))
	env := []string{"GIT_INDEX_FILE=" + warm}
	if _, err := os.Stat(warm); err != nil {
		if !filepath.IsAbs(index) {
			index = filepath.Join(dir, index)
		}
		// a repository with no index yet starts from an empty one
		if data, err := os.ReadFile(index); err == nil {
			os.WriteFile(warm, data, 0o600)
		}
	}
	if _, err = h.gitEnv(root, env, "add", "-A"); err == nil {
		out, err = h.gitEnv(root, env, "write-tree")
	}
	if err != nil {
		h.Log.Warn("snapshot", "snapshot failed", map[string]any{"error": err.Error()})
		return "", ""
	}
	if time.Since(start) < snapSlow {
		s.slow = 0
	} else if s.slow++; s.slow == 2 {
		h.Log.Warn("snapshot", "snapshots are slow in this repository; bash rows show no diff", map[string]any{"ms": time.Since(start).Milliseconds()})
	}
	return root, strings.TrimSpace(out)
}

// bashChanges is the pipeline's Changes hook: it snapshots dir now, and the
// func it returns snapshots again and reports what differs, as one line for
// the model and a diff for the row.
func (h *Host) bashChanges(dir string) func() (note, diff string, paths []string) {
	s := &h.snaps
	s.state.Lock()
	s.open++
	s.mixed = s.mixed || s.open > 1
	s.state.Unlock()
	jobs := h.jobsRunning()
	root, before := h.snapshot(dir)
	return func() (string, string, []string) {
		_, after := h.snapshot(dir)
		s.state.Lock()
		mixed, wrote := s.mixed || jobs || h.jobsRunning(), s.wrote
		if s.open--; s.open == 0 {
			s.mixed, s.wrote = false, nil
		}
		s.state.Unlock()
		if before == "" || after == "" || before == after {
			return "", "", nil
		}
		return h.treeDiff(root, before, after, mixed, wrote)
	}
}

func (h *Host) jobsRunning() bool { return h.jobsRunningCount() > 0 }

func (h *Host) jobsRunningCount() int {
	if h.jobs == nil {
		return 0
	}
	return h.jobs.Running()
}

// noteWrite records a file tool's write while a bash call is open: its row
// shows that change already, so the bash row leaves the path out.
func (h *Host) noteWrite(path string) {
	s := &h.snaps
	s.state.Lock()
	defer s.state.Unlock()
	if s.open == 0 {
		return
	}
	if data, err := os.ReadFile(path); err == nil {
		if s.wrote == nil {
			s.wrote = map[string][32]byte{}
		}
		// the root the paths are joined to is resolved too
		s.wrote[perm.RealPath(path)] = sha256.Sum256(data)
	}
}

func (h *Host) treeDiff(root, before, after string, mixed bool, wrote map[string][32]byte) (note, diff string, changed []string) {
	out, err := h.git(root, "diff", "--name-status", "--no-renames", "-z", before, after)
	if err != nil {
		return "", "", nil
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var paths, shown, lines []string
	for i := 0; i+1 < len(fields); i += 2 {
		abs := filepath.Join(root, fields[i+1])
		if sum, ok := wrote[abs]; ok {
			if data, err := os.ReadFile(abs); err == nil && sha256.Sum256(data) == sum {
				continue
			}
		}
		// NOTE: changed is uncapped; a branch switch in a huge repo marshals every
		// path once per tool.post handler. Cap it here if that shows up.
		paths, changed = append(paths, fields[i+1]), append(changed, abs)
		switch {
		case len(paths) > snapFiles:
		case h.secretPath(abs):
			lines = append(lines, fields[i]+" "+fields[i+1]+snapSecretAs)
		default:
			shown = append(shown, fields[i+1])
			lines = append(lines, fields[i]+" "+fields[i+1])
		}
	}
	if len(paths) == 0 {
		return "", "", nil
	}
	note = "files changed: " + strings.Join(paths[:min(len(paths), snapNoteMax)], ", ")
	if more := len(paths) - snapNoteMax; more > 0 {
		note += fmt.Sprintf(" and %d more", more)
	}
	bodies := map[string][]string{}
	if len(shown) > 0 {
		// no textconv or external diff: a repository's config names those programs
		args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", before, after, "--"}, shown...)
		out, _ = h.gitEnv(root, []string{"GIT_LITERAL_PATHSPECS=1"}, args...)
		// blocks come in path order, like the names above
		for i, block := range strings.Split("\n"+out, "\ndiff --git ")[1:] {
			if i >= len(shown) {
				break
			}
			hunk := false
			for l := range strings.Lines(block) {
				hunk = hunk || strings.HasPrefix(l, "@@")
				if (hunk || strings.HasPrefix(l, "Binary ")) && !strings.HasPrefix(l, `\`) {
					bodies[shown[i]] = append(bodies[shown[i]], strings.TrimRight(l, "\n"))
				}
			}
		}
	}
	var b strings.Builder
	if mixed {
		b.WriteString(snapOverlap + "\n")
	}
	cut := false
	for _, head := range lines {
		b.WriteString(head + "\n")
		_, path, _ := strings.Cut(head, " ")
		for _, l := range bodies[path] {
			if cut = cut || b.Len()+len(l) > snapBytes; cut {
				break
			}
			b.WriteString(l + "\n")
		}
	}
	if cut {
		b.WriteString("... diff cut\n")
	}
	if more := len(paths) - snapFiles; more > 0 {
		fmt.Fprintf(&b, "%d more %s\n", more, plural(more, "file", "files"))
	}
	return note, strings.TrimRight(b.String(), "\n"), changed
}
