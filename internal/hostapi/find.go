// find.go reads sessions across projects: `sessions -a`, the `-r` re-root and
// the project walks share the roots it lists. A store zemi cannot read is
// skipped and named, never fatal, since the answer is a session, not an error.

package hostapi

import (
	"errors"
	"io/fs"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// ProjectSessions is one project's top-level sessions, last used first.
type ProjectSessions struct {
	Root     string
	Sessions []*store.Session
}

// projectRoots are the stores a read of every project opens: this project
// first, then the others last used first. The current root is added even when
// no root file names it, since its store can exist without one.
func projectRoots(dirs config.Dirs) []string {
	root := dirs.TrustRoot()
	roots := []string{root}
	for _, p := range ListProjects(dirs, false) {
		if p.Root != root {
			roots = append(roots, p.Root)
		}
	}
	return roots
}

// SessionProject is the project root whose store holds session id: the
// current one first, then the others last used first, false for none. A
// store that cannot be read is skipped.
func SessionProject(dirs config.Dirs, id string) (root string, ok bool) {
	if id == "" {
		return "", false
	}
	for _, r := range projectRoots(dirs) {
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, r))
		if err != nil {
			continue
		}
		_, err = db.GetSession(id)
		db.Close()
		if err == nil {
			return r, true
		}
	}
	return "", false
}

// ListAllSessions groups every project's top-level sessions, last used first.
// A store it cannot read is left out and returned named, not silently
// dropped: the list alone would read as "no sessions" over a store that has
// them.
func ListAllSessions(dirs config.Dirs) (groups []ProjectSessions, skipped []string) {
	for _, root := range projectRoots(dirs) {
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, root))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			skipped = append(skipped, root)
			continue
		}
		list, err := db.ListSessions(-1)
		db.Close()
		if err != nil {
			skipped = append(skipped, root)
			continue
		}
		if len(list) == 0 {
			continue
		}
		groups = append(groups, ProjectSessions{Root: root, Sessions: list})
	}
	return groups, skipped
}
