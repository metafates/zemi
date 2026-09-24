// health.go is /health (after neovim's :checkhealth): one row per check,
// so a piece failing quietly (no API key, a missing sandbox, a
// reflector that keeps erroring) is visible in one place.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
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
	path   string
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
		c := luaHealthCheck{name: name, fn: fn, origin: h.RT.CurrentOrigin(L), path: h.RT.CurrentPath(L), gen: h.currentGen()}
		reg := &h.healthReg
		reg.mu.Lock()
		var err error
		switch i := slices.IndexFunc(reg.list, func(o luaHealthCheck) bool { return o.name == name }); {
		case i < 0:
			reg.list = append(reg.list, c)
		case reg.list[i].origin.Rank() > c.origin.Rank():
			err = fmt.Errorf("%s is already registered by a more trusted origin", name)
		default:
			if err = pluginClash(h.PluginOf, name, reg.list[i].origin, reg.list[i].path, c.origin, c.path); err == nil {
				reg.list[i] = c
			}
		}
		reg.mu.Unlock()
		if err != nil {
			L.RaiseError("zemi.health.add: %v", err)
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
			defer h.RT.PushFrame(L, c.origin, c.path)()
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
	// a role's own model would otherwise fail only at its first title,
	// summary or run, where nobody looks
	ids := []string{id}
	for _, r := range roles[1:] {
		if m := h.roleModel(r.Name); !slices.Contains(ids, m) {
			ids = append(ids, m)
			if row := h.modelCheck(r.Name+" model", m); row.level != "ok" {
				out = append(out, row)
			}
		}
	}

	// compactDue never fires on an unknown window, so the turn runs until
	// the provider refuses it
	var unknown []string
	for _, m := range ids {
		if m != "" && h.contextWindow(m) == 0 {
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
		add("warn", "sandbox", "unavailable: "+s.err.Error()+"; auto mode runs only read-only commands without asking")
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
		add("warn", "learning", fmt.Sprintf("%d %s to review in /memory", mem, plural(int(mem), "memory", "memories")))
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

	if loads := h.pluginLoads.Load(); loads != nil && len(*loads) > 0 {
		failed := 0
		for _, l := range *loads {
			if l.err != nil {
				failed++
			}
		}
		level, detail := "ok", fmt.Sprintf("%d %s", len(*loads), plural(len(*loads), "plugin", "plugins"))
		if failed > 0 {
			level, detail = "warn", fmt.Sprintf("%s · %d failed", detail, failed)
		}
		add(level, "plugins", detail)
		out[len(out)-1].run = "plugins"
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

	counts, servers := map[string]int{}, h.mcp.list()
	for _, s := range servers {
		s.mu.Lock()
		counts[s.status]++
		s.mu.Unlock()
	}
	switch {
	case len(servers) == 0:
		add("ok", "mcp", "no servers added")
	case counts["ready"] == len(servers):
		add("ok", "mcp", fmt.Sprintf("%d %s ready", len(servers), plural(len(servers), "server", "servers")))
	default:
		var parts []string
		for _, st := range slices.Sorted(maps.Keys(counts)) {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
		level := "warn"
		if counts["ready"]+counts["starting"] == len(servers) {
			level = "ok"
		}
		add(level, "mcp", fmt.Sprintf("%d %s: %s", len(servers), plural(len(servers), "server", "servers"), strings.Join(parts, ", ")))
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

// modelRow is the conversation's model check. A reload toasts it, so a
// models.chat nobody can run is not silent.
func (h *Host) modelRow() healthCheck { return h.modelCheck("model", h.sessionModelID()) }

// modelCheck is what a call on id would fail with, or an id the provider's
// list does not carry (a typo runs until the provider refuses it).
func (h *Host) modelCheck(name, id string) healthCheck {
	_, known := h.modelSpec(id)
	switch err := h.modelErr(id); {
	case err != nil:
		return healthCheck{level: "error", name: name, detail: err.Error()}
	case !known:
		return healthCheck{level: "warn", name: name, detail: unlistedDetail(id)}
	}
	return healthCheck{level: "ok", name: name, detail: id}
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

// WaitMCP returns once no MCP server is starting, or when ctx ends. A
// command-line run calls it first: its one send builds the tool set, and a
// server that is not ready then has no tools in it.
func (h *Host) WaitMCP(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // startMCP's own cap
	defer cancel()
	for slices.ContainsFunc(h.mcp.list(), func(s *mcpServer) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.status == "starting"
	}) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// healthActions labels the command a check's detail view runs; its initial
// is the key.
var healthActions = map[string]string{"errors": "Show errors", "trust": "Change trust", "mcp": "Open MCP servers", "memory": "Review memories", "plugins": "Open plugins"}

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
			label := healthActions[c.run]
			item["actions"] = []any{map[string]any{"text": label, "hotkey": strings.ToLower(label[:1]), "run": c.run}}
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
