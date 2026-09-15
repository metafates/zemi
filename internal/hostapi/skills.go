// skills.go is the M3 skills host API (DESIGN §6.7, §10.5): discovery
// synced against the skills table, the zemi.skills namespace, the enable
// hooks (authored skills run in the boot window after user init; learned
// skills run post-ready, only when enabled), and the /skills command.
// Enable/disable require a user or builtin origin: the model may propose,
// a plugin-registered keymap cannot flip the quarantine.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// SkillRow is one skill's user-facing state (UI and logs).
type SkillRow struct {
	Name        string
	Description string
	Source      string // authored | learned
	Enabled     bool
	Dir         string
	Hook        string
}

// skillRows returns every skill with live enabled state, sorted by name.
func (h *Host) skillRows() []SkillRow {
	var out []SkillRow
	for _, s := range h.skillsReg.list() {
		out = append(out, SkillRow{
			Name: s.Name, Description: s.Description,
			Source: string(s.Source), Enabled: h.skillsReg.isEnabled(s.Name),
			Dir: s.Dir, Hook: s.Hook,
		})
	}
	return out
}

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
	out := make([]skills.Skill, 0, len(r.byName))
	for _, s := range r.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
	found, warns := skills.Discover(
		h.authoredSkillsDir(), h.learnedSkillsDir())
	for _, w := range warns {
		h.Log.Warn("skills", w, nil)
	}
	rows := make([]*store.Skill, 0, len(found))
	for _, s := range found {
		rows = append(rows, &store.Skill{
			Name:           s.Name,
			Path:           s.Dir,
			Source:         string(s.Source),
			DefaultEnabled: s.Source == skills.Authored,
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

func (h *Host) authoredSkillsDir() string {
	if h.Dirs.ConfigHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.ConfigHome, "skills")
}

func (h *Host) learnedSkillsDir() string {
	if h.Dirs.DataHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.DataHome, "skills")
}

// skillIndex resolves enabled skills for the skill tool (DESIGN §10.5:
// discovery lists only enabled skills to the model).
func (h *Host) skillIndex() []tools.SkillInfo {
	var out []tools.SkillInfo
	for _, s := range h.skillsReg.list() {
		if !h.skillsReg.isEnabled(s.Name) {
			continue
		}
		out = append(out, tools.SkillInfo{Name: s.Name, Path: s.Dir})
	}
	return out
}

// skillsIndexPrompt renders the §10.7 skills index section: one line per
// enabled skill, description capped — the prompt carries the index only,
// bodies load on demand through the skill tool.
func (h *Host) skillsIndexPrompt() string {
	var b strings.Builder
	n := 0
	for _, s := range h.skillsReg.list() {
		if !h.skillsReg.isEnabled(s.Name) {
			continue
		}
		if n == 0 {
			b.WriteString("\n\n## Skills\nAvailable skills (invoke the `skill` tool with the name to load its full instructions):\n")
		}
		n++
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
// here. The origin gate is NOT here: Go-side callers (the :skills
// command) run off the interpreter goroutine where the frame origin is
// meaningless — Lua callers gate in the namespace, Go callers are the
// user by construction.
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
// inline; L nil means a Go-side caller (:skills enable) and the hook must
// be enqueued through Do — the single-writer invariant forbids touching
// the LState from here.
func (h *Host) enableHook(L *glua.LState, s skills.Skill) error {
	if L != nil {
		if err := h.runSkillHook(L, s); err != nil {
			return fmt.Errorf("enable hook failed: %w", err)
		}
		return nil
	}
	_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		if err := h.runSkillHook(L, s); err != nil {
			return nil, fmt.Errorf("enable hook failed: %w", err)
		}
		return nil, nil
	})
	return err
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

// SkillsAuthoredHooksStep returns the boot step that runs the enable
// hooks of enabled authored skills, after user init.lua and before the
// project config (DESIGN §5 load order). Hook failures are logged, never
// fatal.
func (h *Host) SkillsAuthoredHooksStep() lua.BootStep {
	return lua.BootStep{
		Name: "authored skill hooks",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.syncSkills()
			for _, s := range h.skillsReg.list() {
				if s.Source != skills.Authored || !h.skillsReg.isEnabled(s.Name) {
					continue
				}
				if err := h.runSkillHook(g.L, s); err != nil {
					continue
				}
			}
			return nil
		},
	}
}

// SkillsLearnedHooksStep returns the post-ready step that runs the enable
// hooks of enabled learned skills (they load only when enabled,
// post-ready, §5).
func (h *Host) SkillsLearnedHooksStep() lua.BootStep {
	return lua.BootStep{
		Name: "learned skill hooks",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.syncSkills()
			for _, s := range h.skillsReg.list() {
				if s.Source != skills.Learned || !h.skillsReg.isEnabled(s.Name) {
					continue
				}
				if err := h.runSkillHook(g.L, s); err != nil {
					continue
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
		all := h.skillsReg.list()
		n := 0
		for _, s := range all {
			if h.skillsReg.isEnabled(s.Name) {
				n++
			}
		}
		h.memNotify("info", "skills", fmt.Sprintf("%d skills, %d enabled", len(all), n))
		return nil
	case len(fields) == 2 && (fields[0] == "enable" || fields[0] == "disable"):
		if err := h.setSkillEnabled(nil, fields[1], fields[0] == "enable"); err != nil {
			return err
		}
		h.memNotify("info", "skills", fields[0]+"d "+fields[1])
		return nil
	default:
		return errors.New("usage: /skills | /skills enable <name> | /skills disable <name>")
	}
}

// SkillsCommandsStep registers the Go-owned /skills command with the
// builtin origin (user config may rebind it).
func (h *Host) SkillsCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "skills commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "skills", NArgs: "*", Desc: "show skills; enable|disable <name>",
				GoFn:   func(h *Host, args string) error { return h.skillsCommand(args) },
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
		out := L.NewTable()
		for _, s := range h.skillsReg.list() {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(s.Name))
			row.RawSetString("description", glua.LString(s.Description))
			row.RawSetString("source", glua.LString(s.Source))
			row.RawSetString("enabled", glua.LBool(h.skillsReg.isEnabled(s.Name)))
			row.RawSetString("path", glua.LString(s.Dir))
			out.Append(row)
		}
		L.Push(out)
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
			return 0
		}
		if err := h.setSkillEnabled(L, name, on); err != nil {
			L.RaiseError("zemi.skills.%s: %v", verb, err)
			return 0
		}
		return 0
	}
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int { return enable(L, true) }))
	tb.RawSetString("disable", L.NewFunction(func(L *glua.LState) int { return enable(L, false) }))

	return tb
}
