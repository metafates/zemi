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
		h.authoredSkillsDir(), h.builtinSkillsDir(), h.learnedSkillsDir())
	for _, w := range warns {
		h.Log.Warn("skills", w, nil)
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
	desc, _ = learn.ScrubSecrets(strings.Join(strings.Fields(sanitize.String(desc)), " "))
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

func (h *Host) enabledSkills() []skills.Skill {
	return slices.DeleteFunc(h.skillsReg.list(), func(s skills.Skill) bool { return !h.skillsReg.isEnabled(s.Name) })
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
	b.WriteString("\n\n# Skills\nA description only says when a skill applies. When one matches the task, load it with the skill tool before acting, and follow it.\n")
	for _, s := range enabled {
		desc := s.Description
		if runes := []rune(desc); len(runes) > skillIndexDescMax {
			desc = string(runes[:skillIndexDescMax]) + "..."
		}
		b.WriteString("- " + s.Name + ": " + desc + "\n")
	}
	return b.String()
}

// skillIndexDescMax bounds one skills-index line (prompt budget).
const skillIndexDescMax = 160

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
		h.Log.Warn("skills", "enable hook failed", map[string]any{
			"skill": s.Name, "error": err.Error(),
		})
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

// skillsCommand dispatches /skills [enable|disable <name>].
func (h *Host) skillsCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		h.openSkills()
		return nil
	case len(fields) == 2 && (fields[0] == "enable" || fields[0] == "disable"):
		if err := h.setSkillEnabled(nil, fields[1], fields[0] == "enable"); err != nil {
			return err
		}
		h.Notify("info", "skills", fields[0]+"d "+fields[1])
		return nil
	default:
		return errors.New("usage: /skills | /skills enable <name> | /skills disable <name>")
	}
}

// openSkills lists discovered skills with their state; space or enter
// toggles the selected one through the same enable/disable path as the
// command and the list stays open.
func (h *Host) openSkills() {
	ui := h.hooks()
	if ui == nil || len(h.skillsReg.list()) == 0 {
		h.Notify("info", "skills", "no skills found (add them under skills/ in your config)")
		return
	}
	ui.OpenOverlay(Overlay{ID: "skills", Title: "Skills", Origin: lua.OriginBuiltin, Items: h.skillItems(), Reload: h.skillItems})
}

func (h *Host) skillItems() []any {
	all := h.skillsReg.list()
	items := make([]any, 0, len(all))
	for _, s := range all {
		mark, verb := "○ ", "enable"
		if h.skillsReg.isEnabled(s.Name) {
			mark, verb = "● ", "disable"
		}
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%s%-20s %s (%s)", mark, s.Name, s.Description, s.Source),
			"run":  "skills " + verb + " " + s.Name,
		})
	}
	return items
}

// SkillsCommandsStep registers the Go-owned /skills command with the
// builtin origin (user config may rebind it).
func (h *Host) SkillsCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "skills commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "skills", NArgs: "*", Desc: "browse skills and switch them on or off",
				GoFn: func(h *Host, args string) error { return h.skillsCommand(args) },
				GoComplete: subcommandCompleter([]string{"enable", "disable"}, func(h *Host, _ string) []string {
					var names []string
					for _, s := range h.skillsReg.list() {
						names = append(names, s.Name)
					}
					return names
				}),
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

// ---------------------------------------------------------------------------
// zemi.skills namespace

func (h *Host) skillsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, s := range h.skillsReg.list() {
			rows = append(rows, map[string]any{
				"name": s.Name, "description": s.Description, "source": string(s.Source),
				"enabled": h.skillsReg.isEnabled(s.Name), "path": s.Dir,
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
		if !memoryManageAllowed(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.skills.%s: only user or builtin origin may enable or disable skills", verb)
		}
		if err := h.setSkillEnabled(L, name, on); err != nil {
			L.RaiseError("zemi.skills.%s: %v", verb, err)
		}
		return 0
	}
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int { return enable(L, true) }))
	tb.RawSetString("disable", L.NewFunction(func(L *glua.LState) int { return enable(L, false) }))

	return tb
}
