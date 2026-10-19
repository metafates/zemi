package hostapi

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// addTo stores a text message in any store, not the host's.
func addTo(t *testing.T, db *store.DB, sid, role, text string) {
	t.Helper()
	parts, err := json.Marshal(fantasy.Message{Role: fantasy.MessageRole(role), Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}}, json.Deterministic(true))
	if err == nil {
		err = db.AddMessage(&store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)})
	}
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
}

// otherStore opens another project's store and writes the root file that
// makes ListProjects find it.
func otherStore(t *testing.T, stateHome string) (string, *store.DB) {
	t.Helper()
	root := t.TempDir()
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
	h       *Host
	dirs    config.Dirs
	plain   string // this project's session, three text matches
	root    string // the other project
	deep    string // its session, the one match below twenty rows
	titled  string // its session, matched by title alone
	garbage string // a root whose store SQLite cannot read
}

func newFindFixture(t *testing.T) findFixture {
	t.Helper()
	h, _ := newTestHostDB(t)
	f := findFixture{h: h, dirs: h.Dirs, plain: store.ShortID()}
	if err := h.DB.CreateSession(f.plain, "", "Postgres tuning", ""); err != nil {
		t.Fatal(err)
	}
	addTo(t, h.DB, f.plain, "user", "Why does Postgres pick a seq scan?")
	addTo(t, h.DB, f.plain, "assistant", "Postgres planner estimates rows badly.")
	long := strings.Repeat("filler ", 40) + "Postgres again" + agent.Attached("file", "<html>noise</html>")
	addTo(t, h.DB, f.plain, "user", long)

	var db *store.DB
	f.root, db = otherStore(t, f.dirs.StateHome)
	f.deep, f.titled = store.ShortID(), store.ShortID()
	for _, s := range []struct{ id, title string }{{f.deep, "Kubernetes rollout"}, {f.titled, "Refactor the parser"}} {
		if err := db.CreateSession(s.id, "", s.title, ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		addTo(t, db, f.deep, "user", "step "+strconv.Itoa(i))
	}
	addTo(t, db, f.deep, "user", "the kubelet waits: Postgres is unreachable")

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

// TestFindSessions: a query reaches every project, groups a session's
// matches, ranks a title hit first, cuts what the user did not type and
// names, rather than fails on, the store it cannot read.
func TestFindSessions(t *testing.T) {
	f := newFindFixture(t)
	found := FindSessions(f.dirs, "postgres")
	if found.Projects != 2 {
		t.Errorf("projects searched = %d, want the 2 stores it read", found.Projects)
	}
	if !slices.Equal(found.Skipped, []string{f.garbage}) {
		t.Errorf("skipped = %v, want the unreadable store %s", found.Skipped, f.garbage)
	}
	var got []string
	for _, s := range found.Sessions {
		got = append(got, s.ID)
	}
	if !slices.Equal(got, []string{f.plain, f.deep}) {
		t.Fatalf("sessions = %v, want %v (more hits first)", got, []string{f.plain, f.deep})
	}
	plain := found.Sessions[0]
	if plain.Count != 3 || len(plain.Hits) != 3 || plain.Title != "Postgres tuning" || plain.Root != f.dirs.TrustRoot() {
		t.Errorf("plain session = %+v, want 3 hits of this project's session", plain)
	}
	newest := plain.Hits[0].Text
	if !strings.Contains(newest, "Postgres again") || !strings.Contains(newest, "…") || strings.Contains(newest, "attached-files") {
		t.Errorf("newest snippet = %q, want a window around the match without what zemi attached", newest)
	}
	for _, h := range f.hits(t, found, f.deep) {
		if !strings.Contains(h.Text, "kubelet") {
			t.Errorf("deep snippet = %q, want the match in it", h.Text)
		}
	}

	titled := FindSessions(f.dirs, "parser")
	if len(titled.Sessions) != 1 || titled.Sessions[0].ID != f.titled || !titled.Sessions[0].TitleHit || titled.Sessions[0].Count != 0 {
		t.Fatalf("title search = %+v, want %s matched by its title", titled.Sessions, f.titled)
	}
	if none := FindSessions(f.dirs, " !? "); none.Projects != 0 || none.Sessions != nil {
		t.Fatalf("a query of punctuation = %+v, want nothing and no store opened", none)
	}
}

// hits is a found session's hits, or a failure when the search missed it.
func (f findFixture) hits(t *testing.T, found Found, id string) []FoundHit {
	t.Helper()
	for _, s := range found.Sessions {
		if s.ID == id {
			return s.Hits
		}
	}
	t.Fatalf("session %s not found in %+v", id, found.Sessions)
	return nil
}

// TestListAllSessions: every project's sessions, this project first.
func TestListAllSessions(t *testing.T) {
	f := newFindFixture(t)
	groups := ListAllSessions(f.dirs)
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

// TestFindSession: an id is looked up here first, then everywhere else, and
// a store that cannot be read does not stop the search.
func TestFindSession(t *testing.T) {
	f := newFindFixture(t)
	for _, c := range []struct {
		name, id, root string
		err            error
	}{
		{"this project's session", f.plain, f.dirs.TrustRoot(), nil},
		{"another project's session", f.deep, f.root, nil},
		{"no such session", "missing1", "", store.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, sess, err := FindSession(f.dirs, c.id)
			if !errors.Is(err, c.err) {
				t.Fatalf("FindSession(%q) error = %v, want %v", c.id, err, c.err)
			}
			if root != c.root || err == nil && sess.ID != c.id {
				t.Fatalf("FindSession(%q) = %q, %+v, want root %q", c.id, root, sess, c.root)
			}
		})
	}
}

// TestOpenFindSessions: the box opens at once, the result replaces it with one
// row per session, and a query nothing matched says so.
func TestOpenFindSessions(t *testing.T) {
	f := newFindFixture(t)
	fake := &fakeUI{}
	f.h.SetUIHooks(fake)
	if err := f.h.OpenFindSessions("postgres"); err != nil {
		t.Fatal(err)
	}
	open := f.waitOverlays(t, fake)
	if want := `Searching every project for "postgres"`; !strings.Contains(open[0].Empty, want) {
		t.Errorf("the searching box says %q, want %q", open[0].Empty, want)
	}
	res := open[1]
	if !res.Refresh || !res.Wide || res.ID != "sessions search" || res.Title != "Search sessions" {
		t.Errorf("result overlay = %+v", res)
	}
	if want := "4 hits in 2 sessions · 2 projects searched · 1 unreadable"; res.Body != want {
		t.Errorf("body = %q, want %q", res.Body, want)
	}
	if len(res.Items) != 2 {
		t.Fatalf("%d rows, want 2", len(res.Items))
	}
	row, ok := res.Items[0].(map[string]any)
	if !ok {
		t.Fatalf("row = %T, want a map", res.Items[0])
	}
	if row["text"] != markedTitle(f.plain, "Postgres tuning") {
		t.Errorf("text = %v", row["text"])
	}
	if desc, _ := row["desc"].(string); !strings.Contains(desc, filepath.Base(f.dirs.TrustRoot())) || !strings.Contains(desc, "3 hits") {
		t.Errorf("desc = %q, want the project and the hit count", desc)
	}
	if detail, _ := row["detail"].(string); !strings.Contains(detail, "· ") {
		t.Errorf("detail = %q, want the hits in it", detail)
	}
	actions, _ := row["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", actions)
	}
	if got := actions[0].(map[string]any)["run"]; got != "sessions open "+f.plain {
		t.Errorf("action run = %v", got)
	}

	fake = &fakeUI{}
	f.h.SetUIHooks(fake)
	if err := f.h.OpenFindSessions("zzznothing"); err != nil {
		t.Fatal(err)
	}
	if got := f.waitOverlays(t, fake)[1].Empty; !strings.Contains(got, `No match for "zzznothing" in 2 projects.`) {
		t.Errorf("empty = %q", got)
	}
}

// waitOverlays waits for the search's two boxes: the one it opens and the one
// it replaces it with.
func (f findFixture) waitOverlays(t *testing.T, fake *fakeUI) []Overlay {
	t.Helper()
	waitFor(t, func() error {
		if n := len(fake.overlays()); n != 2 {
			return fmt.Errorf("%d overlays, want 2", n)
		}
		return nil
	})
	return fake.overlays()
}

// TestOpenFoundSession: a row of another project's store restarts zemi there,
// resuming that session; an id nowhere is a usage error.
func TestOpenFoundSession(t *testing.T) {
	f := newFindFixture(t)
	fake := &fakeUI{}
	f.h.SetUIHooks(fake)
	if err := f.h.OpenFoundSession(f.deep); err != nil {
		t.Fatalf("OpenFoundSession: %v", err)
	}
	if fake.relaunched != f.root || fake.resumed != f.deep {
		t.Errorf("relaunched to %q resuming %q, want %q and %q", fake.relaunched, fake.resumed, f.root, f.deep)
	}
	if err := f.h.OpenFoundSession("missing1"); err == nil || !strings.Contains(err.Error(), `no session "missing1"`) {
		t.Errorf("an unknown id = %v, want a usage error", err)
	}
}
