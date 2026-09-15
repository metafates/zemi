// health.go is /health (after neovim's :checkhealth): one row per check,
// so a piece failing quietly (no API key, a refused permissions.lua, a
// reflector that keeps erroring) is visible in one place.
package hostapi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/metafates/zemi/internal/lua"
)

type healthCheck struct {
	level  string // ok | warn | error
	name   string
	detail string
	run    string // command enter runs on the row
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

	switch id := h.currentModelID(nil); {
	case id == "":
		add("error", "model", "no provider registered; call zemi.provider.register in init.lua")
	case !h.Providers.HasKey(strings.SplitN(id, "/", 2)[0]):
		add("error", "model", id+": api key not set")
	default:
		add("ok", "model", id)
	}

	if path := h.permanentRulesPath(); path != "" {
		data, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			add("ok", "permissions", "no permanent rules yet")
		case err != nil:
			add("error", "permissions", err.Error())
		default:
			if rules, err := parsePermissionsDump(data); err != nil {
				add("error", "permissions", "permissions.lua refused: "+err.Error())
			} else {
				add("ok", "permissions", fmt.Sprintf("%d permanent rules", len(rules)))
			}
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		add("warn", "git", "not found; config changes are not versioned")
	} else {
		add("ok", "git", "config changes are versioned")
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

	if mem, cfg := h.pendingMemory.Load(), h.pendingConfig.Load(); mem+cfg > 0 {
		add("warn", "learning", fmt.Sprintf("%d memories wait in /memory, %d config changes in /config", mem, cfg))
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
		add("error", "lua", "runtime is hot after a wedged call; /reload")
	} else {
		add("ok", "lua", "runtime healthy")
	}

	h.errs.mu.Lock()
	nerr := len(h.errs.entries)
	h.errs.mu.Unlock()
	if nerr > 0 {
		add("warn", "errors", fmt.Sprintf("%d recent; enter lists them", nerr))
		out[len(out)-1].run = "errors"
	} else {
		add("ok", "errors", "none since start")
	}

	h.lsp.mu.Lock()
	var servers []string
	for name := range h.lsp.running {
		servers = append(servers, name)
	}
	h.lsp.mu.Unlock()
	if len(servers) > 0 {
		sort.Strings(servers)
		add("ok", "lsp", strings.Join(servers, ", "))
	} else {
		add("ok", "lsp", "starts when the agent edits a supported file")
	}
	return out
}

// OpenHealth shows the checks in the list overlay.
func (h *Host) OpenHealth() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI attached")
	}
	marks := map[string]string{"ok": "✓", "warn": "!", "error": "✗"}
	var items []any
	for _, c := range h.healthChecks() {
		item := map[string]any{"text": fmt.Sprintf("%s %-12s %s", marks[c.level], c.name, c.detail)}
		if c.run != "" {
			item["run"] = c.run
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "health", Title: "Health", Origin: lua.OriginBuiltin, Items: items})
	return nil
}
