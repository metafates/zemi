// projects.go is what zemi knows of other projects: every root it keeps
// state for, from the trust file and the root files of <state>/projects.
package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// Project is a directory zemi keeps state for.
type Project struct {
	Root    string    // the trust root, absolute
	Trust   string    // the saved decision, "trusted" or "untrusted"; "" for none
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

// TrustWord is the project's trust state as the lists print it: ~ and /
// are untrusted, since no decision for them is ever saved.
func (p Project) TrustWord() string {
	switch {
	case p.Missing:
		return "missing"
	case TrustUnsaved(p.Root):
		return "untrusted"
	case p.Trust == "":
		return "undecided"
	}
	return p.Trust
}

// splashProjects is what the splash offers in place of recent sessions: the
// three projects zemi last started in besides this one, where zemi is rarely
// the user's pick. That is ~ or /, where a terminal starts, and a directory
// outside a repository while it has no session. Cached: the splash renders
// every frame a job runs; forget and delete mark it stale.
func (h *Host) splashProjects() []Project {
	root := h.trustRoot()
	if !TrustUnsaved(root) && len(h.recentSessions()) > 0 {
		return nil
	}
	h.splashMu.Lock()
	defer h.splashMu.Unlock()
	if !h.splashStale.Swap(false) {
		return h.splash
	}
	h.splash = nil
	if _, err := os.Stat(filepath.Join(h.Dirs.RepoRoot(), ".git")); err == nil && !TrustUnsaved(root) {
		return nil
	}
	for _, p := range ListProjects(h.Dirs, false) {
		if p.Root != root && !p.Missing && !p.Opened.IsZero() && len(h.splash) < 3 {
			h.splash = append(h.splash, p)
		}
	}
	return h.splash
}

func (h *Host) projectsCommands() []*Command {
	return []*Command{{
		Name: "projects", NArgs: NArgsAny, Desc: "switch to another project, forget its trust or delete its data",
		GoFn: (*Host).projectsCommand,
		GoComplete: SubcommandCompleter([]string{"switch", "forget", "delete"}, func(h *Host, _ string) []string {
			var roots []string
			for _, p := range ListProjects(h.Dirs, false) {
				if p.Root != h.trustRoot() {
					roots = append(roots, TildePath(p.Root))
				}
			}
			return roots
		}),
	}}
}

// projectsCommand is /projects. A verb's argument is the rest of the line,
// so a root with spaces needs no quoting.
func (h *Host) projectsCommand(line string) error {
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg, yes := strings.CutSuffix(strings.TrimSpace(arg), " --yes")
	ui, noUI := h.Screen()
	if verb == "" {
		if noUI != nil {
			return fmt.Errorf("%w; zemi projects lists them", noUI)
		}
		h.GoSafe("projects", func() { h.openProjects(ui, false, "") })
		return nil
	}
	if verb == "switch" {
		return h.switchProject(ui, noUI, arg)
	}
	if verb != "forget" && verb != "delete" {
		return Usagef("/projects takes switch, forget or delete and a project's directory")
	}
	p, err := h.projectNamed(arg)
	if err != nil {
		return err
	}
	switch {
	case p.Root == h.trustRoot() && verb == "forget":
		return Usagef("zemi runs in %s: /trust changes its trust", TildePath(p.Root))
	case p.Root == h.trustRoot():
		return Usagef("zemi runs in %s: delete its data from another project", TildePath(p.Root))
	case verb == "forget":
		if p.Trust == "" {
			return Usagef("no trust decision is saved for %s", TildePath(p.Root))
		}
		if err := h.forgetTrust(p.Root); err != nil {
			return err
		}
		h.Notify(logging.Info, "Trust decision forgotten", TildePath(p.Root)+" asks again when zemi opens it")
		if noUI == nil {
			h.GoSafe("projects", func() { h.openProjects(ui, true, "") })
		}
		return nil
	case !yes:
		return AskFirst("projects delete "+p.Root+" --yes", "", func(confirm string) error {
			if noUI != nil {
				return noUI
			}
			h.GoSafe("projects", func() { h.openProjects(ui, false, confirm) })
			return nil
		})
	case noUI != nil:
		return h.deleteProject(p) // the process exits when the command returns
	}
	h.GoSafe("delete project", func() {
		err := func() error {
			defer h.Busy("Deleting "+TildePath(p.Root), nil)()
			return h.deleteProject(p)
		}()
		if err != nil {
			h.CommandFailed("projects", err)
		}
		h.openProjects(ui, true, "")
	})
	return nil
}

// switchProject restarts zemi in dir (UIHooks.Relaunch), which any
// directory may be. It refuses while live work would stop with this zemi.
func (h *Host) switchProject(ui UIHooks, noUI error, arg string) error {
	dir, err := h.argDir(arg)
	if err != nil {
		return err
	}
	if noUI != nil {
		return fmt.Errorf("%w; zemi -C %s opens it", noUI, TildePath(dir))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return Usagef("%s is not a directory", TildePath(dir))
	}
	if perm.CanonPath(dir) == perm.CanonPath(h.Dirs.ProjectDir) {
		return Usagef("zemi already runs in %s", TildePath(dir))
	}
	if h.AgentActive() {
		return Usagef("a turn is running: esc esc stops it, then switch")
	}
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return err
	}
	ui.Relaunch(dir)
	return nil
}

// argDir is a directory a /projects verb names: it may start with ~ and is
// taken from the launch dir when relative.
func (h *Host) argDir(arg string) (string, error) {
	if arg == "" {
		return "", Usagef("/projects switch, forget and delete take a project's directory")
	}
	dir := perm.ExpandHome(arg)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(h.Dirs.ProjectDir, dir)
	}
	return filepath.Clean(dir), nil
}

// projectNamed is the listed project at the directory arg names.
func (h *Host) projectNamed(arg string) (Project, error) {
	root, err := h.argDir(arg)
	if err != nil {
		return Project{}, err
	}
	for _, p := range ListProjects(h.Dirs, false) {
		if p.Root == root {
			return p, nil
		}
	}
	return Project{}, Usagef("zemi keeps nothing for %s; /projects lists what it does", TildePath(root))
}

func (h *Host) forgetTrust(root string) error {
	saved := h.Dirs.ReadTrust()
	if _, ok := saved[root]; !ok {
		return nil
	}
	delete(saved, root)
	return h.Dirs.WriteTrust(saved)
}

// deleteProject erases what zemi keeps for a project other than this one: its
// store and state dir, its sessions' scratchpads and plans, its build caches
// and its trust decision. The claim refuses while another zemi has it open.
func (h *Host) deleteProject(p Project) error {
	ids, release, err := store.ClaimForDelete(store.ProjectDBPath(h.Dirs.StateHome, p.Root))
	if errors.Is(err, store.ErrInUse) {
		return Usagef("zemi has %s open in another window; quit it there first", TildePath(p.Root))
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		h.removeSessionFiles(id)
	}
	key := trustKey(p.Root)
	err = errors.Join(os.RemoveAll(projectState(h.Dirs.StateHome, p.Root)),
		os.RemoveAll(filepath.Join(h.Dirs.StateHome, "sandbox-cache", key)))
	if h.Dirs.CacheHome != "" {
		err = errors.Join(err, os.RemoveAll(filepath.Join(h.Dirs.CacheHome, "lsp", key)))
	}
	release()
	h.splashStale.Store(true)
	if err = errors.Join(err, h.forgetTrust(p.Root)); err != nil {
		return err
	}
	h.Notify(logging.Info, "Project data deleted", TildePath(p.Root))
	return nil
}

// openProjects opens /projects, or with refresh replaces the open list; it
// opens every store, so it never runs on the key loop. confirm arms the
// Delete that runs it (Overlay.Confirm).
func (h *Host) openProjects(ui UIHooks, refresh bool, confirm string) {
	items, sel := h.projectItems()
	ui.OpenOverlay(Overlay{
		ID: "projects", Title: "Projects", Origin: lua.OriginBuiltin, Items: items, Selected: sel,
		Anchor: OverlayBottomLeft, Refresh: refresh, Confirm: confirm,
		Empty: "No projects yet. Start zemi in a repository and it appears here.",
	})
}

// projectItems is a row per project, the selection on the first that zemi
// does not run in.
func (h *Host) projectItems() (items []any, sel int) {
	sel = -1
	for i, p := range ListProjects(h.Dirs, true) {
		desc := []string{p.TrustWord()}
		if p.Sessions > 0 {
			desc = append(desc, fmt.Sprintf("%d %s", p.Sessions, plural(p.Sessions, "session", "sessions")))
		}
		if !p.Opened.IsZero() {
			desc = append(desc, ago(p.Opened.UnixMilli()))
		}
		item := map[string]any{"text": sanitize.String(TildePath(p.Root)), "desc": strings.Join(desc, " · "), "fields": h.projectFields(p)}
		switch {
		case p.Root == h.trustRoot():
			item["key"] = "current"
			item["detail"] = "zemi runs here: /trust changes its trust and /permissions its allowed commands. Switch to another project to delete this one's data."
			items = append(items, item)
			continue
		case p.Missing:
			item["detail"] = TildePath(p.Root) + " no longer exists. Delete erases what zemi kept for it."
		default:
			item["detail"] = "Switch restarts zemi there: a job still running here stops, as on quit. Delete erases what zemi keeps for it: sessions, memories, recipes, allowed commands and the trust decision. The project's own files stay."
		}
		if sel < 0 {
			sel = i
		}
		var actions []any
		if !p.Missing {
			actions = append(actions, map[string]any{"text": "Switch to it", "hotkey": "s", "run": "projects switch " + p.Root})
		}
		if p.Trust != "" {
			actions = append(actions, map[string]any{"text": "Forget trust", "hotkey": "f", "run": "projects forget " + p.Root})
		}
		item["actions"] = append(actions, map[string]any{"text": "Delete", "hotkey": "x", "danger": true, "run": "projects delete " + p.Root + " --yes"})
		items = append(items, item)
	}
	return items, max(sel, 0)
}

// projectFields is what a project's detail says of it; an empty value is
// left out.
func (h *Host) projectFields(p Project) []any {
	sessions, memories, recipes, errText := "", "", "", ""
	if p.Latest != nil {
		sessions = fmt.Sprintf("%d · latest %s", p.Sessions, ago(p.Latest.UpdatedAt))
		if t := strings.Join(strings.Fields(sanitize.String(p.Latest.Title)), " "); t != "" {
			sessions += ": " + t
		}
	}
	if p.Memories > 0 {
		memories = strconv.Itoa(p.Memories)
	}
	state := projectState(h.Dirs.StateHome, p.Root)
	if dirs, _ := os.ReadDir(filepath.Join(state, "skills")); len(dirs) > 0 {
		recipes = strconv.Itoa(len(dirs))
	}
	var allowed []string
	b, _ := os.ReadFile(filepath.Join(state, "permissions"))
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			allowed = append(allowed, sanitize.String(line))
		}
	}
	if p.StatsErr != nil {
		errText = p.StatsErr.Error()
	}
	return fieldList("trust", p.TrustWord(), "sessions", sessions, "memories", memories, "recipes", recipes,
		"allowed", strings.Join(allowed, ", "), "data", TildePath(state), "error", errText)
}
