package hostapi

import (
	"slices"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

// logNamespace builds the zemi.log member table; logCall is the __call
// dispatch so `zemi.log(msg)` = `zemi.log.info(msg)` (DESIGN §6.1).
func (h *Host) logNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("debug", L.NewFunction(h.logFn(logging.Debug, 1)))
	tb.RawSetString("info", L.NewFunction(h.logFn(logging.Info, 1)))
	tb.RawSetString("warn", L.NewFunction(h.logFn(logging.Warn, 1)))
	tb.RawSetString("error", L.NewFunction(h.logFn(logging.Error, 1)))

	// list() is the /messages ring: the log file is per launch and outside
	// the read jails, so a widget reads the recent lines from here.
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
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

// logCall skips the table, which __call passes as the first argument.
func (h *Host) logCall(L *glua.LState) *glua.LFunction {
	return L.NewFunction(h.logFn(logging.Info, 2))
}

// logFn logs the message at arg first with an optional context table after it.
func (h *Host) logFn(level logging.Level, first int) glua.LGFunction {
	return func(L *glua.LState) int {
		msg := L.CheckString(first)
		origin := string(h.RT.CurrentOrigin(L))
		h.Log.Log(level, origin, msg, logFields(L, first+1))
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
	l.entries = append(l.entries, msgEntry{h.now(), source, sanitize.String(text)})
	if len(l.entries) > msgLogMax {
		l.entries = l.entries[len(l.entries)-msgLogMax:]
	}
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

// logFields converts an optional trailing context table to log fields.
func logFields(L *glua.LState, idx int) logging.Fields {
	if L.GetTop() < idx || L.Get(idx) == glua.LNil {
		return nil
	}
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("zemi.log: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		L.RaiseError("zemi.log: context must be a table of key/values")
	}
	return logging.Fields(m)
}
