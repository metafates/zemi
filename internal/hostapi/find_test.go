package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// otherStore opens another project's store and writes the root file that
// makes ListProjects find it.
func otherStore(t *testing.T, stateHome string) (string, *store.DB) {
	t.Helper()
	// a real trust root is symlink-resolved, and the store is keyed by it
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := store.ProjectDBPath(stateHome, root)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "root"), []byte(root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, db
}

type findFixture struct {
	dirs    config.Dirs
	plain   string // this project's session
	root    string // the other project
	deep    string // its one session
	titled  string // its other session
	garbage string // a root whose store SQLite cannot read
}

func newFindFixture(t *testing.T) findFixture {
	t.Helper()
	h, _ := newTestHostDB(t)
	f := findFixture{dirs: h.Dirs, plain: store.ShortID()}
	if err := h.DB.CreateSession(f.plain, "", "Postgres tuning", ""); err != nil {
		t.Fatal(err)
	}

	var other *store.DB
	f.root, other = otherStore(t, f.dirs.StateHome)
	f.deep, f.titled = store.ShortID(), store.ShortID()
	for _, s := range []struct{ id, title string }{{f.deep, "Kubernetes rollout"}, {f.titled, "Refactor the parser"}} {
		if err := other.CreateSession(s.id, "", s.title, ""); err != nil {
			t.Fatal(err)
		}
	}

	// never opened, so the file stays what it is: a store no SQLite reads
	f.garbage = t.TempDir()
	dir := filepath.Dir(store.ProjectDBPath(f.dirs.StateHome, f.garbage))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"root": f.garbage + "\n", "zemi.db": "not a database"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// TestListAllSessions: every project's sessions, this project first, and the
// store it cannot read comes back named, not silently dropped.
func TestListAllSessions(t *testing.T) {
	f := newFindFixture(t)
	groups, skipped := ListAllSessions(f.dirs)
	if !slices.Equal(skipped, []string{f.garbage}) {
		t.Errorf("skipped = %v, want the unreadable store %s", skipped, f.garbage)
	}
	if len(groups) != 2 || groups[0].Root != f.dirs.TrustRoot() || groups[1].Root != f.root {
		t.Fatalf("groups = %+v, want this project then %s", groups, f.root)
	}
	ids := func(ps ProjectSessions) []string {
		out := make([]string, len(ps.Sessions))
		for i, s := range ps.Sessions {
			out[i] = s.ID
		}
		return out
	}
	if got := ids(groups[0]); !slices.Equal(got, []string{f.plain}) {
		t.Errorf("this project's sessions = %v, want %s", got, f.plain)
	}
	if got := ids(groups[1]); len(got) != 2 || !slices.Contains(got, f.deep) || !slices.Contains(got, f.titled) {
		t.Errorf("the other project's sessions = %v, want %s and %s", got, f.deep, f.titled)
	}
}
