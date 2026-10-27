// skills.go is the skills host API (DESIGN §6.7, §10.5): discovery
// synced against the skills table, the zemi.skills namespace (enable is
// open to any origin: the enabled set is the user's to change), the enable
// hooks (user skills run in the boot window after user init), and the
// /skills command.

package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
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
	// the embedded tree never changes within a process: write it once,
	// and retry when the write failed
	if !h.builtinSkillsOK.Load() {
		if err := h.materializeBuiltinSkills(); err != nil {
			h.Log.Warn("builtin skills not written", "origin", "skills", "error", err)
		} else {
			h.builtinSkillsOK.Store(true)
		}
	}
	dirs := []skill.Dir{
		{Path: h.userSkillsDir(), Source: skill.User},
		{Path: h.agentsSkillsDir(), Source: skill.User},
		{Path: h.builtinSkillsDir(), Source: skill.Builtin},
	}
	var commands []skill.Skill
	for _, p := range h.pluginDirs() {
		dirs = append(dirs, skill.Dir{Path: filepath.Join(p, "skills"), Source: skill.Plugin})
		if !h.isClaudePlugin(p) {
			continue
		}
		// a Claude-layout plugin's skills keep their bare name unless another
		// skill holds it and its bodies carry the plugin vars; its commands
		// load as model-invocable rows too
		d := &dirs[len(dirs)-1]
		d.Prefix, d.Root, d.Data = h.pluginPrefix(p), p, h.pluginDataDir(h.PluginOf(p))
		commands = append(commands, h.claudeCommandSkills(p, d.Data)...)
	}
	found := h.discoverSkills(dirs...)
	for _, s := range found {
		if s.Root == "" {
			continue
		}
		if body, err := s.Body(); err == nil && strings.Contains(body, "!`") {
			h.Log.Warn("skill body has !`cmd` shell blocks, they stay literal text", "origin", "skills", "skill", s.Name)
		}
	}
	// the command rows go through the same seen-set as Discover: on a clash
	// the bare name retries the plugin-prefixed one, then the earlier row
	// wins, loudly
	seen := make(map[string]string, len(found))
	for _, s := range found {
		seen[s.Name] = s.Dir
	}
	for _, s := range commands {
		if _, clash := seen[s.Name]; !clash {
			seen[s.Name] = s.Dir
			found = append(found, s)
			continue
		}
		pre := h.pluginPrefix(s.Root) + s.Name
		if _, taken := seen[pre]; taken || !skill.ValidName(pre) {
			h.RecordErrorOnce("skills", "", fmt.Sprintf("%s: skill %s is already in %s", s.File, s.Name, seen[s.Name]))
			continue
		}
		h.Log.Warn(fmt.Sprintf("skill %s is already in %s, loaded as %s", s.Name, seen[s.Name], pre), "origin", "skills", "file", s.File)
		s.Name = pre
		seen[pre] = s.Dir
		found = append(found, s)
	}
	rows := make([]*store.Skill, 0, len(found))
	for _, s := range found {
		rows = append(rows, &store.Skill{Name: s.Name, Path: s.Dir, Source: string(s.Source)})
	}
	if err := h.GlobalDB.SyncSkills(rows); err != nil {
		h.Log.Error("sync failed", "origin", "skills", "error", err)
		return
	}
	stored, err := h.GlobalDB.ListSkills()
	if err != nil {
		h.Log.Error("list failed", "origin", "skills", "error", err)
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

// zemiSkillDir is where a ready prompt sends the model for the zemi skill's
// files: the read tool opens them with no ask, also when /skills switched the
// skill off.
func (h *Host) zemiSkillDir() string { return filepath.Join(h.builtinSkillsDir(), "zemi") }

// materializeBuiltinSkills rewrites the builtin skills once per process, so
// an upgraded zemi replaces them and drops the ones it no longer ships; the
// sync retries while the write fails.
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

// zemiCLI names this binary at the end of every builtin skill, read once:
// replaced in place, Linux would name it "(deleted)". With no path it says
// nothing.
var zemiCLI = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return "\n\nThis zemi runs from " + exe + ": use that path for its CLI, since another zemi on PATH may be older."
}()

// skillNote is what only this process knows, put after a skill's body: a
// builtin skill's steps call the zemi CLI, and another one on PATH may be older.
func skillNote(s skill.Skill) string {
	if s.Source == skill.Builtin {
		return zemiCLI
	}
	return ""
}

// skillIndex is what the skill tool may load: every enabled skill.
func (h *Host) skillIndex() []tools.SkillInfo {
	var out []tools.SkillInfo
	// recipes are read live: one written this session loads at once, while
	// the pinned prompt keeps the list it was built with
	for _, s := range h.enabledSkills() {
		out = append(out, tools.SkillInfo{Name: s.Name, Path: s.Dir, File: s.File, Root: s.Root, Data: s.Data, Note: skillNote(s)})
	}
	return out
}

// SkillMentions lists every skill for the + popup: name and description,
// disabled ones included, since naming one is the request.
func (h *Host) SkillMentions() []CompletionItem {
	out := make([]CompletionItem, 0, 8)
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		desc := s.Description
		if s.Root != "" {
			desc = plugin.ExpandRootData(desc, s.Root, s.Data)
		}
		out = append(out, CompletionItem{Text: s.Name, Desc: listDesc(desc)})
	}
	return out
}

// enabledSkills is the registry's enabled skills, then the project's.
func (h *Host) enabledSkills() []skill.Skill {
	enabled := slices.DeleteFunc(h.skillsReg.list(), func(s skill.Skill) bool { return !h.skillsReg.isEnabled(s.Name) })
	return append(enabled, h.projectOnlySkills()...)
}

// recipesDir holds a project's recipes: skills private to this machine, kept
// outside the repository.
func (h *Host) recipesDir() string {
	if h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(projectState(h.Dirs.StateHome, h.trustRoot()), "skills")
}

// projectState is the repository's own state dir, next to its database: out
// of every repository and of every job's working directory.
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
	if !h.inRoots(path, h.recipesDir()) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	note := "\n\nrecipe: "
	clean := sanitize.ScrubShapes(sanitize.String(string(data)))
	switch {
	case sanitize.IsOneBlob(clean):
		os.Remove(path)
		return note + "removed, it was mostly secret material"
	case clean != string(data):
		if err := os.WriteFile(path, []byte(clean), 0o644); err != nil {
			return note + "could not be cleaned: " + err.Error()
		}
		note += fmt.Sprintf("secret-shaped or control text was removed, redacted lines %v: name the env var or use a placeholder like $TOKEN, then re-check the commands; ", redactedLines(clean))
	}
	// a file beside SKILL.md is only cleaned; any case of the name is the
	// same file where the filesystem folds case
	if !strings.EqualFold(filepath.Base(path), "SKILL.md") {
		return strings.TrimSuffix(note, "\n\nrecipe: ")
	}
	sk, err := skill.Parse(filepath.Dir(path), skill.Recipe, "")
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
		if errors.Is(w, skill.ErrDescriptionCut) {
			// the /skills row and the index show the cut text already: a log
			// line, not an error row
			h.Log.Warn(strings.TrimPrefix(w.Error(), "skills: "), "origin", "skills")
			continue
		}
		h.RecordErrorOnce("skills", "", strings.TrimPrefix(w.Error(), "skills: "))
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
	// NOTE: every recipe is a line of every prompt; past a few dozen, rank
	// them with FTS over the descriptions.
	return dedupeByName(append(all, recipes...), func(s skill.Skill) string { return s.Name }, func(n string) bool {
		_, taken := h.skillsReg.get(n)
		return taken
	})
}

// skillDescMax caps one skill index line: one marketplace skill's prose
// rides every prompt of every session, and the body loads whole on demand.
const skillDescMax = 240

// skillsIndexPrompt renders the §10.7 skills index: one line per skill the
// model may load, description capped at skillDescMax: the prompt carries the
// index only, bodies load on demand through the skill tool. The project's
// skills and recipes go in project, for the context message, so the system
// prompt stays the same in every project.
func (h *Host) skillsIndexPrompt() (mine, project string) {
	var lines [2]strings.Builder
	for _, s := range h.enabledSkills() {
		i := 0
		if !s.Source.Stored() {
			i = 1
		}
		desc := s.Description
		if s.Root != "" { // the fallback description is the first body line
			desc = plugin.ExpandRootData(desc, s.Root, s.Data)
		}
		desc = truncRunes(desc, skillDescMax)
		lines[i].WriteString("- " + s.Name + ": " + desc + "\n")
	}
	rule := "A description says when a skill applies; its body is the procedure. When a task matches one, load that skill with the skill tool before the first tool call and follow it, even for a task you know how to do; when none matches, load none.\n"
	if lines[0].Len() > 0 {
		mine, rule = "\n\n# Skills\n"+rule+lines[0].String(), "Loaded and followed like the skills in the system prompt.\n"
	}
	if lines[1].Len() > 0 {
		project = "\n\n# Project skills\n" + rule + lines[1].String()
	}
	return mine, project
}

// setSkillEnabled flips the stored enabled state; a hook already ran in
// the boot window, so it is not re-run here.
func (h *Host) setSkillEnabled(name string, enabled bool) error {
	if h.GlobalDB == nil {
		return errors.New("skills store unavailable")
	}
	if err := h.GlobalDB.SetSkillEnabled(name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// project skills are not stored, so there is no flag to flip
			if slices.ContainsFunc(h.projectOnlySkills(), func(s skill.Skill) bool { return s.Name == name }) {
				return Usagef("skill %q belongs to this project and is on for as long as the project is trusted (%s)", name, "/trust")
			}
			return Usagef("no such skill %q", name)
		}
		return err
	}
	h.skillsReg.setEnabledFlag(name, enabled)
	return nil
}

func skillFile(s skill.Skill) string {
	return cmp.Or(s.File, filepath.Join(s.Dir, "SKILL.md"))
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
		h.RecordErrorOnce("skills", "", s.Name+": enable hook failed: "+err.Error())
		return err
	}
	h.Log.Info("enable hook ran", "origin", "skills", "skill", s.Name)
	return nil
}

// SkillsUserHooksStep runs the hooks of enabled skills in <config>/skills, after
// user init.lua (DESIGN §5 load order); a
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

// skillsCommand dispatches /skills [enable|disable <name>]; confirm (an
// armed danger key's press) runs a delete without its --yes.
func (h *Host) skillsCommand(args string, confirm bool) error {
	typed, _ := agent.CutAttached(args)
	rest, yes := ConfirmArgs(typed, confirm)
	fields := strings.Fields(rest)
	switch {
	case len(fields) == 0:
		return h.openSkills("")
	case len(fields) == 2 && (fields[0] == "enable" || fields[0] == "disable"):
		if err := h.setSkillEnabled(fields[1], fields[0] == "enable"); err != nil {
			return err
		}
		h.Notify(slog.LevelInfo, "Skill "+fields[0]+"d", fields[1])
		return nil
	case len(fields) == 2 && fields[0] == "toggle": // the /skills list, whose marks show the state
		return h.setSkillEnabled(fields[1], !h.skillsReg.isEnabled(fields[1]))
	case len(fields) == 2 && fields[0] == "open":
		return h.openSkillFile(fields[1])
	case len(fields) == 2 && fields[0] == "delete":
		return h.deleteRecipe(fields[1], yes)
	default:
		return Usagef("usage: /skills | /skills enable|disable|open|delete <name>")
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
		h.Notify(slog.LevelInfo, "Recipe deleted", name)
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
	s, ok := h.skillByName(name)
	if !ok {
		return Usagef("no such skill %q", name)
	}
	if s.Source == skill.Builtin {
		return Usagef("zemi rewrites the %s skill on every start, so there is no file to edit; its /skills row shows the text", name)
	}
	h.openInEditor(ui, skillFile(s), "the skill")
	return nil
}

// skillByName finds a skill in the registry or the project's, disabled ones
// included.
func (h *Host) skillByName(name string) (skill.Skill, bool) {
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		if s.Name == name {
			return s, true
		}
	}
	return skill.Skill{}, false
}

// skillBase is the name a skill keeps before a collision prefixed it: the
// dir's, or a command row's file stem, with the plugin that ships it.
func (h *Host) skillBase(s skill.Skill) (base, plugin string) {
	base = filepath.Base(s.Dir)
	if s.File != "" {
		base = strings.TrimSuffix(filepath.Base(s.File), ".md")
	}
	return base, h.PluginOf(s.Dir)
}

// attachSkill puts a +name skill behind the marker: the body as written, the
// note pointing at the typed message for the request. A name nothing offers
// stays prose, like an @ word naming no file.
func (h *Host) attachSkill(b *strings.Builder, name string) {
	s, ok := h.skillByName(name)
	if !ok {
		return
	}
	text, err := tools.SkillText(tools.SkillInfo{Name: s.Name, Path: s.Dir, File: s.File, Root: s.Root, Data: s.Data})
	if err != nil {
		return // the body is gone since it was typed: the token stays prose
	}
	note := fmt.Sprintf("The user attached this skill with +%s in the message above; follow it for their request.\n\n", s.Name)
	if base, by := h.skillBase(s); base != s.Name {
		note = fmt.Sprintf("The user attached this skill with +%s (%s from the %s plugin) in the message above; follow it for their request.\n\n", s.Name, base, by)
	}
	b.WriteString(agent.AttachElement(fmt.Sprintf("skill name=%q", s.Name), note+text+skillNote(s)))
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
		Reload: h.skillItems,
		Empty:  "No skills yet. Add one as skills/<name>/SKILL.md in " + perm.TildePath(h.Dirs.ConfigHome) + " or " + perm.TildePath(h.Dirs.AgentsHome) + ".",
	})
	return nil
}

// sourceOf names where a resource comes from, and which plugin ships it.
func (h *Host) sourceOf[S ~string](source S, path string) string {
	if n := h.PluginOf(path); n != "" {
		return perm.PluginPrefix + n
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
	return LowerFirst(s)
}

// LowerFirst lowercases a string's first rune unless a second upper-case one
// follows: an acronym's word stays whole.
func LowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if next, _ := utf8.DecodeRuneInString(s[n:]); n > 0 && !unicode.IsUpper(next) {
		s = string(unicode.ToLower(r)) + s[n:]
	}
	return s
}

// sourceOrder is the section order every list shares: user, builtin, plugin,
// project, then a recipe, which no origin names.
var sourceOrder = []lua.Origin{lua.OriginUser, lua.OriginBuiltin, lua.OriginPlugin, lua.OriginProject}

// sortBySource puts rows in that order.
func sortBySource[T any, S ~string](rows []T, source func(T) S) {
	pos := func(s S) int {
		if i := slices.Index(sourceOrder, lua.Origin(s)); i >= 0 {
			return i
		}
		return len(sourceOrder)
	}
	slices.SortStableFunc(rows, func(a, b T) int {
		return cmp.Compare(pos(source(a)), pos(source(b)))
	})
}

// dedupeByName keeps the first row of each name, over rows already in section
// order: the more trusted origin won the name. taken, when given, also drops a
// name something outside the list holds.
func dedupeByName[T any](rows []T, name func(T) string, taken func(string) bool) []T {
	seen := set.Set[string]{}
	return slices.DeleteFunc(rows, func(r T) bool {
		n := name(r)
		dup := seen.Has(n) || taken != nil && taken(n)
		seen.Add(n)
		return dup
	})
}

// untrustedNote is why a project file did not load: one sentence for every
// layered resource, the caller adding its own way out.
func (h *Host) untrustedNote(kind, name, dir string) string {
	return fmt.Sprintf("%s %q is in %s, and this project is not trusted: the user runs /trust to load it", kind, name, dir)
}

func (h *Host) skillItems() []OverlayItem {
	all := append(h.skillsReg.list(), h.projectOnlySkills()...)
	sortBySource(all, func(s skill.Skill) skill.Source { return s.Source })
	items := make([]OverlayItem, 0, len(all))
	for _, s := range all {
		toggle := "skills toggle " + s.Name
		item := OverlayItem{Text: s.Name, Desc: listDesc(s.Description), Section: sourceSection(s.Source)}
		if base, by := h.skillBase(s); base != s.Name {
			item.Text = base + "@" + by
		}
		var keys []ItemKey
		state := "on"
		switch {
		case !s.Source.Stored(): // no state to toggle
			item.Locked = new(true)
		case h.skillsReg.isEnabled(s.Name):
			item.On, item.Run = new(true), toggle
			keys = append(keys, ItemKey{Key: "d", Desc: "disable", Run: toggle})
		default:
			state, item.On, item.Run = "off", new(false), toggle
			keys = append(keys, ItemKey{Key: "e", Desc: "enable", Run: toggle})
		}
		body, err := s.Body()
		if err != nil {
			body = "could not read it: " + err.Error()
		}
		if s.Source != skill.Builtin { // zemi rewrites its own on every start
			keys = append(keys, ItemKey{Key: "o", Desc: openInEditorLabel, Run: "skills open " + s.Name})
		}
		item.Keys = keys
		if s.Source == skill.Recipe {
			item.Keys = append(item.Keys, ItemKey{Key: "x", Desc: "delete", Danger: true, Run: "skills delete " + s.Name})
		}
		item.Fields = fieldList("source", h.sourceOf(s.Source, s.Dir), "status", state, "used by", h.usedBy(s, state == "on"), "path", perm.TildePath(skillFile(s)))
		item.Detail = body // enter reads the skill itself: its description is the row's desc
		items = append(items, item)
	}
	return items
}

// usedBy says who runs a skill: the model while it is on, the user's
// +name token when it is off.
func (h *Host) usedBy(s skill.Skill, on bool) string {
	if on {
		return "the model"
	}
	return "+" + s.Name + " only"
}

// skillsCommands is /skills.
func (h *Host) skillsCommands() []*Command {
	return []*Command{{
		Name: "skills", NArgs: NArgsAny, Desc: "turn skills on or off",
		GoFn: func(h *Host, args string, confirm bool) error { return h.skillsCommand(args, confirm) },
		GoComplete: SubcommandCompleter([]string{"enable", "disable", "open", "delete"}, func(h *Host, sub string) []string {
			var names []string
			for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
				if !(sub == "open" && s.Source == skill.Builtin) && !(sub == "delete" && s.Source != skill.Recipe) {
					names = append(names, s.Name)
				}
			}
			return names
		}),
	}}
}

// zemi.skills namespace

func (h *Host) skillsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
			rows = append(rows, map[string]any{
				"name": s.Name, "description": s.Description, "source": string(s.Source),
				"enabled": !s.Source.Stored() || h.skillsReg.isEnabled(s.Name), "path": s.Dir,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	// enable(name, on), like zemi.mcp.enable
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int {
		return luaResult(L, h.setSkillEnabled(L.CheckString(1), L.CheckBool(2)))
	}))

	return tb
}
