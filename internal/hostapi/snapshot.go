// snapshot.go shows what a shell call changed: a git tree of the working
// directory before and after it, and the diff between the two.

package hostapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
)

const (
	// git CPU time, or a wall time load alone does not reach, that makes a
	// snapshot slow
	snapSlow        = 300 * time.Millisecond
	snapStall       = 3 * time.Second
	snapFiles       = 5       // files a row's diff shows
	snapCheckpoints = 100     // files a shell call checkpoints for rewind
	snapBytes       = 4 << 10 // and the bytes they may take
	snapNoteMax     = 10      // paths the model's line names
	snapOverlap     = "may include changes from concurrent commands"
)

type snapshots struct {
	mu    sync.Mutex // one snapshot at a time: they share the warm index
	slow  int        // slow snapshots in a row; two turn snapshots off
	swept bool       // the stores of gone processes are removed

	skipped sync.Map // turn and path a rewind was said not to restore

	state sync.Mutex
	open  int                 // shell calls between their two snapshots
	mixed bool                // two of them overlapped
	wrote map[string][32]byte // what file tools wrote meanwhile, by content
}

// snapshot writes the working tree of dir's repository as a git tree, through
// a temp index and object directory of this process's own, so the user's
// index, stash and object store are never touched. The index stays warm per
// repository: a later snapshot hashes only what changed. env is what reads
// the tree back.
// "" when there is no repository or snapshots are off.
func (h *Host) snapshot(dir string) (root string, env []string, tree string) {
	s := &h.snaps
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slow >= 2 {
		return "", nil, ""
	}
	tmp, err := h.TempDir()
	if err != nil {
		return "", nil, ""
	}
	// CPU time, so a loaded machine does not switch snapshots off, and a
	// long wall bound for a disk git waits on.
	start := time.Now()
	var cpu time.Duration
	git := func(dir string, env []string, args ...string) (string, error) {
		return proc.Git{Dir: dir, Env: env, CPU: &cpu}.Run(context.Background(), args...)
	}
	out, err := git(dir, nil, "rev-parse", "--show-toplevel", "--git-path", "index", "--git-path", "objects")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) != 3 {
		return "", nil, ""
	}
	root, index, objects := lines[0], lines[1], lines[2]
	for _, p := range []*string{&index, &objects} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	sum := sha256.Sum256([]byte(root))
	base := filepath.Join(tmp, fmt.Sprintf("snap-%d-%x", os.Getpid(), sum[:8]))
	warm, objs := base+".index", base+".objects"
	env = []string{"GIT_INDEX_FILE=" + warm, "GIT_OBJECT_DIRECTORY=" + objs, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}
	if !s.swept {
		sweepSnaps(tmp)
		s.swept = true
	}
	if err := os.MkdirAll(objs, 0o700); err != nil {
		return "", nil, ""
	}
	for retry := false; ; retry = true {
		if _, err := os.Stat(warm); err != nil {
			// the user's index warms the first snapshot; without it (no index
			// yet, a failed copy) git starts from an empty one. The copy keeps
			// the index's mtime, by which git rehashes an entry changed in the
			// second the index was written.
			st, err := os.Stat(index)
			data, rerr := os.ReadFile(index)
			if err == nil && rerr == nil && (os.WriteFile(warm, data, 0o600) != nil || os.Chtimes(warm, st.ModTime(), st.ModTime()) != nil) {
				os.Remove(warm)
			}
		}
		if _, err = git(root, env, "add", "-A", "--", "."); err == nil {
			out, err = git(root, env, "write-tree")
		}
		if err == nil || retry {
			break
		}
		// a gc may have pruned a blob the warm index names: start from the user's
		os.Remove(warm)
	}
	if err != nil {
		h.Log.Warn("snapshot failed", "origin", "snapshot", "error", err)
		return "", nil, ""
	}
	if wall := time.Since(start); cpu < snapSlow && wall < snapStall {
		s.slow = 0
	} else if s.slow++; s.slow == 2 {
		h.Log.Warn("snapshots are slow in this repository; shell rows show no diff", "origin", "snapshot", "cpu_ms", cpu.Milliseconds(), "ms", wall.Milliseconds())
	}
	return root, env, strings.TrimSpace(out)
}

// sweepSnaps removes the snapshot stores of processes that are gone, and the
// ones an earlier process with this pid left: nothing prunes their objects.
// NOTE: a dead zemi's store stays while a live process holds its pid.
func sweepSnaps(tmp string) {
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "snap-")
		if !ok {
			continue
		}
		head, _, _ := strings.Cut(rest, "-")
		if pid, err := strconv.Atoi(head); err != nil || pid == os.Getpid() || processGone(pid) {
			os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
}

func processGone(pid int) bool {
	p, err := os.FindProcess(pid)
	return err != nil || errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// shellChanges is the pipeline's Changes hook: it snapshots dir now, and the
// func it returns snapshots again and reports what differs, as one line for
// the model and a diff for the row.
func (h *Host) shellChanges(dir string) func() (note, diff string, paths []string) {
	s := &h.snaps
	s.state.Lock()
	s.open++
	s.mixed = s.mixed || s.open > 1
	s.state.Unlock()
	jobs := h.jobsRunningCount() > 0
	root, env, before := h.snapshot(dir)
	mid := h.turnMsgID()
	return func() (string, string, []string) {
		_, _, after := h.snapshot(dir)
		// a command adopted as a job reports after its turn: its changes
		// belong to no later prompt's rewind
		ckpt := mid != "" && mid == h.turnMsgID()
		s.state.Lock()
		mixed, wrote := s.mixed || jobs || h.jobsRunningCount() > 0, s.wrote
		if s.open--; s.open == 0 {
			s.mixed, s.wrote = false, nil
		}
		s.state.Unlock()
		if before == "" || after == "" || before == after {
			return "", "", nil
		}
		return h.treeDiff(root, env, before, after, mixed, ckpt, wrote)
	}
}

func (h *Host) jobsRunningCount() int {
	if h.jobs == nil {
		return 0
	}
	return h.jobs.Running()
}

// noteWrite records a file tool's write while a shell call is open: its row
// shows that change already, so the shell row leaves the path out.
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

func (h *Host) treeDiff(root string, env []string, before, after string, mixed, ckpt bool, wrote map[string][32]byte) (note, diff string, changed []string) {
	out, err := h.gitEnv(root, env, "diff", "--raw", "--no-renames", "-z", before, after)
	if err != nil {
		return "", "", nil
	}
	// ":<old mode> <new mode> <old id> <new id> <status>", then the path
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var paths, shown, lines []string
	for i := 0; i+1 < len(fields); i += 2 {
		meta, path := strings.Fields(fields[i]), fields[i+1]
		if len(meta) != 5 {
			continue
		}
		status, abs := meta[4], filepath.Join(root, path)
		if sum, ok := wrote[abs]; ok {
			if data, err := os.ReadFile(abs); err == nil && sha256.Sum256(data) == sum {
				continue
			}
		}
		// NOTE: changed is uncapped; a branch switch in a huge repo marshals every
		// path once per tool.post handler. Cap it here if that shows up.
		paths, changed = append(paths, path), append(changed, abs)
		// rewind: what the command deleted or changed, as the before tree
		// holds it (git processes per path, so capped); an ignored file is
		// in no tree and stays as the command left it
		if ckpt && len(changed) <= snapCheckpoints {
			h.checkpointBlob(root, env, before+":"+path, abs, meta[0][1:], meta[1])
		}
		if len(paths) <= snapFiles {
			shown = append(shown, path)
		}
		lines = append(lines, status+" "+path)
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
		out, _ = h.gitEnv(root, append([]string{"GIT_LITERAL_PATHSPECS=1"}, env...), args...)
		// blocks come in path order, like the names above
		for i, block := range strings.Split("\n"+out, "\ndiff --git ")[1:] {
			if i >= len(shown) {
				break
			}
			hunk := false
			for l := range strings.Lines(block) {
				hunk = hunk || strings.HasPrefix(l, "@@")
				binary := strings.HasPrefix(l, "Binary ") // git's "Binary files a/x and b/x differ"
				if binary {
					l = "binary, no text to show"
				}
				if (hunk || binary) && !strings.HasPrefix(l, `\`) {
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
		b.WriteString("… diff cut\n")
	}
	if more := len(paths) - snapFiles; more > 0 {
		fmt.Fprintf(&b, "%d more %s\n", more, Plural(more, "file", "files"))
	}
	return note, strings.TrimRight(b.String(), "\n"), changed
}

// checkpointBlob checkpoints abs as blob (a before-tree path) holds it.
// NOTE: the store keeps no file mode, so a submodule is not restored and a
// deleted executable comes back without its exec bit.
func (h *Host) checkpointBlob(root string, env []string, blob, abs, oldMode, newMode string) {
	switch {
	case oldMode == "160000" || newMode == "160000":
		h.uncheckpointed(abs, "is a submodule")
	case oldMode == "000000":
		h.checkpoint(store.Checkpoint{Path: abs})
	default:
		// the size first: a large file is never read whole
		size, _ := h.gitEnv(root, env, "cat-file", "-s", blob)
		if n, _ := strconv.Atoi(strings.TrimSpace(size)); n > maxCheckpointBytes {
			h.uncheckpointed(abs, "is too large to checkpoint")
		} else if data, err := h.gitEnv(root, env, "cat-file", "-p", blob); err == nil {
			// a link's blob is its target
			h.checkpoint(store.Checkpoint{Path: abs, Existed: true, Link: oldMode == "120000", Content: []byte(data)})
		}
	}
}
