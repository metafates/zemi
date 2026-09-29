// skills.go is the skills host API (DESIGN §6.7, §10.5): discovery
// synced against the skills table, the zemi.skills namespace, the enable
// hooks (user skills run in the boot window after user init), and the
// /skills command. Enable/disable require a user or builtin origin: a
// plugin-registered keymap cannot flip a skill.

package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// skillsReg caches discovery results between DB syncs. Everything that
// touches it runs on the interpreter goroutine except the skill tool's
// index read (agent goroutine), hence the mutex.
type skillReg struct {
	mu      sync.RWMutex
	byName  map[string]skill.Skill
	enabled map[string]bool
}

func (r *skillReg) set(all []skill.Skill, enabled map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = make(map[string]skill.Skill, len(all))
	for _, s := range all {
		r.byName[s.Name] = s
	}
	r.enabled = enabled
}

func (r *skillReg) list() []skill.Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.byName), func(a, b skill.Skill) int { return cmp.Compare(a.Name, b.Name) })
}

func (r *skillReg) isEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled[name]
}

func (r *skillReg) get(name string) (skill.Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byName[name]
	return s, ok
}

func (r *skillReg) setEnabledFlag(name string, enabled bool) {
	r.mu.Lock()
	r.enabled[name] = enabled
	r.mu.Unlock()
}

// syncSkills re-discovers skills and reconciles the global-DB table
// (DESIGN §10.5); an existing row keeps its enabled choice. Invalid skill
// dirs are logged and skipped, never fatal.
func (h *Host) syncSkills() {
	if h.GlobalDB == nil {
		return
	}
	if err := h.materializeBuiltinSkills(); err != nil {
		h.Log.Warn("skills", "builtin skills not written", map[string]any{"error": err.Error()})
	}
	dirs := []skill.Dir{
		{Path: h.userSkillsDir(), Source: skill.User},
		{Path: h.agentsSkillsDir(), Source: skill.User},
		{Path: h.builtinSkillsDir(), Source: skill.Builtin},
	}
	for _, p := range h.pluginDirs() {
		dirs = append(dirs, skill.Dir{Path: filepath.Join(p, "skills"), Source: skill.Plugin})
	}
	found := h.discoverSkills(dirs...)
	rows := make([]*store.Skill, 0, len(found))
	for _, s := range found {
		rows = append(rows, &store.Skill{Name: s.Name, Path: s.Dir, Source: string(s.Source)})
	}
	if err := h.GlobalDB.SyncSkills(rows); err != nil {
		h.Log.Error("skills", "sync failed", map[string]any{"error": err.Error()})
		return
	}
	stored, err := h.GlobalDB.ListSkills()
	if err != nil {
		h.Log.Error("skills", "list failed", map[string]any{"error": err.Error()})
		return
	}
	enabled := make(map[string]bool, len(stored))
	for _, r := range stored {
		enabled[r.Name] = r.Enabled
	}
	h.skillsReg.set(found, enabled)
}

// joinIf joins elem under dir, or is "" when dir is unset.
func joinIf(dir, elem string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, elem)
}

func (h *Host) userSkillsDir() string { return joinIf(h.Dirs.ConfigHome, "skills") }

// agentsSkillsDir is ~/.agents/skills, the user skills other agents read too.
func (h *Host) agentsSkillsDir() string { return joinIf(h.Dirs.AgentsHome, "skills") }

// builtinSkillsDir is where the embedded builtin skills are copied so the
// skill tool reads them from disk like any other skill.
func (h *Host) builtinSkillsDir() string { return joinIf(h.Dirs.DataHome, "builtin-skills") }

// materializeBuiltinSkills rewrites the builtin skills on every sync, so an
// upgraded zemi replaces them and drops the ones it no longer ships.
func (h *Host) materializeBuiltinSkills() error {
	dir := h.builtinSkillsDir()
	if dir == "" {
		return nil
	}
	sub, err := fs.Sub(zemiruntime.Skills, "skills")
	if err != nil {
		return err
	}
	return replaceDir(dir, sub)
}

// skillIndex is what the skill tool may load (modelSkills).
func (h *Host) skillIndex() []tools.SkillInfo {
	var out []tools.SkillInfo
	// recipes are read live: one written this session loads at once, while
	// the pinned prompt keeps the list it was built with
	for _, s := range h.modelSkills() {
		out = append(out, tools.SkillInfo{Name: s.Name, Path: s.Dir})
	}
	return out
}

// enabledSkills is the registry's enabled skills, then the project's.
func (h *Host) enabledSkills() []skill.Skill {
	enabled := slices.DeleteFunc(h.skillsReg.list(), func(s skill.Skill) bool { return !h.skillsReg.isEnabled(s.Name) })
	return append(enabled, h.projectOnlySkills()...)
}

// modelSkills is what the model may load: the enabled skills but the ones
// kept for the user (disable-model-invocation).
func (h *Host) modelSkills() []skill.Skill {
	return slices.DeleteFunc(h.enabledSkills(), func(s skill.Skill) bool { return s.DisableModelInvocation })
}

// recipesDir holds a project's recipes: skills private to this machine, kept
// outside the repository where no sandboxed command can write.
func (h *Host) recipesDir() string {
	if h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(projectState(h.Dirs.StateHome, h.trustRoot()), "skills")
}

// projectState is the repository's own state dir, next to its database: out
// of every repository and of every sandbox's write roots.
func projectState(stateHome, root string) string {
	return filepath.Dir(store.ProjectDBPath(stateHome, root))
}

func (h *Host) discoverRecipes() []skill.Skill {
	if !h.ProjectTrusted() || h.recipesDir() == "" {
		return nil
	}
	return h.discoverSkills(skill.Dir{Path: h.recipesDir(), Source: skill.Recipe})
}

// recipeAfterWrite checks a recipe the model just wrote and scrubs it where
// it lies. After the write, not before: the session already holds the text,
// and the ask showed the user the diff the model made.
func (h *Host) recipeAfterWrite(path string) string {
	if !h.inJail(path, h.recipesDir()) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	note := "\n\nrecipe: "
	clean := learn.ScrubShapes(sanitize.String(string(data)))
	switch {
	case learn.IsOneBlob(clean):
		os.Remove(path)
		return note + "removed, it was mostly secret material"
	case clean != string(data):
		if err := os.WriteFile(path, []byte(clean), 0o644); err != nil {
			return note + "could not be cleaned: " + err.Error()
		}
		var lines []int
		for i, l := range strings.Split(clean, "\n") {
			if strings.Contains(l, "[redacted ") {
				lines = append(lines, i+1)
			}
		}
		note += fmt.Sprintf("secret-shaped or control text was removed, redacted lines %v: name the env var or use a placeholder like $TOKEN, then re-check the commands; ", lines)
	}
	// a file beside SKILL.md is only cleaned; any case of the name is the
	// same file where the filesystem folds case
	if !strings.EqualFold(filepath.Base(path), "SKILL.md") {
		return strings.TrimSuffix(note, "\n\nrecipe: ")
	}
	sk, err := skill.Parse(filepath.Dir(path), skill.Recipe)
	loads := func(s skill.Skill) bool { return s.Source == skill.Recipe && s.Name == sk.Name }
	switch {
	case err != nil && !errors.Is(err, skill.ErrDescriptionCut):
		return note + "will not load: " + strings.TrimPrefix(err.Error(), "skills: ")
	case !slices.ContainsFunc(h.projectOnlySkills(), loads):
		return note + "will not load: another skill is named " + sk.Name + ", or the file is not <Recipes>/<name>/SKILL.md"
	case len(clean) > tools.SkillMaxOut:
		return note + fmt.Sprintf("over %d bytes, the skill tool cuts it: split it and link the parts by [[name]]", tools.SkillMaxOut)
	case err != nil:
		return note + strings.TrimPrefix(err.Error(), "skills: ")
	}
	return note + sk.Name + " saved; the skill tool loads it"
}

// discoverProjectSkills scans the repository's .agents/skills, which other
// agents share. They are body only: a zemi-skill.lua there never runs.
func (h *Host) discoverProjectSkills() []skill.Skill {
	pdir := h.Dirs.ProjectConfigDir()
	if pdir == "" {
		return nil
	}
	return h.discoverSkills(skill.Dir{Path: filepath.Join(pdir, "skills"), Source: skill.Project})
}

func (h *Host) discoverSkills(dirs ...skill.Dir) []skill.Skill {
	found, warns := skill.Discover(dirs...)
	for _, w := range warns {
		h.RecordErrorOnce("skills", strings.TrimPrefix(w, "skills: "))
	}
	return found
}

// projectOnlySkills is the project's skills and recipes whose names no
// user or builtin skill takes, enabled or not. Recipes are read live and
// come after the repository's skills: a recipe loses every name clash.
func (h *Host) projectOnlySkills() []skill.Skill {
	recipes := h.discoverRecipes()
	var all []skill.Skill
	if p := h.projectSkills.Load(); p != nil {
		all = slices.Clone(*p)
	}
	seen := map[string]bool{}
	// NOTE: every recipe is a line of every prompt; past a few dozen, rank
	// them with FTS over the descriptions.
	return slices.DeleteFunc(append(all, recipes...), func(s skill.Skill) bool {
		_, taken := h.skillsReg.get(s.Name)
		taken = taken || seen[s.Name]
		seen[s.Name] = true
		return taken
	})
}

// skillsIndexPrompt renders the §10.7 skills index section: one line per
// skill the model may load, description capped: the prompt carries the index
// only, bodies load on demand through the skill tool.
func (h *Host) skillsIndexPrompt() string {
	enabled := h.modelSkills()
	if len(enabled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Skills\nA description says when a skill applies; its body is the procedure. Before the first tool call of a task a skill covers, load that skill with the skill tool and follow it, even for a task you know how to do.\n")
	for _, s := range enabled {
		b.WriteString("- " + s.Name + ": " + s.Description + "\n")
	}
	return b.String()
}

// setSkillEnabled flips the stored enabled state; a hook already ran in
// the boot window, so it is not re-run here. The origin gate lives in the
// Lua namespace: Go callers run off the interpreter, where the frame origin
// is meaningless.
func (h *Host) setSkillEnabled(name string, enabled bool) error {
	if h.GlobalDB == nil {
		return errors.New("skills store unavailable")
	}
	if _, err := h.GlobalDB.GetSkill(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// project skills are not stored, so there is no flag to flip
			if slices.ContainsFunc(h.projectOnlySkills(), func(s skill.Skill) bool { return s.Name == name }) {
				return Usagef("skill %q belongs to this project and is on for as long as the project is trusted (/trust)", name)
			}
			return Usagef("no such skill %q", name)
		}
		return err
	}
	if err := h.GlobalDB.SetSkillEnabled(name, enabled); err != nil {
		return err
	}
	h.skillsReg.setEnabledFlag(name, enabled)
	h.includedStale.Store(true)
	if s, ok := h.skillsReg.get(name); ok && s.UserInvocable {
		// the markdown commands step builds /<name> or puts back the command the
		// skill displaced; booting, it has yet to run, and zemi run has no TUI
		// and runs no second command
		_ = h.ReloadConfig()
	}
	return nil
}

func skillFile(s skill.Skill) string { return filepath.Join(s.Dir, "SKILL.md") }

// hasSkillCommand reports whether /<name> is the skill's own command.
func (h *Host) hasSkillCommand(s skill.Skill) bool {
	c := h.Commands.Lookup(s.Name)
	return c != nil && c.Path == skillFile(s)
}

// addSkillCommand makes a skill that asks for it the command /<name>. It
// yields to a builtin command and to one of its own origin: a file in
// commands/ or a zemi.command.add says "I want /x", a flag in a skill an
// installer wrote does not. A less trusted holder is replaced.
func (h *Host) addSkillCommand(s skill.Skill, gen Gen) {
	origin := lua.Origin(s.Source)
	old := h.Commands.Lookup(s.Name)
	if old != nil && old.Path == skillFile(s) {
		return
	}
	if old == nil || (old.Origin != lua.OriginBuiltin && old.Origin != origin) {
		var err error
		if old, err = h.Commands.Add(&Command{
			Name: s.Name, NArgs: NArgsAny, Desc: strings.TrimSpace(listDesc(s.Description) + " " + s.ArgumentHint),
			Origin: origin, Path: skillFile(s), Gen: gen,
			GoFn: func(h *Host, args string) error {
				typed, stdin := agent.CutAttached(args)
				return h.sendSkill(s, strings.TrimSpace("/"+s.Name+" "+typed), strings.TrimSpace(typed), stdin)
			},
		}); err == nil {
			return
		}
	}
	h.RecordErrorOnce("skills", fmt.Sprintf("%s: /%s is already a %s command; /skills use %s sends the skill", s.Name, s.Name, old.Origin, s.Name))
}

// runSkillHook evaluates one skill's zemi-skill.lua inline (we are on the
// interpreter goroutine) as user origin: only user skills carry one.
func (h *Host) runSkillHook(ctx context.Context, L *glua.LState, s skill.Skill) error {
	if s.Hook == "" {
		return nil
	}
	if err := h.RT.EvalFileInline(ctx, L, s.Hook, lua.OriginUser); err != nil {
		// /errors and /health, like a dropped skill file: a dead hook is
		// silent otherwise, and the skill looks enabled
		h.RecordErrorOnce("skills", s.Name+": enable hook failed: "+err.Error())
		return err
	}
	h.Log.Info("skills", "enable hook ran", map[string]any{"skill": s.Name})
	return nil
}

// SkillsUserHooksStep runs the hooks of enabled skills in <config>/skills, after
// user init.lua and before the project config (DESIGN §5 load order); a
// failing hook is logged, never fatal.
func (h *Host) SkillsUserHooksStep() lua.BootStep {
	return lua.BootStep{
		Name: "user skill hooks",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.syncSkills()
			for _, s := range h.enabledSkills() {
				// installers write ~/.agents/skills: a hook there would run as user at every boot
				if s.Source == skill.User && filepath.Dir(s.Dir) == h.userSkillsDir() {
					_ = h.runSkillHook(ctx, g.L, s)
				}
			}
			return nil
		},
	}
}

// skillsCommand dispatches /skills [enable|disable <name> | use <name> [request]].
func (h *Host) skillsCommand(args string) error {
	typed, stdin := agent.CutAttached(args)
	fields := strings.Fields(typed)
	switch {
	case len(fields) == 0:
		return h.openSkills("")
	case len(fields) >= 2 && fields[0] == "use":
		// the request stays as typed, line breaks included
		request := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(typed), "use"))
		return h.useSkill(fields[1], strings.TrimSpace(strings.TrimPrefix(request, fields[1])), stdin)
	case len(fields) == 2 && (fields[0] == "enable" || fields[0] == "disable"):
		if err := h.setSkillEnabled(fields[1], fields[0] == "enable"); err != nil {
			return err
		}
		h.Notify(logging.Info, "Skill "+fields[0]+"d", fields[1])
		return nil
	case len(fields) == 2 && fields[0] == "toggle": // the /skills list, whose marks show the state
		return h.setSkillEnabled(fields[1], !h.skillsReg.isEnabled(fields[1]))
	case len(fields) == 2 && fields[0] == "open":
		return h.openSkillFile(fields[1])
	case (len(fields) == 2 || len(fields) == 3 && fields[2] == "--yes") && fields[0] == "delete":
		return h.deleteRecipe(fields[1], len(fields) == 3)
	default:
		return Usagef("usage: /skills | /skills enable|disable|open|delete <name> | /skills use <name> [request]")
	}
}

// deleteRecipe removes a recipe once yes confirms it (AskFirst) and drops
// the pinned prompt: the index moves once, by the user's own act. Every other
// source lives in files the user or the repository owns.
func (h *Host) deleteRecipe(name string, yes bool) error {
	for _, s := range h.projectOnlySkills() {
		if s.Name != name || s.Source != skill.Recipe {
			continue
		}
		if !yes {
			return AskFirst("skills delete "+name+" --yes", "", h.openSkills)
		}
		if err := os.RemoveAll(s.Dir); err != nil {
			return err
		}
		h.agents.mu.Lock()
		h.agents.dropPin()
		h.agents.mu.Unlock()
		h.includedStale.Store(true)
		h.Notify(logging.Info, "Recipe deleted", name)
		return nil
	}
	return Usagef("no recipe %q: only a recipe can be deleted here", name)
}

// openSkillFile opens a skill's SKILL.md in the user's editor, so its
// instructions can be read and fixed before it is enabled.
func (h *Host) openSkillFile(name string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		if s.Name != name {
			continue
		}
		path := filepath.Join(s.Dir, "SKILL.md")
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("skills open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: path, Title: "the skill"}); err != nil {
				h.Notify(logging.Error, "Could not open the skill", err.Error())
			}
		})
		return nil
	}
	return Usagef("no skill %q", name)
}

// useSkill sends the command line as typed with the skill behind
// agent.AttachMarker, which the transcript, titles, rewind and memory quotes
// cut off: the deterministic path when the model did not load it on its
// own. A disabled skill is reachable here: naming it is the request, so its
// description need not ride every prompt.
func (h *Host) useSkill(name, request, stdin string) error {
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		if s.Name == name {
			return h.sendSkill(s, strings.TrimSpace("/skills use "+name+" "+request), request, stdin)
		}
	}
	return Usagef("no skill %q", name)
}

// sendSkill sends line with the skill attached, $ARGUMENTS in it filled
// with the request; the model loading a skill on its own sees it as written.
func (h *Host) sendSkill(s skill.Skill, line, request, stdin string) error {
	text, err := tools.SkillText(s.Dir)
	if err != nil {
		return err
	}
	text = strings.ReplaceAll(text, "$ARGUMENTS", request)
	return h.AgentSend(line + agent.Attached("skill", "The user loaded the skill "+s.Name+" with the command above; follow it for their request.\n\n"+text) + stdin)
}

// openSkills lists discovered skills with their state; space toggles the
// selected one through the same enable/disable path as the command and the
// list stays open, enter opens its detail; confirm arms the action that runs
// it (Overlay.Confirm).
func (h *Host) openSkills(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(Overlay{
		ID: "skills", Title: "Skills", Origin: lua.OriginBuiltin, Items: h.skillItems(), BodyMD: true, Confirm: confirm,
		Reload: func() ([]any, [][]Segment) { return h.skillItems(), nil },
		Empty:  "No skills yet. Add one as skills/<name>/SKILL.md in " + TildePath(h.Dirs.ConfigHome) + " or " + TildePath(h.Dirs.AgentsHome) + ".",
	})
	return nil
}

// sourceOf names where a resource comes from, and which plugin ships it.
func (h *Host) sourceOf[S ~string](source S, path string) string {
	if n := h.PluginOf(path); n != "" {
		return "plugin:" + n
	}
	return string(source)
}

// sourceSection names a list section after where its rows come from.
func sourceSection[S ~string](source S) string {
	if source == "" {
		return ""
	}
	return strings.ToUpper(string(source[:1])) + string(source[1:])
}

// listDesc is a row's desc from a description written for the model: a
// fragment, lowercase and with no final period, like every list's.
func listDesc(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	r, n := utf8.DecodeRuneInString(s)
	if next, _ := utf8.DecodeRuneInString(s[n:]); n > 0 && !unicode.IsUpper(next) { // an acronym stays
		s = string(unicode.ToLower(r)) + s[n:]
	}
	return s
}

// sortBySource puts rows in the section order every list shares, the most
// trusted origin first: user, builtin, plugin, project, then a recipe,
// which no origin names.
func sortBySource[T any, S ~string](rows []T, source func(T) S) {
	slices.SortStableFunc(rows, func(a, b T) int {
		return cmp.Compare(lua.Origin(source(b)).Rank(), lua.Origin(source(a)).Rank())
	})
}

func (h *Host) skillItems() []any {
	all := append(h.skillsReg.list(), h.projectOnlySkills()...)
	sortBySource(all, func(s skill.Skill) skill.Source { return s.Source })
	items := make([]any, 0, len(all))
	for _, s := range all {
		toggle := "skills toggle " + s.Name
		item := map[string]any{"text": s.Name, "desc": listDesc(s.Description), "section": sourceSection(s.Source)}
		var actions []any
		state := "on"
		switch {
		case !s.Source.Stored(): // no state to toggle
			item["locked"] = true
		case h.skillsReg.isEnabled(s.Name):
			item["on"], item["run"] = true, toggle
			actions = append(actions, map[string]any{"text": "Disable", "hotkey": "d", "run": toggle})
		default:
			state, item["on"], item["run"] = "off", false, toggle
			actions = append(actions, map[string]any{"text": "Enable", "hotkey": "e", "run": toggle})
		}
		body, err := skill.Body(s.Dir)
		if err != nil {
			body = "could not read it: " + err.Error()
		}
		use := "/skills use " + s.Name + " "
		if h.hasSkillCommand(s) {
			use = "/" + s.Name + " "
		}
		item["actions"] = append(actions,
			map[string]any{"text": "Read it", "hotkey": "r", "detail": body},
			map[string]any{"text": "Open in the editor", "hotkey": "o", "run": "skills open " + s.Name},
			map[string]any{"text": "Use now", "hotkey": "u", "fill": use},
		)
		if s.Source == skill.Recipe {
			item["actions"] = append(item["actions"].([]any), map[string]any{"text": "Delete", "hotkey": "x", "danger": true, "run": "skills delete " + s.Name + " --yes"})
		}
		item["fields"] = fieldList("source", h.sourceOf(s.Source, s.Dir), "status", state, "used by", h.usedBy(s, state == "on"), "path", TildePath(skillFile(s)))
		item["detail"] = s.Description
		items = append(items, item)
	}
	return items
}

// usedBy says who runs a skill: the model through its index, the user
// through its command.
func (h *Host) usedBy(s skill.Skill, on bool) string {
	var who []string
	if on && !s.DisableModelInvocation {
		who = append(who, "the model")
	}
	if h.hasSkillCommand(s) {
		who = append(who, "/"+s.Name)
	}
	out := cmp.Or(strings.Join(who, " and "), "/skills use only")
	if on && s.UserInvocable && !h.hasSkillCommand(s) {
		out += "; /" + s.Name + " is another command"
	}
	return out
}

// skillsCommands is /skills.
func (h *Host) skillsCommands() []*Command {
	return []*Command{{
		Name: "skills", NArgs: NArgsAny, Desc: "turn skills on or off, or use one",
		GoFn: func(h *Host, args string) error { return h.skillsCommand(args) },
		GoComplete: SubcommandCompleter([]string{"enable", "disable", "use", "open", "delete"}, func(h *Host, _ string) []string {
			var names []string
			for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
				names = append(names, s.Name)
			}
			return names
		}),
	}}
}

// ---------------------------------------------------------------------------
// zemi.skills namespace

func (h *Host) skillsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
			rows = append(rows, map[string]any{
				"name": s.Name, "description": s.Description, "source": string(s.Source),
				"enabled": !s.Source.Stored() || h.skillsReg.isEnabled(s.Name), "path": s.Dir,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	enable := func(L *glua.LState, on bool) int {
		name := L.CheckString(1)
		verb := "enable"
		if !on {
			verb = "disable"
		}
		h.refuseBelowUser(L, "skills."+verb)
		return luaResult(L, h.setSkillEnabled(name, on))
	}
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int { return enable(L, true) }))
	tb.RawSetString("disable", L.NewFunction(func(L *glua.LState) int { return enable(L, false) }))

	return tb
}
