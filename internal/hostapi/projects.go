// projects.go is what zemi knows of other projects: every root it keeps
// state for, from the trust file and the root files of <state>/projects.

package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/config"
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
	Used    time.Time // when zemi was last started or left there; zero with no root file

	// read with stats: a store zemi cannot read leaves them zero and says why
	Messages int
	Latest   *store.Session // the newest top-level session
	StatsErr error
}

// ListProjects lists every root zemi keeps state for, the last used first
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
		root, used, ok := store.ProjectRoot(filepath.Join(dirs.StateHome, "projects", e.Name()))
		if !ok {
			continue
		}
		if byRoot[root] == nil {
			byRoot[root] = &Project{Root: root}
		}
		byRoot[root].Used = used
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
		return cmp.Or(b.Used.Compare(a.Used), cmp.Compare(a.Root, b.Root))
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
		// Messages counts the chat log's rows: runs and the anchor
		// row no longer say how much a project has been used
		var n int64
		if n, err = db.LogCount(); err == nil {
			p.Messages = int(n)
			var latest []store.LogEntry
			if latest, err = db.LogTail(1); err == nil && len(latest) > 0 {
				at, perr := time.Parse(time.RFC3339Nano, latest[0].CreatedAt)
				if perr == nil {
					line, _, _ := strings.Cut(latest[0].Text, "\n")
					p.Latest = &store.Session{ID: store.MainSessionID, Title: line, UpdatedAt: at.UnixMilli()}
				}
			}
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
		return config.Untrusted
	case p.Trust == "":
		return "undecided"
	}
	return p.Trust
}

// splashProjects is what the splash offers in place of recent sessions: the
// three projects zemi was last used in besides this one, where zemi is rarely
// the user's pick. That is ~ or /, where a terminal starts, and a directory
// outside a repository while it has no session. Cached: the splash renders
// every frame a job runs; forget and delete mark it stale.
func (h *Host) splashProjects() []Project {
	root := h.trustRoot()
	// the chat has begun: the splash's picks would bury the chat's tail
	if !TrustUnsaved(root) {
		if n, _ := h.DB.LogCount(); n > 0 {
			return nil
		}
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
		if p.Root != root && !p.Missing && !p.Used.IsZero() && len(h.splash) < 3 {
			h.splash = append(h.splash, p)
		}
	}
	return h.splash
}

// OffersProjects reports whether the splash offers other projects here.
func (h *Host) OffersProjects() bool { return len(h.splashProjects()) > 0 }

func (h *Host) projectsCommands() []*Command {
	return []*Command{{
		Name: "projects", NArgs: NArgsAny, Desc: "switch to another project, forget its trust or delete its data",
		GoFn: (*Host).projectsCommand,
		// a verb is offered only the roots it acts on, as the list's actions are
		GoComplete: SubcommandCompleter([]string{"switch", "forget", "delete"}, func(h *Host, sub string) []string {
			var roots []string
			for _, p := range ListProjects(h.Dirs, false) {
				if p.Root != h.trustRoot() && !(sub == "switch" && p.Missing) && !(sub == "forget" && p.Trust == "") {
					roots = append(roots, perm.TildePath(p.Root))
				}
			}
			return roots
		}),
	}}
}

// projectsCommand is /projects. A verb's argument is the rest of the line,
// so a root with spaces needs no quoting.
func (h *Host) projectsCommand(line string, confirm bool) error {
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg, yes := ConfirmArgs(arg, confirm)
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
		return Usagef("zemi runs in %s: /trust changes its trust", perm.TildePath(p.Root))
	case p.Root == h.trustRoot():
		return Usagef("zemi runs in %s: delete its data from another project", perm.TildePath(p.Root))
	case verb == "forget":
		if p.Trust == "" {
			return Usagef("no trust decision is saved for %s", perm.TildePath(p.Root))
		}
		if err := h.forgetTrust(p.Root); err != nil {
			return err
		}
		h.Notify(slog.LevelInfo, "Trust decision forgotten", perm.TildePath(p.Root)+" asks again when zemi opens it")
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
			defer h.Busy("Deleting "+perm.TildePath(p.Root), nil)()
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
		return fmt.Errorf("%w; zemi -C %s opens it", noUI, perm.TildePath(dir))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return Usagef("%s is not a directory", perm.TildePath(dir))
	}
	if perm.CanonPath(dir) == perm.CanonPath(h.Dirs.ProjectDir) {
		return Usagef("zemi already runs in %s", perm.TildePath(dir))
	}
	if h.AgentActive() {
		return errTurnRunning
	}
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return err
	}
	ui.Relaunch(dir, "")
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
	return Project{}, Usagef("zemi keeps nothing for %s; /projects lists what it does", perm.TildePath(root))
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
// store and state dir, its sessions' scratchpads and plans and its trust
// decision. The claim refuses while another zemi has it open.
func (h *Host) deleteProject(p Project) error {
	// without a root or a state home the paths below are the working
	// directory's, or the state home itself
	if p.Root == "" || h.Dirs.StateHome == "" {
		return Usagef("zemi keeps nothing for %s; /projects lists what it does", perm.TildePath(p.Root))
	}
	_, release, err := store.ClaimForDelete(store.ProjectDBPath(h.Dirs.StateHome, p.Root))
	if errors.Is(err, store.ErrInUse) {
		return Usagef("zemi has %s open in another window; quit it there first", perm.TildePath(p.Root))
	}
	if err != nil {
		return err
	}
	err = errors.Join(os.RemoveAll(projectState(h.Dirs.StateHome, p.Root)))
	release()
	h.splashStale.Store(true)
	if err = errors.Join(err, h.forgetTrust(p.Root)); err != nil {
		return err
	}
	h.Notify(slog.LevelInfo, "Project data deleted", perm.TildePath(p.Root))
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
		Keys: []OverlayKey{{Key: "s", Desc: "switch"}, {Key: "x", Desc: "delete"}},
	})
}

// projectItems is a row per project, the selection on the first that zemi
// does not run in and that exists.
func (h *Host) projectItems() (items []OverlayItem, sel int) {
	sel = -1
	for i, p := range ListProjects(h.Dirs, true) {
		// each column padded, blank when empty, so the age lines up; a word
		// like "untrusted this run" fills the messages column's blank, or
		// moves only its own row's age
		sessions := ""
		if p.Messages > 0 {
			sessions = fmt.Sprintf(" · %d %s", p.Messages, Plural(p.Messages, "message", "messages"))
		}
		desc := fmt.Sprintf("%-24s", fmt.Sprintf("%-9s%s", h.trustWord(p), sessions))
		if !p.Used.IsZero() {
			desc += " · " + ago(p.Used.UnixMilli())
		}
		item := OverlayItem{Glyph: ProjectMark(p.Root), Text: sanitize.String(perm.TildePath(p.Root)), Tail: true, Desc: strings.TrimRight(desc, " "), Fields: h.projectFields(p)}
		switch {
		case p.Root == h.trustRoot():
			item.Tag = "current"
			item.Detail = "zemi runs here: /trust changes its trust. Switch to another project to delete this one's data."
			items = append(items, item)
			continue
		case p.Missing:
			item.Detail = perm.TildePath(p.Root) + " no longer exists. Delete erases what zemi kept for it."
		default:
			item.Detail = "Switch restarts zemi there: a job still running here stops, as on quit. Delete erases what zemi keeps for it: sessions, recipes and the trust decision. The project's own files stay."
		}
		if sel < 0 && !p.Missing {
			sel = i
		}
		var keys []ItemKey
		if !p.Missing {
			keys = append(keys, ItemKey{Key: "s", Desc: "switch to it", Run: "projects switch " + p.Root})
		}
		if p.Trust != "" {
			keys = append(keys, ItemKey{Key: "f", Desc: "forget trust", Run: "projects forget " + p.Root})
		}
		item.Keys = append(keys, ItemKey{Key: "x", Desc: "delete", Danger: true, Run: "projects delete " + p.Root})
		items = append(items, item)
	}
	return items, max(sel, 0)
}

// trustWord is a project's trust word in /projects: TrustWord, or for this
// project the running decision when it differs, so the list and the
// statusline name one state.
func (h *Host) trustWord(p Project) string {
	word := p.TrustWord()
	if p.Root != h.trustRoot() {
		return word
	}
	run := config.Untrusted
	if h.ProjectTrusted() {
		run = config.Trusted
	}
	if run != word {
		return run + " this run"
	}
	return word
}

// projectFields is what a project's detail says of it; an empty value is
// left out. Its trust is the decision a gone root keeps too, and its path
// repeats the detail's title, which the UI shows only when the title clips.
func (h *Host) projectFields(p Project) []Field {
	msgs, recipes, errText := "", "", ""
	if p.Latest != nil {
		msgs = fmt.Sprintf("%d · latest %s", p.Messages, ago(p.Latest.UpdatedAt))
		if t := sanitize.Line(p.Latest.Title); t != "" {
			msgs += ": " + markedTitle(p.Root, t)
		}
	}
	state := projectState(h.Dirs.StateHome, p.Root)
	if dirs, _ := os.ReadDir(filepath.Join(state, "skills")); len(dirs) > 0 {
		recipes = strconv.Itoa(len(dirs))
	}
	if p.StatsErr != nil {
		errText = p.StatsErr.Error()
	}
	trust := h.trustWord(Project{Root: p.Root, Trust: p.Trust})
	return fieldList("trust", trust, "messages", msgs, "recipes", recipes,
		"path", sanitize.String(perm.TildePath(p.Root)), "data", perm.TildePath(state), "error", errText)
}
