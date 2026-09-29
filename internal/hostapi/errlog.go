// errlog.go keeps the recent runtime errors behind the statusline badge
// (DESIGN §11): widget renders, keymaps and event handlers record here,
// and /errors lists them newest first and clears the badge.

package hostapi

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
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

// RecordError keeps one error (sanitized) and raises the badge. The same
// message twice in a row is kept once, at the later time.
func (h *Host) RecordError(source, msg string) {
	l := &h.errs
	l.mu.Lock()
	msg = sanitize.String(msg)
	if n := len(l.entries); n > 0 && l.entries[n-1].msg == msg {
		l.entries[n-1].at = h.now() // the badge is back, so the row says when
	} else {
		l.entries = append(l.entries, errEntry{h.now(), source, msg})
		if len(l.entries) > errLogMax {
			l.entries = l.entries[len(l.entries)-errLogMax:]
		}
	}
	l.unseen = true
	l.mu.Unlock()
	if h.test != nil {
		h.test.recorded(source, msg)
	}
	h.refresh()
}

// errorsSince is the errors recorded at or after t under a source starting
// with prefix, oldest first, one "error: <source>: <headline>" line each.
func (h *Host) errorsSince(t time.Time, prefix string) []string {
	l := &h.errs
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if !e.at.Before(t) && strings.HasPrefix(e.source, prefix) {
			out = append(out, "error: "+e.source+": "+Headline(e.msg))
		}
	}
	return out
}

// Recorded reports whether msg is the newest recorded error: a toast of it
// can point at /errors for the rest.
func (h *Host) Recorded(msg string) bool {
	l := &h.errs
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries) > 0 && l.entries[len(l.entries)-1].msg == sanitize.String(msg)
}

// RecordErrorOnce logs and records an error unless the list already holds
// it. Skills, agents and workflows are re-discovered on every turn and
// reload, and one broken file must not refill the badge or the log.
func (h *Host) RecordErrorOnce(source, msg string) {
	msg = sanitize.String(msg)
	l := &h.errs
	l.mu.Lock()
	dup := slices.ContainsFunc(l.entries, func(e errEntry) bool { return e.source == source && e.msg == msg })
	l.mu.Unlock()
	if !dup {
		h.Log.Warn(source, msg, nil)
		h.RecordError(source, msg)
	}
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
				"index": i, "at": e.at.Format(time.TimeOnly), "source": e.source, "message": e.msg,
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

// errorAskPrompt takes an error's time, source and text; it rides behind
// the typed line, which names the error since its index dies with the process.
const errorAskPrompt = "zemi recorded this error at %s from %s. Load the zemi skill and read its debug.md, find the cause, and fix it if it is in my config; otherwise tell me what to do.\n\n%s"

// OpenErrors runs /errors: it lists the recent errors newest first and
// clears the badge; "<n>" selects one, "ask <n>" hands it to the agent to
// explain and fix.
func (h *Host) OpenErrors(args string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	l := &h.errs
	l.mu.Lock()
	entries := append([]errEntry(nil), l.entries...)
	l.unseen = false
	l.mu.Unlock()
	h.refresh()
	f := strings.Fields(args)
	sel := 0
	if len(f) > 0 {
		i, err := strconv.Atoi(f[len(f)-1])
		if err != nil || i < 0 || i >= len(entries) {
			return Usagef("no error %q; /errors lists them", args)
		}
		if f[0] == "ask" {
			e := entries[i]
			prompt := "/errors ask " + e.source + ": " + Headline(e.msg) + agent.Attached("command", fmt.Sprintf(errorAskPrompt, e.at.Format(time.TimeOnly), e.source, e.msg))
			h.GoSafe("errors ask", func() {
				if err := h.AgentSend(prompt); err != nil {
					h.Notify(logging.Error, "Could not send", h.friendlyError(err))
				}
			})
			return nil
		}
		sel = len(entries) - 1 - i
	}
	items := make([]any, 0, len(entries))
	noModel := h.modelErr(h.currentModelID(nil))
	for i, e := range slices.Backward(entries) {
		at := e.at.Format(time.TimeOnly)
		var actions []any
		if noModel == nil {
			actions = append(actions, map[string]any{"text": "Ask zemi to fix it", "hotkey": "a", "run": fmt.Sprintf("errors ask %d", i)})
		}
		if kind, name, ok := strings.Cut(e.source, " "); ok && (kind == "mcp" || kind == "lsp") {
			actions = append(actions, map[string]any{"text": "Restart", "hotkey": "r", "run": kind + " restart " + name})
		}
		items = append(items, map[string]any{
			"text":    e.source,
			"desc":    at + " · " + Headline(e.msg),
			"fields":  fieldList("at", at, "error", ShortChunk(e.msg)),
			"actions": actions,
		})
	}
	ui.OpenOverlay(Overlay{ID: "errors", Title: "Errors", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Empty: "No errors yet."})
	return nil
}

// chunkPath matches a Lua chunk name ("@user:/home/me/.config/zemi/init.lua:"),
// tagged with its origin or not.
var chunkPath = regexp.MustCompile(`(?:@\w+:)?(/\S*/[^/:\s]+):`)

// ShortChunk makes chunk names readable: the origin tag dropped and home
// shortened to ~.
func ShortChunk(s string) string {
	return chunkPath.ReplaceAllStringFunc(s, func(m string) string { return TildePath(m[strings.Index(m, "/"):]) })
}

// Headline is an error's first line, chunk names shortened: what stderr,
// a toast and the /errors row show; the rest is /errors detail.
func Headline(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	return ShortChunk(first)
}

// SetInitError records how the user init.lua step ended: "" when it ran
// clean. The splash and a headless send read it, so a config that failed
// to load is not reported as a missing model.
func (h *Host) SetInitError(msg string) { h.initFail.Store(&msg) }

func (h *Host) InitError() string {
	if p := h.initFail.Load(); p != nil {
		return *p
	}
	return ""
}
