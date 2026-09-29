// projects.go is what zemi knows of other projects: every root it keeps
// state for, from the trust file and the root files of <state>/projects.
package hostapi

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// Project is a directory zemi keeps state for.
type Project struct {
	Root    string    // the trust root, absolute
	Trust   string    // "trusted", "untrusted", or "" undecided
	Missing bool      // Root no longer exists
	Opened  time.Time // when zemi last started there; zero with no root file

	// read with stats: a store zemi cannot read leaves them zero and says why
	Sessions, Memories int
	Latest             *store.Session // the newest top-level session
	StatsErr           error
}

// ListProjects lists every root zemi keeps state for, the last opened first
// and the ones never opened since the root files came last. A root file is
// read only when it hashes to its dir (store.ProjectRoot); a dir with none is
// listed only through a trust decision for its root. With stats it opens
// every store, read-only.
func ListProjects(dirs config.Dirs, stats bool) []Project {
	if dirs.StateHome == "" {
		return nil
	}
	byRoot := map[string]*Project{}
	for root, v := range dirs.ReadTrust() {
		byRoot[root] = &Project{Root: root, Trust: v}
	}
	entries, _ := os.ReadDir(filepath.Join(dirs.StateHome, "projects"))
	for _, e := range entries {
		root, opened, ok := store.ProjectRoot(filepath.Join(dirs.StateHome, "projects", e.Name()))
		if !ok {
			continue
		}
		if byRoot[root] == nil {
			byRoot[root] = &Project{Root: root}
		}
		byRoot[root].Opened = opened
	}
	out := make([]Project, 0, len(byRoot))
	for _, p := range byRoot {
		if _, err := os.Stat(p.Root); errors.Is(err, fs.ErrNotExist) {
			p.Missing = true
		}
		if TrustUnsaved(p.Root) {
			p.Trust = "untrusted"
		}
		if stats {
			p.readStats(dirs.StateHome)
		}
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Project) int {
		return cmp.Or(b.Opened.Compare(a.Opened), cmp.Compare(a.Root, b.Root))
	})
	return out
}

func (p *Project) readStats(stateHome string) {
	db, err := store.OpenRead(store.ProjectDBPath(stateHome, p.Root))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		defer db.Close()
		var latest []*store.Session
		if p.Sessions, p.Memories, err = db.Counts(); err == nil {
			latest, err = db.ListSessions(1)
		}
		if len(latest) > 0 {
			p.Latest = latest[0]
		}
	}
	p.StatsErr = err
}

// TrustWord is the project's trust state as the lists print it.
func (p Project) TrustWord() string {
	switch {
	case p.Missing:
		return "missing"
	case p.Trust == "":
		return "undecided"
	}
	return p.Trust
}
