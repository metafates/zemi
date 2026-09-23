// health.go is /health (after neovim's :checkhealth): one row per check,
// so a piece failing quietly (no API key, a missing sandbox, a
// reflector that keeps erroring) is visible in one place.
package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

type healthCheck struct {
	level  string // ok | warn | error
	name   string
	detail string
	run    string // the command the row's detail action runs
}

// healthReg holds the checks Lua registered with zemi.health.add;
// healthChecks runs them after the built-in rows.
type healthReg struct {
	mu   sync.Mutex
	list []luaHealthCheck
}

type luaHealthCheck struct {
	name   string
	fn     *glua.LFunction
	origin lua.Origin
	gen    Gen
}

func (r *healthReg) purge() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

// health.add(name, fn): fn returns { level = "ok"|"warn"|"error",
// detail = "..." } or a bare detail string. A name is one row: a less
// trusted origin is refused, anyone else replaces it.
func (h *Host) healthNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, fn := L.CheckString(1), L.CheckFunction(2)
		if strings.TrimSpace(name) == "" {
			L.RaiseError("zemi.health.add: name is required")
		}
		c := luaHealthCheck{name: name, fn: fn, origin: h.RT.CurrentOrigin(L), gen: h.currentGen()}
		reg := &h.healthReg
		reg.mu.Lock()
		i := slices.IndexFunc(reg.list, func(o luaHealthCheck) bool { return o.name == name })
		refused := i >= 0 && reg.list[i].origin.Rank() > c.origin.Rank()
		switch {
		case refused:
		case i >= 0:
			reg.list[i] = c
		default:
			reg.list = append(reg.list, c)
		}
		reg.mu.Unlock()
		if refused {
			L.RaiseError("zemi.health.add: %s is already registered by a more trusted origin", name)
		}
		return 0
	}))
	// list() -> the rows /health shows, so config written from Lua can be
	// checked without asking the user to run /health. Registered checks run
	// inline: list is already on the interpreter goroutine, where the queue
	// luaHealthRows uses would deadlock.
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, c := range append(h.goHealthRows(), h.inlineHealthRows(L)...) {
			rows = append(rows, map[string]any{"level": c.level, "name": c.name, "detail": c.detail})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// inlineHealthRows runs the registered checks on the calling state.
func (h *Host) inlineHealthRows(L *glua.LState) []healthCheck {
	h.healthReg.mu.Lock()
	list := slices.Clone(h.healthReg.list)
	h.healthReg.mu.Unlock()
	gen := h.currentGen()
	var out []healthCheck
	for _, c := range list {
		if c.gen != gen {
			continue
		}
		row := healthCheck{level: "ok", name: c.name}
		func() {
			defer h.RT.PushFrame(L, c.origin, "health")()
			base := L.GetTop()
			L.Push(c.fn)
			if err := L.PCall(0, 1, nil); err != nil {
				L.SetTop(base)
				row.level, row.detail = "error", sanitize.String(err.Error())
				return
			}
			v, err := lua.ToGo(L.Get(-1))
			L.SetTop(base)
			if err != nil {
				row.level, row.detail = "error", sanitize.String(err.Error())
				return
			}
			row.level, row.detail = healthResult(v)
		}()
		out = append(out, row)
	}
	return out
}

// luaHealthRows runs the registered checks through the queue, bounded by
// the veto deadline: /health is Go-side, so the checks must not run on the
// calling goroutine.
func (h *Host) luaHealthRows() []healthCheck {
	h.healthReg.mu.Lock()
	n := len(h.healthReg.list)
	h.healthReg.mu.Unlock()
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lua.VetoDeadline)
	defer cancel()
	vals, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		return []any{h.inlineHealthRows(L)}, nil
	})
	if err != nil || len(vals) == 0 {
		return []healthCheck{{level: "error", name: "health checks", detail: sanitize.String(fmt.Sprint(err))}}
	}
	rows, _ := vals[0].([]healthCheck)
	return rows
}

func healthResult(v any) (level, detail string) {
	switch v := v.(type) {
	case string:
		return "ok", sanitize.String(v)
	case map[string]any:
		level, _ = v["level"].(string)
		detail, _ = v["detail"].(string)
		switch level {
		case "ok", "warn", "error":
		default:
			level = "ok"
		}
		return level, sanitize.String(detail)
	}
	return "ok", ""
}

// PluginsStep loads site/pack/*/start/*/plugin/*.lua sorted, per the boot
// order (DESIGN §5). A failing file does not stop the ones after it.
func (h *Host) PluginsStep() lua.BootStep {
	return lua.BootStep{
		Name: "plugins",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			files, err := filepath.Glob(filepath.Join(h.Dirs.PluginsDir(), "*", "start", "*", "plugin", "*.lua"))
			slices.Sort(files)
			var errs []error
			for _, f := range files {
				if err := lua.LoadFileStep("plugin "+f, f, lua.OriginPlugin).Run(ctx, r, g); err != nil {
					errs = append(errs, err)
				}
			}
			h.pluginsBoot.Store(&[2]int{len(errs), len(files)})
			return errors.Join(append(errs, err)...)
		},
	}
}

// goHealthRows are the Go-side checks, in the order /health shows them.
func (h *Host) goHealthRows() []healthCheck {
	var out []healthCheck
	add := func(level, name, detail string) {
		out = append(out, healthCheck{level: level, name: name, detail: detail})
	}

	if h.DB == nil || h.GlobalDB == nil {
		add("error", "store", fmt.Sprintf("sessions and memory unavailable: %v", h.initErr))
	} else {
		add("ok", "store", "sessions and memory on disk")
	}

	var names []string
	for _, s := range h.Providers.List() {
		switch {
		case !h.Providers.HasKey(s.ID): // a spec without one: openai_compat runs, the rest fail their first turn
			names = append(names, s.ID+" (no key)")
		case s.Builtin:
			names = append(names, s.ID+" (builtin)")
		default:
			names = append(names, s.ID)
		}
	}
	if len(names) == 0 {
		add("warn", "providers", "none usable")
	} else if n := len(h.Providers.Missing()); n > 0 {
		add("ok", "providers", fmt.Sprintf("%s; %d builtin without a key exported", strings.Join(names, ", "), n))
	} else {
		add("ok", "providers", strings.Join(names, ", "))
	}

	id := h.sessionModelID()
	out = append(out, h.modelRow())

	// compactDue never fires on an unknown window, so the turn runs until
	// the provider refuses it
	var unknown []string
	for _, tier := range append([]string{""}, Tiers...) {
		m := cmp.Or(h.tierModel(tier), id) // "" is no tier: the conversation's model
		if m != "" && h.contextWindow(m) == 0 && !slices.Contains(unknown, m) {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 {
		add("warn", "context", "unknown for "+strings.Join(unknown, ", ")+": no context percent, no auto compaction; set context_window on the model entry, or agent.context_window")
	} else if id != "" {
		add("ok", "context", "window known, auto compaction on")
	}

	if _, err := exec.LookPath("git"); err != nil {
		add("warn", "git", "not found; the prompt has no git status and git commands that print file content ask")
	} else {
		add("ok", "git", "found")
	}

	// internal/tools resolves rg the same way at init, in this process.
	if rg, err := exec.LookPath("rg"); err != nil {
		add("warn", "ripgrep", "not found; glob and grep use the slower Go walker")
	} else {
		add("ok", "ripgrep", rg+" runs glob and grep")
	}

	switch s := h.sandbox.Load(); {
	case s.err != nil:
		add("warn", "sandbox", "unavailable: "+s.err.Error()+"; settled commands limited to readers")
	case !s.backend.EnforcesProtect:
		add("ok", "sandbox", s.backend.Name+" confines the commands auto mode and read-only agents run; it cannot keep .git read-only, so the project is read-only to them")
	default:
		add("ok", "sandbox", s.backend.Name+" confines the commands auto mode and read-only agents run")
	}

	h.agents.mu.Lock()
	reflectErr := h.agents.reflectErr
	h.agents.mu.Unlock()
	switch {
	case errors.Is(reflectErr, errNoProposal):
		add("warn", "reflector", "last run "+reflectErr.Error())
	case reflectErr != nil:
		add("error", "reflector", "last run failed: "+reflectErr.Error())
	default:
		add("ok", "reflector", "runs when a session closes and at startup for idle ones")
	}

	if mem := h.pendingMemory.Load(); mem > 0 {
		add("warn", "learning", fmt.Sprintf("%d %s to review in /memory", mem, plural(int(mem), "suggestion", "suggestions")))
		out[len(out)-1].run = "memory"
	} else {
		add("ok", "learning", "nothing to review")
	}

	// the project's own skills are always on and never in the registry, so
	// counting only the registry would disagree with the prompt
	proj := len(h.projectOnlySkills())
	msg := fmt.Sprintf("%d found, %d enabled", len(h.skillsReg.list())+proj, len(h.enabledSkills()))
	if proj > 0 {
		msg += fmt.Sprintf(" (%d from this project)", proj)
	}
	add("ok", "skills", msg)

	if h.RT.IsHot() {
		add("error", "lua", "stopped: a boot step failed or a call wedged; see /errors, then /reload")
	} else {
		add("ok", "lua", "runtime healthy")
	}

	// a widget in a region no node draws is the first thing a relayout breaks
	drawn := h.Layout().Regions()
	var undrawn []string
	for _, w := range h.Widgets.List() {
		if !drawn[w.Region] {
			undrawn = append(undrawn, w.ID+" ("+w.Region+")")
		}
	}
	if len(undrawn) > 0 {
		add("warn", "layout", "the layout draws no region for: "+strings.Join(undrawn, ", "))
	} else {
		add("ok", "layout", "every widget's region is drawn")
	}

	// a pack directory with neither plugin/*.lua nor lua/ loads nothing, silently
	if packs, _ := filepath.Glob(filepath.Join(h.Dirs.PluginsDir(), "*", "start", "*")); len(packs) > 0 {
		var empty []string
		for _, p := range packs {
			files, _ := filepath.Glob(filepath.Join(p, "plugin", "*.lua"))
			if _, err := os.Stat(filepath.Join(p, "lua")); len(files) == 0 && err != nil {
				empty = append(empty, filepath.Base(p))
			}
		}
		// the boot's counts, so the pair agrees when files moved since
		boot := cmp.Or(h.pluginsBoot.Load(), &[2]int{})
		failed, total := boot[0], boot[1]
		level, detail := "ok", fmt.Sprintf("%d %s loaded", total, plural(total, "file", "files"))
		if failed > 0 {
			level, detail = "warn", fmt.Sprintf("%d of %d %s failed, see /errors", failed, total, plural(total, "file", "files"))
		}
		if len(empty) > 0 {
			level = "warn"
			detail += "; nothing to load in " + strings.Join(empty, ", ") + ": a plugin is site/pack/*/start/<name>/plugin/*.lua or lua/"
		}
		add(level, "plugins", detail)
	}

	h.errs.mu.Lock()
	nerr := len(h.errs.entries)
	var shown errEntry
	boot := false
	for _, e := range h.errs.entries {
		if strings.HasPrefix(e.source, "boot") {
			shown, boot = e, true
		}
	}
	if nerr > 0 && !boot {
		shown = h.errs.entries[nerr-1]
	}
	h.errs.mu.Unlock()
	if nerr > 0 {
		// name one of them: headless there is no /errors to open, and a
		// boot step that failed is an error so --health does not read green
		level := "warn"
		if boot {
			level = "error"
		}
		add(level, "errors", fmt.Sprintf("%d since start, %s: %s", nerr, shown.source, Headline(shown.msg)))
		out[len(out)-1].run = "errors"
	} else {
		add("ok", "errors", "none since start")
	}

	if h.ProjectTrusted() {
		add("ok", "trust", "trusted: "+TildePath(h.Dirs.TrustRoot()))
	} else {
		add("warn", "trust", "not trusted: project config, instructions and language servers are off; /trust")
	}
	out[len(out)-1].run = "trust"

	failed, servers := 0, h.mcp.list()
	for _, s := range servers {
		s.mu.Lock()
		if s.status == "failed" || s.status == "needs sign-in" {
			failed++
		}
		s.mu.Unlock()
	}
	switch {
	case len(servers) == 0:
		add("ok", "mcp", "no servers added")
	case failed > 0:
		add("warn", "mcp", fmt.Sprintf("%d %s, %d not working", len(servers), plural(len(servers), "server", "servers"), failed))
	default:
		add("ok", "mcp", fmt.Sprintf("%d %s ready", len(servers), plural(len(servers), "server", "servers")))
	}
	if len(servers) > 0 {
		out[len(out)-1].run = "mcp"
	}

	var lsps []string
	for _, row := range h.lspRows() {
		lsps = append(lsps, fmt.Sprintf("%s (%s)", row["name"], row["status"]))
	}
	switch {
	case len(lsps) > 0:
		add("ok", "lsp", strings.Join(lsps, ", "))
	case !h.ProjectTrusted():
		add("ok", "lsp", "off: project not trusted (/trust)")
	default:
		add("ok", "lsp", "none for this project: install gopls, pyright, typescript-language-server, clangd or rust-analyzer, or zemi.lsp.add one")
	}
	return out
}

// modelRow is the model check: what a send would fail with, or an id the
// provider's list does not carry (a typo runs until the provider refuses
// it). A reload toasts it, so a models.main nobody can run is not silent.
func (h *Host) modelRow() healthCheck {
	h.agents.mu.Lock()
	id := h.liveModelID()
	h.agents.mu.Unlock()
	_, known := h.modelSpec(id)
	switch err := h.modelErr(id); {
	case err != nil:
		return healthCheck{level: "error", name: "model", detail: err.Error()}
	case !known:
		return healthCheck{level: "warn", name: "model", detail: unlistedDetail(id)}
	}
	return healthCheck{level: "ok", name: "model", detail: id}
}

// unlistedDetail is the warning for an id the provider's models list
// does not carry: the pick is taken, since the provider decides.
func unlistedDetail(id string) string {
	return id + ": not in the provider's models list; the provider decides whether it exists"
}

func (h *Host) healthChecks() []healthCheck {
	return append(h.goHealthRows(), h.luaHealthRows()...)
}

// HealthRow is one check for a caller with no UI (zemi --health).
type HealthRow struct{ Level, Name, Detail string }

// HealthRows runs the checks and returns them as plain data.
func (h *Host) HealthRows() []HealthRow {
	checks := h.healthChecks() // once: every check has side effects and file IO
	out := make([]HealthRow, 0, len(checks))
	for _, c := range checks {
		out = append(out, HealthRow{Level: c.level, Name: c.name, Detail: c.detail})
	}
	return out
}

// healthActions labels the command a check's detail view runs.
var healthActions = map[string]string{"errors": "Show errors", "trust": "Change trust", "mcp": "Open MCP servers", "memory": "Review memories"}

// OpenHealth shows the checks in the list overlay.
func (h *Host) OpenHealth() error {
	ui, err := h.Screen()
	if err != nil {
		return fmt.Errorf("%w; --health prints the checks", err)
	}
	// A Go command runs on the key loop, and the checks do file IO and call
	// Lua under the veto deadline: collect them off it, then open.
	h.GoSafe("health", func() {
		ui.OpenOverlay(Overlay{ID: "health", Title: "Health", Origin: lua.OriginBuiltin, Items: h.healthItems()})
	})
	return nil
}

// healthItems are the /health rows, problems first.
func (h *Host) healthItems() []any {
	var problems, ok []any
	for _, c := range h.healthChecks() {
		item := map[string]any{"text": c.name, "desc": c.detail, "section": "OK"}
		// a detail view that only repeats a short, fine row leads nowhere
		if c.run != "" || c.level != "ok" || utf8.RuneCountInString(c.detail) > 48 {
			item["detail"] = c.detail
		}
		if c.run != "" {
			item["actions"] = []any{map[string]any{"text": healthActions[c.run], "run": c.run}}
		}
		if c.level == "ok" {
			ok = append(ok, item)
			continue
		}
		item["section"] = "Needs attention"
		problems = append(problems, item)
	}
	return append(problems, ok...)
}
