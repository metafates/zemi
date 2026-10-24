package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// TestDB runs queries through execute against a seeded project store, its
// global store, a missing one and an unmigrated one.
func TestDB(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	sid := store.ShortID()
	db, err := store.Open(store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot()))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(sid, "", "fix the failing test", "default"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	global, err := store.Open(filepath.Join(dirs.StateHome, "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	global.Close()
	// an empty project: -C needs the dir to exist, the store must not
	empty := filepath.Join(home, "empty", "repo")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	// a store the migrations never ran on
	stale := filepath.Join(home, "stale", "repo")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	// the store path keys on the symlink-resolved root, as TrustRoot does
	resolve := func(dir string) string {
		r, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	emptyStore := store.ProjectDBPath(dirs.StateHome, resolve(empty))
	staleDB := store.ProjectDBPath(dirs.StateHome, resolve(stale))
	if err := os.MkdirAll(filepath.Dir(staleDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
	}{
		{"args join into one query", []string{"db", "select", "id,", "title", "from", "sessions"}, 0, sid + "  fix the failing test\n", ""},
		{"json is one object per row", []string{"db", "--json", "select id, title from sessions"}, 0, `{"id":"` + sid + `","title":"fix the failing test"}` + "\n", ""},
		{"null is empty in text", []string{"db", "select parent_id from sessions"}, 0, "parent_id\n\n", ""},
		{"null is null in json", []string{"db", "--json", "select parent_id from sessions"}, 0, `{"parent_id":null}` + "\n", ""},
		{"a write fails", []string{"db", "insert", "into", "kv", "values", "('x','{}',0)"}, 1, "", "readonly"},
		{"no store names the path", []string{"-C", empty, "db", "select 1"}, 1, "", "no store at " + perm.TildePath(emptyStore)},
		{"an unmigrated store is refused", []string{"-C", stale, "db", "select 1"}, 1, "", "zemi health"},
		{"global reads the global store", []string{"db", "--global", "select", "count(*)", "from", "sessions"}, 0, "count(*)\n0\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut strings.Builder
			code := execute(c.args, &out, &errOut, func(options) int { return 0 })
			if code != c.code {
				t.Fatalf("exit %d, want %d; stderr: %s", code, c.code, errOut.String())
			}
			if c.stdout != "" && !strings.Contains(out.String(), c.stdout) {
				t.Fatalf("stdout %q, want it to hold %q", out.String(), c.stdout)
			}
			if c.stderr != "" && !strings.Contains(errOut.String(), c.stderr) {
				t.Fatalf("stderr %q, want it to hold %q", errOut.String(), c.stderr)
			}
		})
	}
}
