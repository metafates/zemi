// eventlog.go keeps what happened for Lua reflection: every host event is
// counted and the recent ones kept whether or not anything listened
// (zemi.event.log, zemi.event.stats), and the recent notifications are
// listed (zemi.notify.list), so a widget can show a session's activity.

package hostapi

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

const (
	eventLogMax  = 500
	notifyLogMax = 100
)

type eventEntry struct {
	at      time.Time
	name    string
	from    string
	payload map[string]any
}

type notifyEntry struct {
	at                 time.Time
	level, title, body string
}

type eventLog struct {
	mu      sync.Mutex
	entries []eventEntry
	counts  map[string]int
	notes   []notifyEntry
}

// recordEvent counts an emission and keeps it; the streaming deltas are
// counted only, one per token would push everything else out.
func (h *Host) recordEvent(name, from string, payload map[string]any) {
	l := &h.evlog
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = map[string]int{}
	}
	l.counts[name]++
	if strings.HasSuffix(name, "_delta") {
		return
	}
	l.entries = append(l.entries, eventEntry{h.now(), name, from, payload})
	if len(l.entries) > eventLogMax {
		l.entries = l.entries[len(l.entries)-eventLogMax:]
	}
}

func (h *Host) recordNotify(level slog.Level, title, body string) {
	l := &h.evlog
	l.mu.Lock()
	l.notes = append(l.notes, notifyEntry{h.now(), notifyName(level), title, body})
	if len(l.notes) > notifyLogMax {
		l.notes = l.notes[len(l.notes)-notifyLogMax:]
	}
	l.mu.Unlock()
}

// eventLogFn is zemi.event.log([{ name = ..., limit = n }]): recent events
// newest first, each the table its handlers saw plus at and ts.
func (h *Host) eventLogFn(L *glua.LState) int {
	var name string
	limit := eventLogMax
	if opts, ok := luaToGoChecked(L, 1, "zemi.event.log").(map[string]any); ok {
		name = strField(opts, "name")
		if n, ok := opts["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
	}
	l := &h.evlog
	l.mu.Lock()
	entries := slices.Clone(l.entries)
	l.mu.Unlock()
	rows := make([]any, 0, min(limit, len(entries)))
	for _, e := range slices.Backward(entries) {
		if name != "" && e.name != name || len(rows) == limit {
			continue
		}
		ev := eventTable(e.name, e.from, e.payload)
		ev["at"], ev["ts"] = e.at.Format(time.TimeOnly), float64(e.at.UnixMilli())/1000
		rows = append(rows, ev)
	}
	L.Push(lua.FromGo(L, rows))
	return 1
}

// eventStatsFn is zemi.event.stats(): emissions per event name since
// start, the deltas included.
func (h *Host) eventStatsFn(L *glua.LState) int {
	l := &h.evlog
	l.mu.Lock()
	counts := make(map[string]any, len(l.counts))
	for k, v := range l.counts {
		counts[k] = v
	}
	l.mu.Unlock()
	L.Push(lua.FromGo(L, counts))
	return 1
}

// notifyListFn is zemi.notify.list(): recent notifications newest first.
func (h *Host) notifyListFn(L *glua.LState) int {
	l := &h.evlog
	l.mu.Lock()
	notes := slices.Clone(l.notes)
	l.mu.Unlock()
	rows := make([]any, 0, len(notes))
	for _, n := range slices.Backward(notes) {
		rows = append(rows, map[string]any{
			"at": n.at.Format(time.TimeOnly), "ts": float64(n.at.UnixMilli()) / 1000,
			"level": n.level, "title": n.title, "body": n.body,
		})
	}
	L.Push(lua.FromGo(L, rows))
	return 1
}
