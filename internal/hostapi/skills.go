// skills.go is the skills host API (DESIGN §6.7, §10.5): discovery
// synced against the skills table, the zemi.skills namespace, the enable
// hooks (authored skills run in the boot window after user init; learned
// skills run post-ready, only when enabled), and the /skills command.
// Enable/disable require a user or builtin origin: the model may propose,
// a plugin-registered keymap cannot flip the quarantine.
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

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
	glua "github.com/yuin/gopher-lua"
)

// skillsReg caches discovery results between DB syncs. Everything that
// touches it runs on the interpreter goroutine except the skill tool's
// index read (agent goroutine), hence the mutex.
type skillReg struct {
	mu      sync.RWMutex
	byName  map[string]skills.Skill
	enabled map[string]bool
}

func (r *skillReg) set(all []skills.Skill, enabled map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = make(map[string]skills.Skill, len(all))
	for _, s := range all {
		r.byName[s.Name] = s
	}
	r.enabled = enabled
}

func (r *skillReg) list() []skills.Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.byName), func(a, b skills.Skill) int { return cmp.Compare(a.Name, b.Name) })
}

func (r *skillReg) isEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled[name]
}

func (r *skillReg) get(name string) (skills.Skill, bool) {
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
// (DESIGN §10.5): authored skills default enabled, learned start
// disabled; an existing row keeps its enabled choice. Invalid skill dirs
// are logged and skipped, never fatal.
func (h *Host) syncSkills() {
	if h.GlobalDB == nil {
		return
	}
	if err := h.materializeBuiltinSkills(); err != nil {
		h.Log.Warn("skills", "builtin skills not written", map[string]any{"error": err.Error()})
	}
	found, warns := skills.Discover(
		skills.Dir{Path: h.authoredSkillsDir(), Source: skills.Authored},
		skills.Dir{Path: h.builtinSkillsDir(), Source: skills.Builtin},
		skills.Dir{Path: h.learnedSkillsDir(), Source: skills.Learned})
	for _, w := range warns {
		h.RecordErrorOnce("skills", strings.TrimPrefix(w, "skills: "))
	}
	rows := make([]*store.Skill, 0, len(found))
	for _, s := range found {
		rows = append(rows, &store.Skill{
			Name:           s.Name,
			Path:           s.Dir,
			Source:         string(s.Source),
			DefaultEnabled: s.Source != skills.Learned,
		})
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

// draftLearnedSkill writes one reflector-proposed skill under the data
// dir's skills/ (DESIGN §10.5): body only, never a Lua hook, text through
// the memory scrubber, and disabled like every learned skill until the
// user enables it in /skills.
func (h *Host) draftLearnedSkill(sid, name, desc, body string) error {
	dir := h.learnedSkillsDir()
	switch {
	case dir == "":
		return errors.New("no data dir for learned skills")
	case !skills.ValidName(name):
		return fmt.Errorf("invalid skill name %q", name)
	case strings.TrimSpace(desc) == "" || strings.TrimSpace(body) == "":
		return errors.New("description and body are required")
	case len([]rune(desc)) > skillIndexDescMax:
		return fmt.Errorf("description over %d characters", skillIndexDescMax)
	}
	path := filepath.Join(dir, name)
	if _, taken := h.skillsReg.get(name); taken {
		return fmt.Errorf("skill %q already exists", name)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("skill %q already exists", name)
	}
	desc, _ = learn.ScrubSecrets(sanitize.Line(desc))
	body, scrubbed := learn.ScrubSecrets(sanitize.String(body))
	if scrubbed && learn.IsOneBlob(body) {
		return errors.New("draft is mostly secret material")
	}
	fm, err := yaml.Marshal(map[string]string{"name": name, "description": desc})
	if err != nil {
		return err
	}
	doc := "---\n" + string(fm) + "---\n\n" + strings.TrimSpace(body) +
		"\n\n<!-- drafted by the reflector from session " + sid + "; review before enabling -->\n"
	if len(doc) > skills.MaxFileBytes {
		return errors.New("draft too large")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(doc), 0o644)
}

// joinIf joins elem under dir, or is "" when dir is unset.
func joinIf(dir, elem string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, elem)
}

func (h *Host) authoredSkillsDir() string { return joinIf(h.Dirs.ConfigHome, "skills") }

// builtinSkillsDir is where the embedded builtin skills are copied so the
// skill tool reads them from disk like any other skill.
func (h *Host) builtinSkillsDir() string { return joinIf(h.Dirs.DataHome, "builtin-skills") }

// materializeBuiltinSkills rewrites the builtin skills on every sync, so an
// upgraded zemi replaces them.
func (h *Host) materializeBuiltinSkills() error {
	dir := h.builtinSkillsDir()
	if dir == "" {
		return nil
	}
	return fs.WalkDir(zemiruntime.Skills, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := zemiruntime.Skills.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, strings.TrimPrefix(p, "skills/"))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

func (h *Host) learnedSkillsDir() string { return joinIf(h.Dirs.DataHome, "skills") }

// skillIndex resolves enabled skills for the skill tool (DESIGN §10.5:
// discovery lists only enabled skills to the model).
func (h *Host) skillIndex() []tools.SkillInfo {
	var out []tools.SkillInfo
	for _, s := range h.enabledSkills() {
		out = append(out, tools.SkillInfo{Name: s.Name, Path: s.Dir})
	}
	return out
}

// enabledSkills is the registry's enabled skills, then the project's.
func (h *Host) enabledSkills() []skills.Skill {
	enabled := slices.DeleteFunc(h.skillsReg.list(), func(s skills.Skill) bool { return !h.skillsReg.isEnabled(s.Name) })
	return append(enabled, h.projectOnlySkills()...)
}

// discoverProjectSkills scans the repository's skill dirs, which other
// agents share. They are body only: a zemi-skill.lua there never runs.
func (h *Host) discoverProjectSkills() []skills.Skill {
	root := h.Dirs.RepoRoot()
	if root == "" {
		return nil
	}
	var dirs []skills.Dir
	for _, d := range []string{".zemi", ".agents", ".claude"} {
		dirs = append(dirs, skills.Dir{Path: filepath.Join(root, d, "skills"), Source: skills.Project})
	}
	found, warns := skills.Discover(dirs...)
	for _, w := range warns {
		h.RecordErrorOnce("skills", strings.TrimPrefix(w, "skills: "))
	}
	return found
}

// projectOnlySkills is the project's skills whose names no authored, builtin
// or learned skill takes, enabled or not.
func (h *Host) projectOnlySkills() []skills.Skill {
	p := h.projectSkills.Load()
	if p == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(*p), func(s skills.Skill) bool {
		_, taken := h.skillsReg.get(s.Name)
		return taken
	})
}

// skillsIndexPrompt renders the §10.7 skills index section: one line per
// enabled skill, description capped — the prompt carries the index only,
// bodies load on demand through the skill tool.
func (h *Host) skillsIndexPrompt() string {
	enabled := h.enabledSkills()
	if len(enabled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Skills\nEach description is a trigger, not a summary: the procedure is in the body. Before the first tool call that does the task, check this list. If one covers the task, load it with the skill tool and follow it, even for a task you know how to do.\n")
	for _, s := range enabled {
		b.WriteString("- " + s.Name + ": " + s.Description + "\n")
	}
	return b.String()
}

// skillIndexDescMax bounds a learned draft's description like discovery
// bounds an authored one, so the index never clips.
const skillIndexDescMax = skills.MaxDescriptionRunes

// setSkillEnabled flips the stored enabled state and runs the enable
// hook of a learned skill being enabled (post-ready loading, §10.5);
// authored hooks already ran in the boot window, so they are not re-run
// here. The origin gate lives in the Lua namespace: Go callers run off the
// interpreter, where the frame origin is meaningless.
func (h *Host) setSkillEnabled(L *glua.LState, name string, enabled bool) error {
	if h.GlobalDB == nil {
		return errors.New("skills store unavailable")
	}
	if _, err := h.GlobalDB.GetSkill(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// project skills are not stored, so there is no flag to flip
			if slices.ContainsFunc(h.projectOnlySkills(), func(s skills.Skill) bool { return s.Name == name }) {
				return fmt.Errorf("skill %q belongs to this project and is on for as long as the project is trusted (/trust)", name)
			}
			return fmt.Errorf("no such skill %q", name)
		}
		return err
	}
	if err := h.GlobalDB.SetSkillEnabled(name, enabled); err != nil {
		return err
	}
	h.skillsReg.setEnabledFlag(name, enabled)
	if enabled {
		if s, ok := h.skillsReg.get(name); ok && s.Source == skills.Learned {
			return h.enableHook(L, s)
		}
	}
	return nil
}

// enableHook runs a learned skill's enable hook. L non-nil means we are
// on the interpreter goroutine (zemi.skills.enable) and the hook runs
// inline; L nil means a Go-side caller (/skills enable) and the hook must
// be enqueued through Do — the single-writer invariant forbids touching
// the LState from here.
func (h *Host) enableHook(L *glua.LState, s skills.Skill) error {
	var err error
	if L != nil {
		err = h.runSkillHook(L, s)
	} else {
		_, err = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			return nil, h.runSkillHook(L, s)
		})
	}
	if err != nil {
		return fmt.Errorf("enable hook failed: %w", err)
	}
	return nil
}

// runSkillHook evaluates one skill's zemi-skill.lua inline (we are on the
// interpreter goroutine); the origin comes from the path classification,
// so learned hooks run under the learned denylist (no tool registration).
func (h *Host) runSkillHook(L *glua.LState, s skills.Skill) error {
	if s.Hook == "" {
		return nil
	}
	origin := lua.OriginUser
	if s.Source == skills.Learned {
		origin = lua.OriginLearned
	}
	if err := h.RT.EvalFileInline(L, s.Hook, origin); err != nil {
		// /errors and /health, like a dropped skill file: a dead hook is
		// silent otherwise, and the skill looks enabled
		h.RecordErrorOnce("skills", s.Name+": enable hook failed: "+err.Error())
		return err
	}
	h.Log.Info("skills", "enable hook ran", map[string]any{"skill": s.Name})
	return nil
}

// SkillsAuthoredHooksStep runs the hooks of enabled authored skills, after
// user init.lua and before the project config (DESIGN §5 load order).
func (h *Host) SkillsAuthoredHooksStep() lua.BootStep {
	return h.skillHooksStep("authored skill hooks", skills.Authored)
}

// SkillsLearnedHooksStep runs the hooks of enabled learned skills post-ready
// (§5).
func (h *Host) SkillsLearnedHooksStep() lua.BootStep {
	return h.skillHooksStep("learned skill hooks", skills.Learned)
}

// skillHooksStep runs the enable hooks of enabled skills from src; a failing
// hook is logged, never fatal.
func (h *Host) skillHooksStep(name string, src skills.Source) lua.BootStep {
	return lua.BootStep{
		Name: name,
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.syncSkills()
			for _, s := range h.enabledSkills() {
				if s.Source == src {
					_ = h.runSkillHook(g.L, s)
				}
			}
			return nil
		},
	}
}

// skillsCommand dispatches /skills [enable|disable <name> | use <name> [request]].
func (h *Host) skillsCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		h.openSkills()
		return nil
	case len(fields) >= 2 && fields[0] == "use":
		return h.useSkill(fields[1], strings.Join(fields[2:], " "))
	case len(fields) == 2 && (fields[0] == "enable" || fields[0] == "disable"):
		if err := h.setSkillEnabled(nil, fields[1], fields[0] == "enable"); err != nil {
			return err
		}
		h.Notify("info", "Skills", fields[0]+"d "+fields[1])
		return nil
	case len(fields) == 2 && fields[0] == "toggle": // the /skills list, whose marks show the state
		return h.setSkillEnabled(nil, fields[1], !h.skillsReg.isEnabled(fields[1]))
	case len(fields) == 2 && fields[0] == "open":
		return h.openSkillFile(fields[1])
	default:
		return errors.New("usage: /skills | /skills enable|disable|open <name> | /skills use <name> [request]")
	}
}

// openSkillFile opens a skill's SKILL.md in the user's editor, so its
// instructions can be read and fixed before it is enabled.
func (h *Host) openSkillFile(name string) error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		if s.Name != name {
			continue
		}
		path := filepath.Join(s.Dir, "SKILL.md")
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("skills open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: path, Title: "the skill"}); err != nil {
				h.Notify("warn", "Skill not opened", err.Error())
			}
		})
		return nil
	}
	return fmt.Errorf("no skill %q", name)
}

// useSkill sends a skill's body as the next user message, the
// deterministic path when the model did not load it on its own; request
// is the task that follows it. A disabled skill is reachable here: naming
// it is the request, so its description need not ride every prompt. Only
// a learned draft keeps its quarantine (DESIGN §10.5).
func (h *Host) useSkill(name, request string) error {
	for _, s := range append(h.skillsReg.list(), h.projectOnlySkills()...) {
		if s.Name != name {
			continue
		}
		if s.Source == skills.Learned && !h.skillsReg.isEnabled(name) {
			return fmt.Errorf("skill %q is an unreviewed draft: /skills open %s, then enable it", name, name)
		}
		msg, err := skillMessage(s, request)
		if err != nil {
			return err
		}
		return h.AgentSend(msg)
	}
	return fmt.Errorf("no skill %q", name)
}

// skillMessage is the user message /skills use sends: the body first, so
// the model reads the procedure before the task it applies to.
func skillMessage(s skills.Skill, request string) (string, error) {
	body, err := skills.Body(s.Dir)
	if err != nil {
		return "", err
	}
	msg := "Follow this skill for the request below.\n<loaded_skill name=\"" + s.Name + "\">\nSkill directory: " + s.Dir + "\n\n" + body + "\n</loaded_skill>"
	if request != "" {
		msg += "\n\n" + request
	}
	return msg, nil
}

// openSkills lists discovered skills with their state; space toggles the
// selected one through the same enable/disable path as the command and the
// list stays open, enter opens its detail.
func (h *Host) openSkills() {
	if ui := h.hooks(); ui != nil {
		ui.OpenOverlay(Overlay{ID: "skills", Title: "Skills", Origin: lua.OriginBuiltin, Items: h.skillItems(),
			Reload: func() ([]any, [][]Segment) { return h.skillItems(), nil },
			Empty:  "No skills found. Add one as skills/<name>/SKILL.md in " + TildePath(h.Dirs.ConfigHome) + "."})
	}
}

// sourceSection names a list section after where its rows come from.
func sourceSection(source string) string {
	if source == "" {
		return ""
	}
	return strings.ToUpper(source[:1]) + source[1:]
}

func (h *Host) skillItems() []any {
	all := append(h.skillsReg.list(), h.projectOnlySkills()...)
	slices.SortStableFunc(all, func(a, b skills.Skill) int { return cmp.Compare(a.Source, b.Source) })
	items := make([]any, 0, len(all))
	for _, s := range all {
		toggle := "skills toggle " + s.Name
		use := map[string]any{"text": "Use now", "fill": "/skills use " + s.Name + " "}
		item := map[string]any{"text": s.Name, "desc": s.Description, "section": sourceSection(string(s.Source))}
		state := "enabled"
		switch {
		case s.Source == skills.Project: // no stored state to toggle
			item["key"], item["actions"] = "on", []any{use}
		case h.skillsReg.isEnabled(s.Name):
			item["key"], item["run"], item["actions"] = "on", toggle, []any{map[string]any{"text": "Disable", "run": toggle}, use}
		default:
			state = "disabled"
			item["key"], item["run"], item["actions"] = "off", toggle, []any{map[string]any{"text": "Enable", "run": toggle}}
		}
		open := map[string]any{"text": "Open", "run": "skills open " + s.Name}
		item["actions"] = append(item["actions"].([]any), open)
		// enabling a drafted skill is the only review it gets (DESIGN §10.4),
		// so the instructions being approved are in the detail, not just a path
		body, err := skills.Body(s.Dir)
		if err != nil {
			body = "could not read it: " + err.Error()
		}
		item["detail"] = fmt.Sprintf("%s\n\nsource: %s · %s\npath: %s\n\n%s",
			s.Description, s.Source, state, TildePath(filepath.Join(s.Dir, "SKILL.md")), body)
		items = append(items, item)
	}
	return items
}

// skillsCommands is /skills.
func (h *Host) skillsCommands() []*Command {
	return []*Command{{
		Name: "skills", NArgs: "*", Desc: "turn skills on or off, or use one",
		GoFn: func(h *Host, args string) error { return h.skillsCommand(args) },
		GoComplete: subcommandCompleter([]string{"enable", "disable", "use", "open"}, func(h *Host, _ string) []string {
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
				"enabled": s.Source == skills.Project || h.skillsReg.isEnabled(s.Name), "path": s.Dir,
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
		return luaResult(L, h.setSkillEnabled(L, name, on))
	}
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int { return enable(L, true) }))
	tb.RawSetString("disable", L.NewFunction(func(L *glua.LState) int { return enable(L, false) }))

	return tb
}
