// errlog.go keeps the recent runtime errors behind the statusline badge
// (DESIGN §11): widget renders, keymaps and event handlers record here,
// and /errors lists them newest first and clears the badge.
package hostapi

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

const errLogMax = 50

type errEntry struct {
	at     time.Time
	source string
	msg    string
}

type errLog struct {
	mu      sync.Mutex
	entries []errEntry
	unseen  bool
}

// RecordError keeps one error (sanitized) and raises the badge.
func (h *Host) RecordError(source, msg string) {
	l := &h.errs
	l.mu.Lock()
	l.entries = append(l.entries, errEntry{time.Now(), source, sanitize.String(msg)})
	if len(l.entries) > errLogMax {
		l.entries = l.entries[len(l.entries)-errLogMax:]
	}
	l.unseen = true
	l.mu.Unlock()
	h.refresh()
}

// errorsNamespace is zemi.errors: the same list /errors shows, readable
// from Lua so the agent can inspect zemi's own failures (boot included)
// through the lua tool without a permission ask.
func (h *Host) errorsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		l := &h.errs
		l.mu.Lock()
		entries := append([]errEntry(nil), l.entries...)
		l.mu.Unlock()
		rows := make([]any, 0, len(entries))
		for i, e := range slices.Backward(entries) { // newest first, like /errors
			rows = append(rows, map[string]any{
				"index": i, "at": e.at.Format("15:04:05"), "source": e.source, "message": e.msg,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// HasUnseenErrors reports whether errors arrived since /errors last opened.
func (h *Host) HasUnseenErrors() bool {
	h.errs.mu.Lock()
	defer h.errs.mu.Unlock()
	return h.errs.unseen
}

// OpenErrors runs /errors: it lists the recent errors newest first and
// clears the badge; "<n>" selects one, "ask <n>" hands it to the agent to
// explain and fix.
func (h *Host) OpenErrors(args string) error {
	l := &h.errs
	l.mu.Lock()
	entries := append([]errEntry(nil), l.entries...)
	l.unseen = false
	l.mu.Unlock()
	h.refresh()
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI attached")
	}
	f := strings.Fields(args)
	sel := 0
	if len(f) > 0 {
		i, err := strconv.Atoi(f[len(f)-1])
		if err != nil || i < 0 || i >= len(entries) {
			return fmt.Errorf("no error %q; /errors lists them", args)
		}
		if f[0] == "ask" {
			e := entries[i]
			prompt := fmt.Sprintf("zemi recorded this error at %s from %s. Load the zemi-debug skill, find the cause, and fix it if it is in my config; otherwise tell me what to do.\n\n%s",
				e.at.Format("15:04:05"), e.source, e.msg)
			h.GoSafe("errors ask", func() {
				if err := h.AgentSend(prompt); err != nil {
					h.Notify("error", "agent", err.Error())
				}
			})
			return nil
		}
		sel = len(entries) - 1 - i
	}
	items := make([]any, 0, len(entries))
	for i, e := range slices.Backward(entries) {
		at := e.at.Format("15:04:05")
		actions := []any{map[string]any{"text": "Ask zemi to fix it", "run": fmt.Sprintf("errors ask %d", i)}}
		if kind, name, ok := strings.Cut(e.source, " "); ok && (kind == "mcp" || kind == "lsp") {
			actions = append(actions, map[string]any{"text": "Restart " + name, "run": kind + " restart " + name})
		}
		items = append(items, map[string]any{
			"text":    e.source,
			"desc":    at + " · " + strings.Join(strings.Fields(e.msg), " "),
			"detail":  at + "\n" + e.msg,
			"actions": actions,
		})
	}
	ui.OpenOverlay(Overlay{ID: "errors", Title: "Errors", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Empty: "No errors since zemi started."})
	return nil
}
