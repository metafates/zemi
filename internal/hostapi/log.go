package hostapi

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

// logNamespace builds the zemi.log member table; zemi.log(msg) is
// zemi.log.info(msg) (DESIGN §6.1).
func (h *Host) logNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("debug", L.NewFunction(h.logFn(slog.LevelDebug)))
	tb.RawSetString("info", L.NewFunction(h.logFn(slog.LevelInfo)))
	tb.RawSetString("warn", L.NewFunction(h.logFn(slog.LevelWarn)))
	tb.RawSetString("error", L.NewFunction(h.logFn(slog.LevelError)))

	// list() is the /messages ring: a widget reads the recent lines from
	// here, never the per-launch log file.
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, e := range slices.Backward(h.messages()) {
			rows = append(rows, map[string]any{
				"at": e.at.Format(time.TimeOnly), "ts": float64(e.at.Unix()),
				"source": e.source, "message": e.text,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// logFn logs the message with an optional context table after it.
func (h *Host) logFn(level slog.Level) glua.LGFunction {
	return func(L *glua.LState) int {
		msg := L.CheckString(1)
		origin := string(h.RT.CurrentOrigin(L))
		h.Log.Log(context.Background(), level, msg, logArgs(L, origin, 2)...)
		h.RecordMessage(origin+" "+strings.ToLower(level.String()), msg)
		return 0
	}
}

const msgLogMax = 100

type msgEntry struct {
	at     time.Time
	source string
	text   string
}

type msgLog struct {
	mu      sync.Mutex
	entries []msgEntry
}

// RecordMessage keeps one print or zemi.log line for /messages. The file
// log holds them all, but it is per launch and the agent may not read it,
// so a print from a keymap, a widget render or a timer would otherwise be
// invisible while zemi runs.
func (h *Host) RecordMessage(source, text string) {
	l := &h.msgs
	l.mu.Lock()
	l.entries = appendLast(l.entries, msgEntry{h.now(), source, sanitize.String(text)}, msgLogMax)
	l.mu.Unlock()
}

func (h *Host) messages() []msgEntry {
	l := &h.msgs
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// OpenMessages runs /messages: the recent print and zemi.log lines,
// newest first.
func (h *Host) OpenMessages() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range slices.Backward(h.messages()) {
		b.WriteString(e.at.Format(time.TimeOnly) + " " + e.source + "  " + e.text + "\n")
	}
	o := Overlay{ID: "messages", Title: "Messages", Origin: lua.OriginBuiltin, Body: strings.TrimRight(b.String(), "\n")}
	if o.Body == "" {
		o.Empty = "Nothing printed since zemi started."
	}
	ui.OpenOverlay(o)
	return nil
}

// logArgs is origin and the optional context table at idx as key/value
// pairs, the table's keys sorted and prefixed "ctx." so none can pose as
// the record's level, msg or origin.
func logArgs(L *glua.LState, origin string, idx int) []any {
	args := []any{"origin", origin}
	if L.GetTop() < idx || L.Get(idx) == glua.LNil {
		return args
	}
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("zemi.log: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		L.RaiseError("zemi.log: context must be a table of key/values")
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		args = append(args, "ctx."+k, m[k])
	}
	return args
}
