// health.go is /health (after neovim's :checkhealth): one row per check,
// so a piece failing quietly (no API key, a refused permissions.lua, a
// reflector that keeps erroring) is visible in one place.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	run    string // command enter runs on the row
}

// healthReg holds the checks Lua registered with zemi.health.register;
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

// health.register(name, fn): fn returns { level = "ok"|"warn"|"error",
// detail = "..." } or a bare detail string.
func (h *Host) healthNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		name, fn := L.CheckString(1), L.CheckFunction(2)
		if strings.TrimSpace(name) == "" {
			L.RaiseError("zemi.health.register: name is required")
		}
		h.healthReg.mu.Lock()
		h.healthReg.list = append(h.healthReg.list, luaHealthCheck{
			name: name, fn: fn, origin: h.RT.CurrentOrigin(L), gen: h.currentGen(),
		})
		h.healthReg.mu.Unlock()
		return 0
	}))
	return tb
}

// luaHealthRows runs the registered checks, each bounded by the veto
// deadline so a slow one cannot stall /health. It goes through the queue,
// so it must not run on the interpreter goroutine; /health is Go-side.
func (h *Host) luaHealthRows() []healthCheck {
	h.healthReg.mu.Lock()
	list := append([]luaHealthCheck(nil), h.healthReg.list...)
	h.healthReg.mu.Unlock()
	gen := h.currentGen()
	var out []healthCheck
	for _, c := range list {
		if c.gen != gen {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), lua.VetoDeadline)
		vals, err := h.RT.Call(ctx, c.origin, c.fn)
		cancel()
		row := healthCheck{level: "ok", name: c.name}
		switch {
		case err != nil:
			row.level, row.detail = "error", sanitize.String(err.Error())
		case len(vals) == 0:
			row.detail = "no result"
		default:
			row.level, row.detail = healthResult(vals[0])
		}
		out = append(out, row)
	}
	return out
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

func (h *Host) healthChecks() []healthCheck {
	var out []healthCheck
	add := func(level, name, detail string) {
		out = append(out, healthCheck{level: level, name: name, detail: detail})
	}

	if h.DB == nil || h.GlobalDB == nil {
		add("error", "store", fmt.Sprintf("sessions and memory unavailable: %v", h.initErr))
	} else {
		add("ok", "store", "sessions and memory on disk")
	}

	h.agents.mu.Lock()
	id := h.liveModelID()
	h.agents.mu.Unlock()
	switch {
	case id == "":
		add("error", "model", "no provider registered; call zemi.provider.register in init.lua")
	case !h.Providers.HasKey(strings.SplitN(id, "/", 2)[0]):
		add("error", "model", id+": api key not set")
	default:
		add("ok", "model", id)
	}

	if h.permanentRulesPath() != "" {
		switch rules, err := h.readPermanentRules(); {
		case err != nil:
			add("error", "permissions", err.Error())
		case len(rules) == 0:
			add("ok", "permissions", "no permanent rules yet")
		default:
			add("ok", "permissions", fmt.Sprintf("%d permanent rules", len(rules)))
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		add("warn", "git", "not found; config changes are not versioned")
	} else {
		add("ok", "git", "config changes are versioned")
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
		add("ok", "sandbox", s.backend.Name+" confines the commands auto and plan mode run; it cannot keep .git read-only, so the project is read-only to them")
	default:
		add("ok", "sandbox", s.backend.Name+" confines the commands auto and plan mode run")
	}

	h.agents.mu.Lock()
	reflectErr := h.agents.reflectErr
	h.agents.mu.Unlock()
	switch {
	case h.reflectEveryN() == 0:
		add("warn", "reflector", "disabled (reflector.every_n_sessions = 0)")
	case reflectErr != "":
		add("error", "reflector", "last run failed: "+reflectErr)
	default:
		add("ok", "reflector", "runs when a session closes")
	}

	if mem := h.pendingMemory.Load(); mem > 0 {
		add("warn", "learning", fmt.Sprintf("%d memories wait in /memory", mem))
		out[len(out)-1].run = "memory"
	} else {
		add("ok", "learning", "nothing waiting for review")
	}

	all, enabled := h.skillsReg.list(), 0
	for _, s := range all {
		if h.skillsReg.isEnabled(s.Name) {
			enabled++
		}
	}
	add("ok", "skills", fmt.Sprintf("%d found, %d enabled", len(all), enabled))

	if h.RT.IsHot() {
		add("error", "lua", "stopped: a boot step failed or a call wedged; see /errors, then /reload")
	} else {
		add("ok", "lua", "runtime healthy")
	}

	h.errs.mu.Lock()
	nerr := len(h.errs.entries)
	h.errs.mu.Unlock()
	if nerr > 0 {
		add("warn", "errors", fmt.Sprintf("%d since start", nerr))
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
	return append(out, h.luaHealthRows()...)
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
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI attached")
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
