// errlog.go keeps the recent runtime errors behind the statusline badge
// (DESIGN §11): widget renders, keymaps and event handlers record here,
// and /errors lists them newest first and clears the badge.

package hostapi

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

const errLogMax = 50

type errEntry struct {
	at         time.Time
	kind, name string // the source as "kind name"; name empty for a bare kind
	msg        string
}

// errSource is a record's source as it renders: "kind name", or a bare kind.
func errSource(kind, name string) string {
	if name == "" {
		return kind
	}
	return kind + " " + name
}

// source renders the entry the way it was composed before: one string,
// "kind name" or a bare kind.
func (e errEntry) source() string { return errSource(e.kind, e.name) }

type errLog struct {
	mu      sync.RWMutex
	entries []errEntry
	unseen  bool
}

// The error kinds an entry's restart key comes from: the kind is the first
// word of the command the /errors restart row runs ("<kind> restart <name>"),
// so a record site spelling it off would silently lose the key.
const (
	errMCP = "mcp"
	errLSP = "lsp"
)

// RecordError keeps one error (sanitized) and raises the badge. kind names
// what failed and name what it acted on ("mcp" and a server's name); the
// same message twice in a row is kept once, at the later time.
func (h *Host) RecordError(kind, name, msg string) {
	l := &h.errs
	l.mu.Lock()
	msg = sanitize.String(msg)
	if n := len(l.entries); n > 0 && l.entries[n-1].msg == msg {
		l.entries[n-1].at = h.now() // the badge is back, so the row says when
	} else {
		l.entries = appendLast(l.entries, errEntry{at: h.now(), kind: kind, name: name, msg: msg}, errLogMax)
	}
	l.unseen = true
	l.mu.Unlock()
	if h.test != nil {
		h.test.recorded(kind, name, msg)
	}
	h.refresh()
}

// errorsSince is the errors recorded at or after t under a source starting
// with prefix, oldest first, one "error: <source>: <headline>" line each.
func (h *Host) errorsSince(t time.Time, prefix string) []string {
	l := &h.errs
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []string
	for _, e := range l.entries {
		if !e.at.Before(t) && strings.HasPrefix(e.source(), prefix) {
			out = append(out, "error: "+e.source()+": "+Headline(e.msg))
		}
	}
	return out
}

// Recorded reports whether msg is the newest recorded error: a toast of it
// can point at /errors for the rest.
func (h *Host) Recorded(msg string) bool {
	l := &h.errs
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries) > 0 && l.entries[len(l.entries)-1].msg == sanitize.String(msg)
}

// RecordErrorOnce logs and records an error unless the list already holds
// it. Skills, agents and workflows are re-discovered on every turn and
// reload, and one broken file must not refill the badge or the log.
func (h *Host) RecordErrorOnce(kind, name, msg string) {
	msg = sanitize.String(msg)
	l := &h.errs
	l.mu.RLock()
	dup := slices.ContainsFunc(l.entries, func(e errEntry) bool { return e.kind == kind && e.name == name && e.msg == msg })
	l.mu.RUnlock()
	if !dup {
		h.Log.Warn(msg, "origin", errSource(kind, name))
		h.RecordError(kind, name, msg)
	}
}

// errorsNamespace is zemi.errors: the same list /errors shows, readable
// from Lua so the agent can inspect zemi's own failures (boot included)
// through the lua tool without a permission ask.
func (h *Host) errorsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		l := &h.errs
		l.mu.RLock()
		entries := slices.Clone(l.entries)
		l.mu.RUnlock()
		rows := make([]any, 0, len(entries))
		for i, e := range slices.Backward(entries) { // newest first, like /errors
			rows = append(rows, map[string]any{
				"index": i, "at": e.at.Format(time.TimeOnly), "source": e.source(), "message": e.msg,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// HasUnseenErrors reports whether errors arrived since /errors last opened.
func (h *Host) HasUnseenErrors() bool {
	h.errs.mu.RLock()
	defer h.errs.mu.RUnlock()
	return h.errs.unseen
}

// errorAskPrompt takes an error's time and source, zemiSkillDir and the
// error's text; it rides behind the typed line, which names the error since
// its index dies with the process.
const errorAskPrompt = "zemi recorded this error at %s from %s. Read SKILL.md, then debug.md, in %s, the zemi skill's directory, find the cause, and fix it if it is in my config; otherwise tell me what to do.\n\n%s"

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
	entries := slices.Clone(l.entries)
	l.unseen = false
	l.mu.Unlock()
	h.refresh()
	f := strings.Fields(args)
	sel := 0
	if len(f) > 0 {
		// a bad index opens the list at the newest: the toast naming /errors
		// would advise the command that just failed
		if i, err := strconv.Atoi(f[len(f)-1]); err == nil && i >= 0 && i < len(entries) {
			if f[0] == "ask" {
				e := entries[i]
				prompt := "/errors ask " + e.source() + ": " + Headline(e.msg) + agent.Attached("command", fmt.Sprintf(errorAskPrompt, e.at.Format(time.TimeOnly), e.source(), h.zemiSkillDir(), e.msg))
				h.GoSafe("errors ask", func() {
					if err := h.AgentSend(prompt); err != nil {
						h.Notify(slog.LevelError, "Could not send", h.friendlyError(err))
					}
				})
				return nil
			}
			sel = len(entries) - 1 - i
		}
	}
	items := make([]OverlayItem, 0, len(entries))
	noModel := h.modelErr(h.roleModel(defaultRole))
	for i, e := range slices.Backward(entries) {
		at := e.at.Format(time.TimeOnly)
		var keys []ItemKey
		if noModel == nil {
			keys = append(keys, ItemKey{Key: "a", Desc: "ask zemi to fix it", Run: fmt.Sprintf("errors ask %d", i)})
		}
		if (e.kind == errMCP || e.kind == errLSP) && e.name != "" {
			keys = append(keys, ItemKey{Key: "r", Desc: "restart", Run: e.kind + " restart " + e.name})
		}
		items = append(items, OverlayItem{
			Text:   e.source(),
			Desc:   at + "  " + Headline(e.msg),
			Fields: fieldList("at", at, "error", ShortChunk(e.msg)),
			Keys:   keys,
		})
	}
	ui.OpenOverlay(Overlay{ID: "errors", Title: "Errors", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Empty: "No recorded errors yet; a failed turn draws its error in the chat."})
	return nil
}

// chunkPath matches a Lua chunk name ("@user:/home/me/.config/zemi/init.lua:"),
// tagged with its origin or not.
var chunkPath = regexp.MustCompile(`(?:@\w+:)?(/\S*/[^/:\s]+):`)

// ShortChunk makes chunk names readable: the origin tag dropped and home
// shortened to ~.
func ShortChunk(s string) string {
	return chunkPath.ReplaceAllStringFunc(s, func(m string) string { return perm.TildePath(m[strings.Index(m, "/"):]) })
}

// Headline is an error's first line, chunk names shortened: what stderr,
// a toast and the /errors row show; the rest is /errors detail.
func Headline(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	return ShortChunk(first)
}

// SetInitError records how the user init.lua step ended: "" when it ran
// clean. The splash reads it, so a config that failed to load is not
// reported as a missing model.
func (h *Host) SetInitError(msg string) { h.initFail.Store(&msg) }

func (h *Host) InitError() string {
	if p := h.initFail.Load(); p != nil {
		return *p
	}
	return ""
}
