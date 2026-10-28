package hostapi

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// TestProjectsCommand runs /projects verbs in order over one state tree:
// this project, another with a session, its files and a decision, a third a
// store handle holds open, as another zemi would, and a trusted one since
// deleted, used last.
func TestProjectsCommand(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cur, other, held, gone := filepath.Join(tmp, "cur"), filepath.Join(tmp, "other"), filepath.Join(tmp, "held"), filepath.Join(tmp, "gone")
	dirs := config.Dirs{ProjectDir: cur, StateHome: filepath.Join(tmp, "state"), DataHome: filepath.Join(tmp, "data"), CacheHome: filepath.Join(tmp, "cache")}
	for _, root := range []string{cur, other, held, gone} {
		os.MkdirAll(root, 0o755)
		if root == cur {
			continue // the host opens it
		}
		g, p, err := store.OpenForDirs(dirs.StateHome, root)
		if err != nil {
			t.Fatal(err)
		}
		g.Close()
		if root == held {
			defer p.Close()
			continue
		}
		if root == gone {
			p.Close()
			os.Remove(gone)
			continue
		}
		if _, err := p.AppendLog("user", "fix the flaky test", "", "", ""); err != nil {
			t.Fatal(err)
		}
		p.Close()
	}
	if err := dirs.WriteTrust(map[string]string{other: "trusted", gone: "trusted"}); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(projectState(dirs.StateHome, other), "skills"),
	}
	for _, f := range files {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("echo \x1b]x\ngo test ./...\n"), 0o600) // an unterminated escape hides only its own line
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), dirs)
	ui := &pluginsUI{opened: make(chan Overlay, 4), notes: make(chan string, 4)}
	h.SetUIHooks(ui)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	// a verb completes only the roots it acts on, in the tilde form the composer shows
	for verb, roots := range map[string][]string{"switch": {held, other}, "forget": {gone, other}, "delete": {gone, held, other}} {
		want := make([]string, len(roots))
		for i, root := range roots {
			want[i] = perm.TildePath(root)
		}
		if got := h.Commands.Lookup("projects").GoComplete(h, "", "/projects "+verb+" "); !slices.Equal(got, want) {
			t.Errorf("/projects %s completes %q, want %q", verb, got, want)
		}
	}
	for _, c := range []struct {
		name, args string
		err        string   // a usage error, which opens nothing
		want, not  []string // in the list it opens and the toasts
		gone       bool     // the other project's files are deleted
		relaunch   string   // the dir zemi restarts in, which opens nothing
		busy       bool     // a turn runs
	}{
		{
			name: "the list", args: "",
			want: []string{
				// the running decision beside the saved one, and a gone root's decision in its detail
				"desc:untrusted this run       · now", "[trust untrusted this run]", "tag:current",
				"desc:missing                  · now", "[trust trusted] [messages ]",
				"desc:trusted   · 1 message    · now", "[messages 1 · latest now: " + markedTitle(store.MainSessionID, "fix the flaky test") + "]",
				"desc:switch to it key:s run:projects switch " + other, "desc:forget trust key:f run:projects forget " + other, "danger:true desc:delete key:x run:projects delete " + other,
				"Selected:2", // held: the gone root, used last, has nothing to switch to
			},
			not: []string{"run:projects switch " + gone},
		},
		{name: "forget a gone root", args: "forget " + gone, want: []string{"Trust decision forgotten"}},
		{name: "switch", args: "switch " + other + "/sub/..", relaunch: other},
		{name: "switch mid-turn", args: "switch " + other, busy: true, err: "a turn is running"},
		{name: "switch to this project", args: "switch " + cur, err: "zemi already runs in"},
		{name: "switch to a missing dir", args: "switch " + filepath.Join(tmp, "nope"), err: "is not a directory"},
		{name: "forget this project", args: "forget " + cur, err: "/trust changes its trust"},
		{name: "delete this project", args: "delete " + cur + " --yes", err: "delete its data from another project"},
		{name: "an unknown dir", args: "forget " + filepath.Join(tmp, "nope"), err: "zemi keeps nothing for"},
		{name: "delete asks first", args: "delete " + other, want: []string{"Confirm:projects delete " + other + " --yes"}},
		{name: "forget drops the decision", args: "forget " + other, want: []string{"Trust decision forgotten", "undecided"}, not: []string{"Forget trust"}},
		{name: "delete a store another zemi holds", args: "delete " + held + " --yes", want: []string{"open in another window", "run:projects delete " + held}},
		{name: "delete", args: "delete " + other + " --yes", want: []string{"Project data deleted"}, not: []string{"run:projects delete " + other}, gone: true},
	} {
		h.agents.mu.Lock()
		h.agents.active = c.busy
		h.agents.mu.Unlock()
		ui.relaunched = ""
		if err := h.RunCommand(t.Context(), "projects", c.args, false); c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) || len(ui.opened) > 0 {
				t.Fatalf("%s: err %v, %d lists opened", c.name, err, len(ui.opened))
			}
			continue
		} else if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.relaunch != "" {
			if ui.relaunched != c.relaunch || len(ui.opened) > 0 {
				t.Fatalf("%s: relaunched in %q, %d lists opened", c.name, ui.relaunched, len(ui.opened))
			}
			continue
		}
		var list Overlay
		select {
		case list = <-ui.opened:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: no list opened", c.name)
		}
		got := fmt.Sprintf("%s Selected:%d Confirm:%s", dumpItems(list.Items), list.Selected, list.Confirm)
		for len(ui.notes) > 0 {
			got += "\n" + <-ui.notes
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q in\n%s", c.name, n, got)
			}
		}
		for _, f := range files {
			if _, err := os.Stat(f); c.gone != errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %s: %v", c.name, f, err)
			}
		}
	}
	h.SetUIHooks(nil)
	for _, args := range []string{"", "switch " + other} {
		if err := h.RunCommand(t.Context(), "projects", args, false); !errors.Is(err, ErrNeedsTUI) {
			t.Fatalf("headless /projects %s: %v", args, err)
		}
	}
}

// TestDeleteProjectWithoutState: a project with no root, or a host with no
// state home, is refused: the paths deleteProject removes would be the
// working directory's, or the state home itself.
func TestDeleteProjectWithoutState(t *testing.T) {
	h, _ := newTestHost(t)
	dirs := h.Dirs
	defer func() { h.Dirs = dirs }()
	dirs.StateHome = t.TempDir()
	for _, c := range []struct{ name, root, state string }{
		{"no root", "", dirs.StateHome},
		{"no state home", t.TempDir(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.Dirs.StateHome, h.Dirs.ProjectDir = c.state, c.root
			if err := h.deleteProject(Project{Root: c.root}); err == nil {
				t.Error("delete ran with no state to delete")
			}
		})
	}
	if _, err := os.Stat(dirs.StateHome); err != nil {
		t.Errorf("the state home went with it: %v", err)
	}
}

// TestSplashProjects: the splash offers the other projects that still exist
// in ~ always and outside a repository only while the dir has no session.
func TestSplashProjects(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, c := range []struct {
		name    string
		dir     string // under a temp dir, or home when "~"
		repo    bool
		session bool
		listed  bool
	}{
		{name: "home with a session", dir: "~", session: true, listed: true},
		{name: "a new dir outside a repository", dir: "plain", listed: true},
		{name: "a dir with a session", dir: "plain", session: true},
		{name: "a repository", dir: "repo", repo: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp, _ := filepath.EvalSymlinks(t.TempDir())
			state, other, gone := filepath.Join(tmp, "state"), filepath.Join(tmp, "other"), filepath.Join(tmp, "gone")
			dir := filepath.Join(tmp, c.dir)
			if c.dir == "~" {
				dir = home
			}
			for _, d := range []string{dir, other, gone} {
				os.MkdirAll(d, 0o755)
				if d == dir {
					continue
				}
				g, p, err := store.OpenForDirs(state, d)
				if err != nil {
					t.Fatal(err)
				}
				g.Close()
				p.Close()
			}
			os.Remove(gone)
			if c.repo {
				os.Mkdir(filepath.Join(dir, ".git"), 0o755)
			}
			log := slog.New(slog.DiscardHandler)
			h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{ProjectDir: dir, StateHome: state})
			t.Cleanup(h.Close)
			if c.session {
				h.DB.AppendLog(store.KindUser, "a prompt", "", "", "")
			}
			var got []string
			for _, p := range h.splashProjects() {
				got = append(got, p.Root)
			}
			if want := map[bool][]string{true: {other}}[c.listed]; !slices.Equal(got, want) {
				t.Fatalf("splashProjects = %q, want %q", got, want)
			}
		})
	}
}

// TestListProjects: every root with a checked root file or a trust decision
// is listed, the last used first, with its trust word and session count.
func TestListProjects(t *testing.T) {
	home, _ := os.UserHomeDir()
	tmp := t.TempDir()
	dirs := config.Dirs{StateHome: t.TempDir()}
	for _, c := range []struct {
		root     string
		trust    string
		opened   time.Duration // ago; 0 writes no root file
		sessions int
	}{
		{root: filepath.Join(tmp, "trusted"), trust: "trusted", opened: time.Minute, sessions: 2},
		{root: filepath.Join(tmp, "undecided"), opened: time.Hour},
		{root: filepath.Join(tmp, "decision-only"), trust: "untrusted"},
		{root: filepath.Join(tmp, "neither"), sessions: 1},
		{root: "/zemi-test-missing/x", trust: "trusted", opened: 2 * time.Hour},
		{root: home, opened: 3 * time.Hour},
	} {
		_ = os.MkdirAll(c.root, 0o755) // the missing one's parent is not writable
		if c.opened != 0 || c.sessions > 0 {
			g, p, err := store.OpenForDirs(dirs.StateHome, c.root)
			if err != nil {
				t.Fatal(err)
			}
			for i := range c.sessions {
				if _, err := p.AppendLog("user", fmt.Sprintf("message %d", i), "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			g.Close()
			p.Close()
			rootFile := filepath.Join(filepath.Dir(store.ProjectDBPath(dirs.StateHome, c.root)), "root")
			if c.opened == 0 {
				_ = os.Remove(rootFile)
			} else if at := time.Now().Add(-c.opened); os.Chtimes(rootFile, at, at) != nil {
				t.Fatal("no root file")
			}
		}
		if c.trust != "" {
			saved := dirs.ReadTrust()
			saved[c.root] = c.trust
			if err := dirs.WriteTrust(saved); err != nil {
				t.Fatal(err)
			}
		}
	}
	var got []string
	for _, p := range ListProjects(dirs, true) {
		got = append(got, fmt.Sprintf("%s %s %d", p.Root, p.TrustWord(), p.Messages))
	}
	want := []string{
		filepath.Join(tmp, "trusted") + " trusted 2",
		filepath.Join(tmp, "undecided") + " undecided 0",
		"/zemi-test-missing/x missing 0",
		home + " untrusted 0",
		filepath.Join(tmp, "decision-only") + " untrusted 0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListProjects:\n%q\nwant\n%q", got, want)
	}
}
