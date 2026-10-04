package ui

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// fail is "" when ok, and what the check saw otherwise: a check states its own
// condition, where a map keyed by it can have its polarity flipped into a pass.
func fail(ok bool, seen any) string {
	if ok {
		return ""
	}
	return fmt.Sprint(seen)
}

// TestToggleOverlayStaysOpen: in a Reload overlay (skills) space and enter
// run the row and rebuild the list in place, keeping the selection and the
// box's size; ctrl+n moves like j. An open filter takes space as text, so a
// query of two words toggles nothing, and a rebuild with fewer rows keeps it.
func TestToggleOverlayStaysOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	on := false
	items := func() []any {
		mark := "○ b"
		if on {
			mark = "● b"
		}
		return []any{map[string]any{"text": "a"}, map[string]any{"text": mark}, "c", "d", "e"} // filterMin rows
	}
	a.openOverlay(hostapi.Overlay{ID: "t", Origin: lua.OriginBuiltin, Items: items(), Reload: func() ([]any, [][]hostapi.Segment) { on = !on; return items(), nil }})
	a.overlayKey("ctrl+n", "")
	a.overlays[0].size = boxSize{60, 12}
	for _, key := range []string{"space", "enter"} {
		wasOn := on
		a.overlayKey(key, map[string]string{"space": " "}[key])
		if len(a.overlays) != 1 || a.overlays[0].selected != 1 || on == wasOn || a.overlays[0].size != (boxSize{60, 12}) {
			t.Fatalf("%s: overlays=%d selected=%d on=%v size=%v", key, len(a.overlays), a.overlays[0].selected, on, a.overlays[0].size)
		}
	}
	if got := a.overlays[0].labels[1]; got != "○ b" {
		t.Fatalf("row=%q", got)
	}
	a.overlayKey("", "/")
	a.overlayKey("", "b")
	wasOn := on
	a.overlayKey("space", " ")
	if st := a.overlays[0]; !st.active || st.query != "b " || len(st.visible) != 1 || on != wasOn {
		t.Fatalf("space in a filter: on %v -> %v, %+v", wasOn, on, st.listFilter)
	}
	a.openOverlay(hostapi.Overlay{ID: "t", Origin: lua.OriginBuiltin, Items: items()[:filterMin-1]})
	if !a.overlays[0].filterable {
		t.Fatal("a rebuild to fewer than filterMin rows dropped the filter row")
	}
}

// TestReloadClosesLuaOverlays: an overlay the interpreter owns must not
// survive a reload. Its callbacks are dropped as stale, so leaving it on
// screen reads as a frozen picker; Go-built ones keep working and stay.
func TestReloadClosesLuaOverlays(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openOverlay(hostapi.Overlay{ID: "lua", Origin: lua.OriginUser, Gen: 1, Items: []any{"a"}})
	a.openOverlay(hostapi.Overlay{ID: "go", Origin: lua.OriginBuiltin, Items: []any{"b"}})

	a.host.PurgeGeneration()
	if len(a.overlays) != 1 || a.overlays[0].o.ID != "go" {
		t.Fatalf("overlays after purge: %+v", a.overlays)
	}
}

// TestOverlayFilter: "/" types a query that narrows the rows (all words,
// any case, a row's section and value too), enter keeps it and gives the
// keys back to the list, esc clears it before closing, and a new query
// selects its first match.
func TestOverlayFilter(t *testing.T) {
	var fixture []any
	for _, s := range []string{"alpha one", "Beta two", "gamma three", "xyz", "qqq"} { // filterMin rows
		fixture = append(fixture, map[string]any{"text": s, "fill": s})
	}
	hotkeyed := slices.Clone(fixture)
	hotkeyed[2] = map[string]any{"text": "gamma three", "fill": "gamma three", "hotkey": "3"}
	models := []any{
		map[string]any{"text": "gpt", "section": "openai"},
		map[string]any{"text": "sonnet", "section": "anthropic"},
		map[string]any{"text": "opus", "section": "anthropic", "copy": "anthropic/opus"},
		map[string]any{"text": "llama", "section": "meta"},
		map[string]any{"text": "qwen", "section": "meta"},
	}
	runes := func(s string, keys ...string) []string { return append(strings.Split(s, ""), keys...) }
	state := func(a *App, active bool, query string, rows, sel int) string {
		if len(a.overlays) != 1 {
			return fmt.Sprintf("%d overlays", len(a.overlays))
		}
		st := a.overlays[0]
		if st.active != active || st.query != query || len(st.visible) != rows || st.selected != sel {
			return fmt.Sprintf("active=%v query=%q rows=%d selected=%d", st.active, st.query, len(st.visible), st.selected)
		}
		return ""
	}
	closedWith := func(a *App, composer string) string {
		if len(a.overlays) != 0 || a.composer.text != composer {
			return fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text)
		}
		return ""
	}
	for _, c := range []struct {
		name  string
		o     hostapi.Overlay
		keys  []string
		check func(a *App) string
	}{
		{"typing narrows", hostapi.Overlay{}, runes("/t"), func(a *App) string { return state(a, true, "t", 2, 0) }},
		{"every word must match, backspace widens", hostapi.Overlay{}, runes("/two b", "backspace", "backspace"), func(a *App) string { return state(a, true, "two", 1, 0) }},
		{"enter keeps the query and gives j to the list", hostapi.Overlay{}, runes("/t", "enter", "j"), func(a *App) string { return state(a, false, "t", 2, 1) }},
		{"esc while typing clears and keeps the row", hostapi.Overlay{}, runes("/ga", "esc"), func(a *App) string { return state(a, false, "", 5, 2) }},
		{"esc on a kept query clears, a second closes", hostapi.Overlay{}, runes("/t", "enter", "esc", "esc"), func(a *App) string { return closedWith(a, "") }},
		{"/ on a kept query types on", hostapi.Overlay{}, runes("/t", "enter", "/", "w"), func(a *App) string { return state(a, true, "tw", 1, 0) }},
		{"backspace on an empty query stops typing", hostapi.Overlay{}, runes("/", "backspace"), func(a *App) string { return state(a, false, "", 5, 0) }},
		{"j moves while no filter types", hostapi.Overlay{}, runes("jj"), func(a *App) string { return state(a, false, "", 5, 2) }},
		{"enter on a kept query acts on the row", hostapi.Overlay{}, runes("/e", "enter", "enter"), func(a *App) string { return closedWith(a, "alpha one") }},
		{"a typing filter takes the hotkey", hostapi.Overlay{Items: hotkeyed}, runes("/3"), func(a *App) string { return state(a, true, "3", 0, 0) }},
		{"a hotkey runs its row", hostapi.Overlay{Items: hotkeyed}, runes("3"), func(a *App) string { return closedWith(a, "gamma three") }},
		{"/ on a list of fewer than filterMin rows is inert and draws no filter row", hostapi.Overlay{Items: []any{"a", "b"}}, runes("/"), func(a *App) string {
			if box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20)); strings.Contains(box, "filter") {
				return box
			}
			return state(a, false, "", 2, 0)
		}},
		{"a section name narrows to its rows and drops the other header", hostapi.Overlay{Items: models}, runes("/anthropic"), func(a *App) string {
			if v := a.overlays[0].visible; !slices.Equal(v, []int{1, 2}) || strings.Contains(fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20)), "openai") {
				return fmt.Sprint(v)
			}
			return ""
		}},
		{"a row's value matches too, so a full model id finds its row", hostapi.Overlay{Items: models}, runes("/anthropic/op"), func(a *App) string {
			return fail(slices.Equal(a.overlays[0].visible, []int{2}), fmt.Sprint(a.overlays[0].visible))
		}},
		{"a one-token label matches fuzzily, and says where", hostapi.Overlay{Items: models}, runes("/snt"), func(a *App) string {
			st := a.overlays[0]
			return fail(slices.Equal(st.visible, []int{1}) && fmt.Sprint(st.match) == "[[0 2 5]]", fmt.Sprint(st.visible, st.match))
		}},
		{"a render_item row keeps its spaces, so the highlight lands on what matched", hostapi.Overlay{Items: []any{"x", "q1", "q2", "q3", "q4"}, ItemText: [][]hostapi.Segment{{{Text: "●  "}, {Text: "sonnet", HL: "Ok"}}}}, runes("/net"), func(a *App) string {
			var lit []string
			for _, row := range a.overlayBox(&a.overlays[0], 80, 20) {
				for _, s := range row {
					if s.HL == "Host:Title" && s.Text != "› " && strings.Contains(fmt.Sprint(row), "●") {
						lit = append(lit, s.Text)
					}
				}
			}
			return fail(slices.Equal(lit, []string{"net"}), fmt.Sprint(lit))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			c.o.ID, c.o.Origin = "f", lua.OriginBuiltin
			if c.o.Items == nil {
				c.o.Items = fixture
			}
			a.openOverlay(c.o)
			for _, k := range c.keys {
				text := map[string]string{"space": " "}[k]
				if len([]rune(k)) == 1 {
					text = k
				}
				a.overlayKey(k, text)
			}
			if msg := c.check(a); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// TestTopChromeTakesInput: keys, ctrl+c and the wheel reach the prompt
// drawn on top, never one hidden under it.
func TestTopChromeTakesInput(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	perm := make(chan permAnswer, 1)
	a.trust = &trustPrompt{answer: make(chan string, 1)}
	a.permAsk = &permAskPrompt{answer: perm}
	a.handleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if a.permAsk != nil || a.trust == nil || (<-perm).ok {
		t.Fatal("n should deny the permission prompt drawn over the trust prompt")
	}

	a.openOverlay(hostapi.Overlay{Items: []any{"a", "b", "c"}})
	a.permAsk = &permAskPrompt{answer: perm}
	a.wheel(0, 0, 1)
	if a.permAsk.sel != 1 || a.overlays[0].selected != 0 {
		t.Fatalf("wheel: prompt sel=%d overlay sel=%d", a.permAsk.sel, a.overlays[0].selected)
	}
}

// pressWhileLuaBusy attaches a program that never runs, so Lua work goes to
// the worker, and presses keys while the interpreter is held; it fails when
// a key waits on Lua. release frees the interpreter.
func pressWhileLuaBusy(t *testing.T, app *App, keys ...tea.KeyPressMsg) (release func()) {
	t.Helper()
	app.program = tea.NewProgram(nil)
	busy, hold := make(chan struct{}), make(chan struct{})
	go app.host.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(*glua.LState) ([]any, error) {
		close(busy)
		<-hold
		return nil, nil
	})
	<-busy
	done := make(chan struct{})
	go func() {
		for _, k := range keys {
			app.Update(k)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(hold)
		t.Fatal("a key waited on the interpreter")
	}
	return func() { close(hold) }
}

// TestOverlaySelectOffKeyLoop: enter on a Lua picker returns while the
// interpreter is busy; on_choice runs on the background worker.
func TestOverlaySelectOffKeyLoop(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.ui.select({ "a" }, {}, function() end)`)
	defer pressWhileLuaBusy(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})()
	if len(app.overlays) != 0 {
		t.Fatal("the picker should close at once")
	}
}

// TestPaletteLuaCompleterOffKeyLoop: typing past "/name " returns while the
// interpreter is busy, and the Lua completer's answer fills the palette once
// it is free.
func TestPaletteLuaCompleterOffKeyLoop(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.command.add("greet", function() end, { nargs = "?", complete = function() return { "alice", "bob" } end })`)
	var keys []tea.KeyPressMsg
	for _, r := range "/greet b" {
		keys = append(keys, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	pressWhileLuaBusy(t, app, keys...)()
	deadline := time.After(2 * time.Second)
	for !app.palette.open {
		select {
		case msg := <-app.hooks:
			app.Update(msg)
		case <-deadline:
			t.Fatal("the completer never answered")
		}
	}
	if len(app.palette.args) != 1 || app.palette.args[0].label != "bob" {
		t.Fatalf("palette args %v", app.palette.args)
	}
	// the next key keeps the popup and its size until the completer answers
	app.renderSync(t)
	size := app.palette.size
	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if !app.palette.open || app.palette.size != size {
		t.Fatalf("open=%v size %v, was %v", app.palette.open, app.palette.size, size)
	}
}

// TestTopBoxJoinsAlone: the sides of a dimmed box running into the top
// box's borders do not join them.
func TestTopBoxJoinsAlone(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	items := make([]any, 14)
	for i := range items {
		items[i] = fmt.Sprintf("item %d", i)
	}
	app.openOverlay(hostapi.Overlay{ID: "list", Title: "List", Items: items})
	app.openOverlay(hostapi.Overlay{ID: "in", Title: strings.Repeat("a long title ", 6), Input: true})
	app.renderSync(t)
	if f := ansi.Strip(app.Frame()); strings.ContainsAny(f, "┬┴├┤┼") {
		t.Fatalf("the top box joined the dimmed one under it:\n%s", f)
	}
}

// TestModalDimsBackground: a modal sinks what lies under it toward the
// canvas; a toast and the palette are not modal and do not.
// TestBoxAreaFallsBackToTheScreen: a box centers on the stage, or on the
// whole screen when the stage cannot give it 40 columns and 8 rows, which is
// what a prompt on a small terminal stands on.
func TestBoxAreaFallsBackToTheScreen(t *testing.T) {
	screen := uv.Rect(0, 0, 120, 30)
	for _, c := range []struct {
		name        string
		stage, want uv.Rectangle
	}{
		{"room", uv.Rect(4, 2, 110, 20), uv.Rect(4, 2, 110, 20)},
		{"the least that fits", uv.Rect(4, 2, 40, 8), uv.Rect(4, 2, 40, 8)},
		{"too narrow", uv.Rect(4, 2, 39, 20), screen},
		{"too few rows", uv.Rect(4, 2, 110, 7), screen},
	} {
		if got := boxArea(c.stage, 120, 30); got != c.want {
			t.Errorf("%s: boxArea = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestModalDimsBackground(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "hello there")
	for i := range 20 { // a chat full of text for the box to stand over
		appendReply(host, fmt.Sprintf("answer %d %s", i, strings.Repeat("xyz ", 17)), hostapi.TurnStat{})
	}
	glyph := func(row []hostapi.Segment, x int) rune {
		cells := dropCells(row, x)
		if len(cells) == 0 {
			return ' '
		}
		c, _ := utf8.DecodeRuneInString(cells[0].Text)
		return c
	}
	runes := func(rows [][]hostapi.Segment) [][]rune {
		out := make([][]rune, len(rows))
		for y, row := range rows {
			out[y] = make([]rune, 0, app.width)
			for x := range app.width {
				out[y] = append(out[y], glyph(row, x))
			}
		}
		return out
	}
	app.renderSync(t)
	app.Frame()
	backdrop := runes(app.lastRows)
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"a"}})
	app.pushToast(slog.LevelInfo, "Copied to the clipboard", "")
	seg := func(text string) hostapi.Segment {
		app.renderSync(t)
		app.Frame()
		for _, row := range app.lastRows {
			for _, s := range row {
				if strings.Contains(s.Text, text) {
					return s
				}
			}
		}
		t.Fatalf("%q not on screen", text)
		return hostapi.Segment{}
	}
	if s := seg("hello"); s.Fade != modalFade || s.HL == hostapi.ChromeDim {
		t.Fatalf("under an overlay: %+v", s)
	}
	// a box covers its own cells and nothing else: every glyph the backdrop drew
	// outside it stays where it was, so no word of the chat is cut short, and the
	// cells beside it keep the backdrop's own style, so no stripe of the
	// terminal's background digs into what the box stands over
	r := app.chromeRect
	for y := r.Min.Y; y < r.Max.Y && y < len(app.lastRows); y++ {
		for x := range app.width {
			if x >= r.Min.X && x < r.Max.X {
				continue
			}
			if got := glyph(app.lastRows[y], x); got != backdrop[y][x] {
				t.Fatalf("the box rewrote the backdrop at %d,%d: %q, was %q", x, y, got, backdrop[y][x])
			}
		}
	}
	if s := dropCells(app.lastRows[r.Min.Y+1], r.Min.X-1)[0]; s.HL == "" || s.Fade != modalFade {
		t.Fatalf("the chat beside the box: %+v", s)
	}
	if s := seg("Copied"); s.Fade != 0 {
		t.Fatalf("the overlay faded a toast: %+v", s)
	}
	// a box behind the modal sinks to the canvas with the rest of the backdrop
	app.overlays = nil
	under := make([]any, 30)
	for i := range under {
		under[i] = fmt.Sprintf("under %02d", i)
	}
	app.openOverlay(hostapi.Overlay{ID: "under", Title: "U", Origin: lua.OriginBuiltin, Items: under})
	app.renderSync(t)
	app.Frame()
	app.openOverlay(hostapi.Overlay{ID: "t2", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"a"}})
	if s := seg("under 00"); s.OnPanel || s.Fade != modalFade {
		t.Fatalf("a box behind the modal: %+v", s)
	}
	app.overlays = nil
	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if s := seg("untrusted"); s.Fade != 0 {
		t.Fatalf("the palette faded the screen: %+v", s)
	}
}

// TestToastOverBox: the tray draws over every window, a box included: on a
// screen too short for one the box is the whole screen, so the tray lands on
// the box's own rows and must still be the text drawn there.
func TestToastOverBox(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 10)
	app.openOverlay(hostapi.Overlay{ID: "list", Title: "List", Origin: lua.OriginBuiltin, Items: []any{"a", "b", "c"}})
	app.pushToast(slog.LevelInfo, "Copied to the clipboard", "")
	app.renderSync(t)
	app.Frame()
	y := slices.IndexFunc(app.lastRows, func(row []hostapi.Segment) bool {
		return strings.Contains(hostapi.SegsText(row), "Copied to the clipboard")
	})
	if y < 0 {
		t.Fatalf("the box drew over the tray:\n%s", ansi.Strip(app.Frame()))
	}
	// the box's own rows, which the tray must sit inside for the check above
	// to mean anything
	if r := app.chromeRect; y < r.Min.Y || y >= r.Max.Y {
		t.Fatalf("the box %v does not reach the tray's row %d", r, y)
	}
}

// TestBootFailedBox: a boot that failed leaves a runtime with no theme, no
// widgets and no Lua at all, where the only screen left is Go's. The box
// states the error and the log, the composer gives way to it, `c` copies the
// error, and a boot that succeeds takes it away.
func TestBootFailedBox(t *testing.T) {
	app, host := bootAgentApp(t, 90, 24)
	host.PurgeGeneration() // what a failed boot leaves behind: no theme, no widgets
	app.SignalBoot(errors.New("boot step zemi.tui: zemi/tui/init.lua:12: attempt to call a nil value (global 'pending')"))
	for app.Pump() {
	}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"Boot failed", "boot step zemi.tui:", "The whole traceback is in the log", "c copy the error", "q quit"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the box does not say %q:\n%s", want, frame)
		}
	}
	// the log path wraps across the box's rows, its border between them, so the
	// screen is read flat
	flat := strings.NewReplacer("\r", "", "\n", "", " ", "", "│", "").Replace(frame)
	if !strings.Contains(flat, perm.TildePath(host.Dirs.LogDir())) {
		t.Fatalf("the box names no log:\n%s", frame)
	}
	// the screen is the box: no composer stands under it, and no key reaches one
	if strings.Contains(frame, "Ask anything") {
		t.Fatalf("the composer is still drawn:\n%s", frame)
	}
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if app.composer.text != "" {
		t.Fatalf("a key reached the composer under the boot box: %q", app.composer.text)
	}
	app.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	for app.Pump() {
	}
	if len(app.toasts) == 0 || !strings.Contains(app.toasts[len(app.toasts)-1].title, "Copied") {
		t.Fatalf("copying the error: %+v", app.toasts)
	}
	// a boot that succeeds takes the box away and gives the screen back
	app.SignalBoot(nil)
	for app.Pump() {
	}
	app.renderSync(t)
	if f := ansi.Strip(app.Frame()); strings.Contains(f, "Boot failed") || !strings.Contains(f, "Ask anything") {
		t.Fatalf("the screen after a good boot:\n%s", f)
	}
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if app.composer.text != "x" {
		t.Fatalf("the composer is still captured after a good boot: %q", app.composer.text)
	}
}

func TestOverlayNavigation(t *testing.T) {
	detail := func(actions ...any) map[string]any {
		return map[string]any{"text": "err", "desc": "short", "detail": "line one\nline two", "actions": actions}
	}
	reloads, spaced, backs := 0, 0, 0
	for _, c := range []struct {
		name  string
		o     hostapi.Overlay
		keys  []string
		check func(a *App) string // "" when the state is right
	}{
		{
			"enter on an inert row keeps the box open",
			hostapi.Overlay{Items: []any{"a", "b"}},
			[]string{"enter"},
			func(a *App) string { return fail(len(a.overlays) == 1, "closed") },
		},
		{
			"enter on a copy row toasts and keeps the list open",
			hostapi.Overlay{Items: []any{map[string]any{"text": "deepseek", "copy": "export X"}}},
			[]string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || len(a.toasts) != 1 || a.toasts[0].title != "Copied" {
					return fmt.Sprintf("overlays=%d toasts=%+v", len(a.overlays), a.toasts)
				}
				return ""
			},
		},
		{
			"y copies a row's value in place of its text, and enter still runs the row",
			hostapi.Overlay{Items: []any{map[string]any{"text": "gpt-5", "desc": "400k", "run": "nope", "copy": "openai/gpt-5\x1b[2J"}}},
			nil,
			func(a *App) string {
				batch, _ := a.overlayKey("y", "")().(tea.BatchMsg)
				if got := fmt.Sprint(batch[0]()); got != "openai/gpt-5" {
					return fmt.Sprintf("y copied %q", got)
				}
				a.overlayKey("enter", "")
				return fail(len(a.overlays) == 0, "enter did not run the row")
			},
		},
		{
			"enter on a copy list's row runs the form it names, not the hint's word",
			hostapi.Overlay{ID: "copy", Origin: lua.OriginBuiltin, Verb: "copy", Items: []any{map[string]any{"text": "Whole reply", "run": "copy reply"}}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "copy", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil },
				})
				a.overlayKey("enter", "")
				if ran != "reply" || len(a.overlays) != 0 {
					return fmt.Sprintf("ran %q, overlays=%d", ran, len(a.overlays))
				}
				return ""
			},
		},
		{
			"an empty overlay shows its empty line and enter does nothing",
			hostapi.Overlay{Empty: "Nothing to see."},
			[]string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || !strings.Contains(fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20)), "Nothing to see.") {
					return "no empty line"
				}
				return ""
			},
		},
		{
			"an empty list under a body keeps its empty line",
			hostapi.Overlay{Body: "header", Empty: "Nothing cached yet."},
			nil,
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20))
				return fail(strings.Contains(box, "header") && strings.Contains(box, "Nothing cached yet."), box)
			},
		},
		{
			"a reload list whose rows only open says nothing of space",
			hostapi.Overlay{Items: []any{detail()}, Reload: func() ([]any, [][]hostapi.Segment) { return nil, nil }},
			nil,
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20))
				return map[bool]string{true: box}[strings.Contains(box, "toggle")]
			},
		},
		{
			"enter opens the detail and esc goes back with the selection kept",
			hostapi.Overlay{Title: "Errors", Items: []any{"a", detail()}},
			[]string{"down", "enter", "esc"},
			func(a *App) string {
				if len(a.overlays) != 1 || a.overlays[0].selected != 1 {
					return fmt.Sprintf("overlays=%d selected=%d", len(a.overlays), a.overlays[0].selected)
				}
				return ""
			},
		},
		{
			"going back from a toggle list's detail, by esc or an inert action, does not rebuild it",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Nothing"})}, Reload: func() ([]any, [][]hostapi.Segment) { backs++; return nil, nil }},
			[]string{"enter", "esc", "enter", "enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && backs == 0, fmt.Sprintf("overlays=%d rebuilds=%d", len(a.overlays), backs))
			},
		},
		{
			"the detail view has a breadcrumb title",
			hostapi.Overlay{Title: "Errors", Items: []any{detail()}},
			[]string{"enter"},
			func(a *App) string {
				d := a.overlays[len(a.overlays)-1]
				if len(a.overlays) != 2 || d.o.Title != "Errors / err" || !d.back {
					return fmt.Sprintf("%+v", d)
				}
				return ""
			},
		},
		{
			"a detail's prose is never a field",
			hostapi.Overlay{Items: []any{map[string]any{"text": "trust", "detail": "trusted: ~/x"}}},
			[]string{"enter"},
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20))
				return fail(strings.Contains(box, "trusted: ~/x"), box)
			},
		},
		{
			"an item's fields are set as a table, and y copies them as lines",
			hostapi.Overlay{Items: []any{map[string]any{"text": "mcp", "detail": "prose", "fields": []any{[]any{"status", "ready"}}}}},
			[]string{"enter"},
			func(a *App) string {
				if box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20)); !strings.Contains(box, "{ready Host:Ok") {
					return box
				}
				batch, _ := a.overlayKey("y", "")().(tea.BatchMsg)
				return fail(fmt.Sprint(batch[0]()) == "status: ready\n\nprose", fmt.Sprintf("y copied %q", fmt.Sprint(batch[0]())))
			},
		},
		{
			"a detail view takes its own height and widens to its breadcrumb",
			hostapi.Overlay{Title: "A long parent title", Items: []any{map[string]any{"text": strings.Repeat("x", 50), "detail": "d"}, "b", "c", "d", "e", "f", "g"}},
			[]string{"enter"},
			func(a *App) string {
				list, detail := a.overlayBox(&a.overlays[0], 120, 20), a.overlayBox(&a.overlays[1], 120, 20)
				if len(detail) >= len(list) || hostapi.SegsWidth(detail[0]) <= hostapi.SegsWidth(list[0]) || strings.Contains(hostapi.SegsText(detail[0]), "…") {
					return fmt.Sprintf("list %d rows %d wide, detail %d rows %q", len(list), hostapi.SegsWidth(list[0]), len(detail), hostapi.SegsText(detail[0]))
				}
				return ""
			},
		},
		{
			"a field repeating the breadcrumb shows only when the title is cut",
			hostapi.Overlay{Title: "Projects", Items: []any{map[string]any{"text": "~/code/app", "fields": []any{[]any{"trust", "trusted"}, []any{"path", "~/code/app"}}}}},
			[]string{"enter"},
			func(a *App) string {
				body := func(w int) string {
					var out []string
					for _, r := range a.overlayBox(&a.overlays[1], w, 20) {
						out = append(out, hostapi.SegsText(r))
					}
					return strings.Join(out, "\n")
				}
				if wide, narrow := body(120), body(24); strings.Contains(wide, "path") || !strings.Contains(narrow, "path") {
					return fmt.Sprintf("wide:\n%s\nnarrow:\n%s", wide, narrow)
				}
				return ""
			},
		},
		{
			"the wheel scrolls a detail body with actions, and the box keeps its width",
			hostapi.Overlay{Title: "Errors", Items: []any{detail(map[string]any{"text": "Ask", "fill": "x"}, map[string]any{"text": "Back"})}},
			[]string{"enter"},
			func(a *App) string {
				d := &a.overlays[1]
				d.o.Body = strings.Repeat("line\n", 60)
				before := hostapi.SegsWidth(a.overlayBox(d, 80, 20)[0])
				a.wheel(0, 0, 1)
				d.selected = 1
				if after := hostapi.SegsWidth(a.overlayBox(d, 80, 20)[0]); d.body.top != 1 || after != before {
					return fmt.Sprintf("scroll=%d width %d -> %d", d.body.top, before, after)
				}
				return ""
			},
		},
		{
			"a detail view draws no filter, so typing hides no action",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Ask", "fill": "x"})}},
			[]string{"enter"},
			func(a *App) string {
				for _, k := range []string{"/", "x"} {
					a.overlayKey(k, k)
				}
				d := a.overlays[len(a.overlays)-1]
				return fail(len(d.visible) == 1 && !d.active, fmt.Sprintf("visible=%v filtering=%v", d.visible, d.active))
			},
		},
		{
			"an action that fills closes the stack",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Use", "fill": "/use "})}},
			[]string{"enter", "enter"},
			func(a *App) string {
				if len(a.overlays) != 0 || a.composer.text != "/use " {
					return fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text)
				}
				return ""
			},
		},
		{
			"an action with nothing to do goes back",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Cancel"})}},
			[]string{"enter", "enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && !a.overlays[0].back, "not back")
			},
		},
		{
			"a plugin's run action goes back, as its hint says",
			hostapi.Overlay{Origin: lua.OriginPlugin, Items: []any{detail(map[string]any{"text": "Restart", "run": "help"})}},
			[]string{"enter", "enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && !a.overlays[0].back, "not back")
			},
		},
		{
			"a row that opens a screen leaves its list under it, for esc to come back to",
			hostapi.Overlay{ID: "m", Items: []any{map[string]any{"text": "Choose a model per role", "verb": "open", "run": "nope"}}},
			[]string{"enter"},
			func(a *App) string { return fail(len(a.overlays) == 1, "closed") },
		},
		{
			"a run action in a toggle list rebuilds the list",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Toggle", "run": "nope"})}, Reload: func() ([]any, [][]hostapi.Segment) { reloads++; return []any{detail()}, nil }},
			[]string{"enter", "enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && reloads == 1, "not rebuilt")
			},
		},
		{
			"space passes by a toggle list row with its own verb",
			hostapi.Overlay{Items: []any{map[string]any{"text": "Choose a model per role", "verb": "open", "run": "nope"}}, Reload: func() ([]any, [][]hostapi.Segment) { spaced++; return nil, nil }},
			[]string{"space"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && spaced == 0, "space ran the row")
			},
		},
		{
			"a fill row closes a toggle list, so the composer has the keys",
			hostapi.Overlay{Items: []any{map[string]any{"text": "Install", "verb": "install", "fill": "/plugins install "}}, Reload: func() ([]any, [][]hostapi.Segment) { return nil, nil }},
			[]string{"enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 0 && a.composer.text == "/plugins install ", fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text))
			},
		},
		{
			"a toggle list rebuilds when the overlay stacked on it closes, after its pick",
			hostapi.Overlay{ID: "p", Items: []any{"old"}},
			nil,
			func(a *App) string {
				a.overlays[0].o.Reload = func() ([]any, [][]hostapi.Segment) { return []any{a.composer.text}, nil }
				a.openOverlay(hostapi.Overlay{ID: "c", Origin: lua.OriginBuiltin, Items: []any{map[string]any{"text": "pick", "fill": "new"}}})
				a.overlayKey("enter", "")
				return fail(len(a.overlays) == 1 && a.overlays[0].labels[0] == "new", fmt.Sprintf("overlays=%d", len(a.overlays)))
			},
		},
		{
			"opening an open id replaces it in place with the selection on its row, else at its place, unless a lower origin opens it",
			hostapi.Overlay{ID: "p", Items: []any{"a", "b"}},
			[]string{"down"},
			func(a *App) string {
				for _, c := range []struct {
					items []any
					sel   int
				}{{[]any{"x", "y", "b"}, 2}, {[]any{"c", "d", "e"}, 2}} {
					a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: c.items})
					if len(a.overlays) != 1 || a.overlays[0].selected != c.sel {
						return fmt.Sprintf("%v: overlays=%d selected=%d", c.items, len(a.overlays), a.overlays[0].selected)
					}
				}
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginPlugin, Items: []any{"e"}})
				return fail(len(a.overlays) == 2 && a.overlays[0].labels[0] == "c", "a plugin replaced a builtin list")
			},
		},
		{
			"a refresh replaces an open id and opens nothing once it closed",
			hostapi.Overlay{ID: "p", Items: []any{"a"}},
			[]string{"esc"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []any{"b"}, Refresh: true})
				return fail(len(a.overlays) == 0, "a closed list came back")
			},
		},
		{
			"a refresh of a closed list builds nothing and wakes no frame",
			hostapi.Overlay{ID: "p", Items: []any{"a"}},
			[]string{"esc"},
			func(a *App) string {
				gen, built := a.frameGen, false
				a.RefreshOverlay("p", func() hostapi.Overlay { built = true; return hostapi.Overlay{} })
				return fail(!built && a.frameGen == gen, fmt.Sprintf("built %v, frame moved %d", built, a.frameGen-gen))
			},
		},
		{
			"a refresh rebuilds the detail open over the list from its item, keeping the selected action, and drops it with the item",
			hostapi.Overlay{ID: "p", Items: []any{detail("a", "b"), "x"}},
			[]string{"enter", "down"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []any{"x", map[string]any{"text": "err", "detail": "line three", "actions": []any{"a", "b"}}}})
				if d := a.overlays[len(a.overlays)-1]; len(a.overlays) != 2 || d.o.Body != "line three" || d.selected != 1 {
					return fmt.Sprintf("overlays=%d body=%q selected=%d", len(a.overlays), d.o.Body, d.selected)
				}
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []any{"x"}})
				return fail(len(a.overlays) == 1, "the gone item's detail stayed")
			},
		},
		{
			"a refresh keeps an armed danger press while the same action sits under it",
			hostapi.Overlay{ID: "p", Items: []any{map[string]any{"text": "run", "detail": "d", "actions": []any{"Open", map[string]any{"text": "Stop", "hotkey": "x", "danger": true, "run": "stop 1"}}}}},
			[]string{"x"},
			func(a *App) string {
				item := func(run string) hostapi.Overlay {
					return hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []any{map[string]any{"text": "run", "detail": "d2", "actions": []any{"Open", map[string]any{"text": "Stop", "hotkey": "x", "danger": true, "run": run}}}}}
				}
				if a.openOverlay(item("stop 1")); !a.overlays[1].armed {
					return "the detail's arm was dropped"
				}
				a.openOverlay(item("stop 2"))
				return fail(!a.overlays[1].armed, "an arm outlived its action")
			},
		},
		{
			"a refresh keeps an armed danger row of the list itself",
			hostapi.Overlay{ID: "p", Items: []any{map[string]any{"text": "Stop", "danger": true, "run": "stop 1"}}},
			[]string{"enter"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []any{map[string]any{"text": "Stop", "danger": true, "run": "stop 1"}}})
				return fail(a.overlays[0].armed, "the list's arm was dropped")
			},
		},
		{
			"a Go screen's list key runs its command, a plugin list's none",
			hostapi.Overlay{ID: "p", Items: []any{"x"}, Keys: []hostapi.OverlayKey{{Key: "p", Desc: "pause", Run: "zz pause"}}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil },
				})
				if a.overlayKey("p", "p"); ran != "pause" {
					return "ran " + ran
				}
				ran = ""
				a.openOverlay(hostapi.Overlay{ID: "q", Origin: lua.OriginPlugin, Items: []any{"x"}, Keys: []hostapi.OverlayKey{{Key: "p", Desc: "pause", Run: "zz pause"}}})
				a.overlayKey("p", "p")
				return fail(ran == "", "a plugin list's key ran "+ran)
			},
		},
		{
			"a danger list key acts on its second press, which esc and a wheel drop and a refresh keeps",
			hostapi.Overlay{ID: "p", Items: []any{"x"}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil },
				})
				o := hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []any{"x"}, Keys: []hostapi.OverlayKey{{Key: "x", Desc: "stop workflow", Run: "zz stop", Danger: true}}}
				a.openOverlay(o)
				a.overlayKey("x", "x")
				a.overlayKey("esc", "")
				a.overlayKey("x", "x")
				a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
				if a.overlayKey("x", "x"); ran != "" || len(a.overlays) != 1 {
					return "the first press ran " + ran
				}
				if foot := hostapi.SegsText(a.overlayBox(&a.overlays[0], 80, 20)[3]); !strings.Contains(foot, "x again to stop workflow") {
					return "footer " + foot
				}
				o.Refresh = true
				a.openOverlay(o)
				a.overlayKey("x", "x")
				return fail(ran == "stop", "the second press ran "+ran)
			},
		},
		{
			"a wide box takes the widest a screen shows, whatever opened it",
			hostapi.Overlay{ID: "p", Items: []any{"x"}, Wide: true},
			nil,
			func(a *App) string {
				a.overlayBox(&a.overlays[0], 120, 30)
				return fail(a.overlays[0].size.w == boxInner(120), fmt.Sprintf("%d cells wide", a.overlays[0].size.w))
			},
		},
		{
			"closing the parent drops its detail view",
			hostapi.Overlay{ID: "p", Items: []any{detail()}},
			[]string{"enter"},
			func(a *App) string {
				a.closeOverlayByID("p")
				return fail(len(a.overlays) == 0, "detail left")
			},
		},
		{
			"ctrl+c on a detail view closes the whole menu",
			hostapi.Overlay{ID: "p", Items: []any{detail()}},
			[]string{"enter"},
			func(a *App) string {
				a.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
				return fail(len(a.overlays) == 0 && a.armed == armed{}, "still open")
			},
		},
		{
			"the list keys scroll a body with no list",
			hostapi.Overlay{Body: strings.Repeat("line\n", 50)},
			[]string{"down"},
			func(a *App) string { return fail(a.overlays[0].body.top == 1, "no scroll") },
		},
		{
			"a multi-line label stays on one row",
			hostapi.Overlay{Items: []any{"fix: empty input\n\nThe parser crashed."}},
			nil,
			func(a *App) string {
				return fail(a.overlays[0].items[0].label == "fix: empty input The parser crashed.", "label kept its newlines")
			},
		},
		{
			"a newline in the title or empty text adds no terminal line",
			hostapi.Overlay{Title: "a\nb", Empty: "p\nq"},
			nil,
			func(a *App) string {
				for _, row := range a.overlayBox(&a.overlays[0], 80, 20) {
					if strings.Contains(hostapi.SegsText(row), "\n") {
						return fmt.Sprintf("row %q holds a newline", hostapi.SegsText(row))
					}
				}
				return ""
			},
		},
		{
			"Selected picks the first item",
			hostapi.Overlay{Items: []any{"a", "b", "c"}, Selected: 2},
			nil,
			func(a *App) string { return fail(a.overlays[0].selected == 2, "wrong selection") },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			c.o.Origin = cmp.Or(c.o.Origin, lua.OriginBuiltin)
			a.openOverlay(c.o)
			for _, k := range c.keys {
				a.overlayBox(&a.overlays[len(a.overlays)-1], 80, 20) // keys act on the box drawn
				a.overlayKey(k, "")
			}
			if msg := c.check(a); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// TestOverlayHints: the footer names only keys that act on the selection,
// with one verb per act of enter's in every list.
func TestOverlayHints(t *testing.T) {
	reload := func() ([]any, [][]hostapi.Segment) { return nil, nil }
	run := []any{map[string]any{"text": "a", "run": "x"}}
	keys := []hostapi.OverlayKey{{Key: "e", Desc: "edit", Run: "edit"}}
	hints := func(o hostapi.Overlay, filter string) string {
		a := newTestApp(t, t.TempDir())
		o.Origin = lua.OriginBuiltin
		a.openOverlay(o)
		// "/" types, "=" is a kept query
		a.overlays[0].active, a.overlays[0].query = filter == "/", strings.TrimPrefix(filter, "=")
		box := a.overlayBox(&a.overlays[0], 80, 20)
		if len(box) > 20 {
			t.Errorf("%d rows do not fit 20", len(box))
		}
		return hostapi.SegsText(box[len(box)-2])
	}
	for _, c := range []struct {
		o      hostapi.Overlay
		filter string
		want   string
	}{
		{hostapi.Overlay{Items: []any{"inert"}}, "", "y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "fill": "x"}}, Verb: "run"}, "", "enter fill · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "detail": "x"}}, Verb: "pick"}, "", "enter open · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "run": "x"}}, Origin: lua.OriginBuiltin, Verb: "pick"}, "", "enter pick · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "run": "x", "verb": "open"}}, Verb: "pick", Reload: reload}, "", "enter open · y copy · esc close"},
		{hostapi.Overlay{Items: run, Verb: "copy"}, "", "enter copy · esc close"},
		{hostapi.Overlay{Items: run}, "", "enter run · y copy · esc close"},
		{hostapi.Overlay{Items: []any{"a"}, OnSelect: &glua.LFunction{}}, "", "enter pick · y copy · esc close"},
		{hostapi.Overlay{Items: run, Reload: reload}, "", "enter toggle · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "run": "x", "detail": "d"}}, Reload: reload}, "", "enter open · space toggle · y copy · esc close"},
		{hostapi.Overlay{Items: run, Keys: keys}, "", "enter run · y copy · e edit · esc close"},
		{hostapi.Overlay{Keys: keys}, "", "e edit · esc close"},
		{hostapi.Overlay{Keys: []hostapi.OverlayKey{{Key: "r", Desc: "resume"}}}, "", "esc close"}, // a row's key waits for a row
		{hostapi.Overlay{Items: run, Keys: keys}, "/", "enter done · esc clear"},
		{hostapi.Overlay{Items: run, Keys: keys}, "=a", "enter run · y copy · e edit · esc clear"},
		{hostapi.Overlay{Body: "text"}, "", "y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "copy": "export X"}}}, "", "enter copy · esc close"},
		{hostapi.Overlay{Body: "text", Items: []any{map[string]any{"text": "a", "fill": "x"}}}, "/", "enter done · esc clear"},
		{hostapi.Overlay{Body: strings.Repeat("line\n", 10), Items: slices.Repeat([]any{map[string]any{"text": "a", "fill": "x"}}, 30)}, "", "enter fill · y copy · esc close · ctrl+d/u scroll"},
	} {
		if got := strings.Trim(hints(c.o, c.filter), "│ "); got != c.want {
			t.Errorf("%v: %q, want %q", c.o.Items, got, c.want)
		}
	}
}

// TestListAlias: a list = true binding makes its key the move it names
// wherever a list, popup or prompt has the keys, and nowhere else.
func TestListAlias(t *testing.T) {
	many := make([]any, 40)
	for i := range many {
		many[i] = fmt.Sprintf("row %d", i)
	}
	for _, c := range []struct {
		name  string
		setup func(a *App)
		key   string
		check func(a *App) string
	}{
		{"ctrl+j moves an overlay's selection", func(a *App) {
			a.openOverlay(hostapi.Overlay{ID: "o", Origin: lua.OriginBuiltin, Items: []any{"a", "b"}})
		}, "ctrl+j", func(a *App) string { return fail(a.overlays[0].selected == 1, "not moved") }},
		{"ctrl+j moves the palette's selection", func(a *App) {
			runLua(t, a, `zemi.command.add("aa", function() end) zemi.command.add("ab", function() end)`)
			pressKeys(a, "/a")
		}, "ctrl+j", func(a *App) string {
			return fail(a.palette.open && a.palette.selected == 1, "not moved")
		}},
		{"ctrl+j on the bare composer scrolls the chat", func(a *App) { pressKeys(a, "hi") }, "ctrl+j", func(a *App) string {
			return fail(a.composer.text == "hi", fmt.Sprintf("composer %q", a.composer.text))
		}},
		{"an alias of ctrl+d moves half a page", func(a *App) {
			a.openOverlay(hostapi.Overlay{ID: "o", Origin: lua.OriginBuiltin, Items: many})
			a.overlayBox(&a.overlays[0], 80, 20)
		}, "ctrl+f", func(a *App) string {
			st := a.overlays[0]
			want := max(st.view.pageItems(len(st.visible))/2, 1)
			return fail(st.selected == want && want > 1, fmt.Sprintf("selected %d, want %d", st.selected, want))
		}},
		{"an alias of ctrl+u leaves the draft under a popup alone", func(a *App) {
			runLua(t, a, `zemi.command.add("aa", function() end)`)
			pressKeys(a, "/a")
		}, "ctrl+b", func(a *App) string {
			return fail(a.composer.text == "/a", fmt.Sprintf("composer %q", a.composer.text))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 80, 20)
			runLua(t, app, `zemi.keymap.set("ctrl+j", "down", { list = true })
zemi.keymap.set("ctrl+f", "ctrl+d", { list = true })
zemi.keymap.set("ctrl+b", "ctrl+u", { list = true })`)
			c.setup(app)
			pressKeys(app, c.key)
			if msg := c.check(app); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

func TestMoveSel(t *testing.T) {
	for _, c := range []struct {
		key                 string
		n, page, from, want int
	}{
		{"ctrl+d", 20, 10, 0, 5},
		{"ctrl+u", 20, 10, 12, 7},
		{"ctrl+d", 20, 10, 17, 19},
		{"ctrl+u", 20, 10, 3, 0},
		{"ctrl+d", 20, 1, 0, 1},
	} {
		if sel := c.from; !moveSel(&sel, c.n, c.page, c.key) || sel != c.want {
			t.Errorf("%s from %d over %d, page %d: %d, want %d", c.key, c.from, c.n, c.page, sel, c.want)
		}
	}
}

// TestHintOrder: pairs sort into their slots, and a narrow row drops the
// middle pairs, scrolling and copy first, never enter or esc.
func TestHintOrder(t *testing.T) {
	pairs := []hintPair{{"esc", "close"}, {"y", "copy"}, {"a", "keep"}, {"enter", "open"}, {"d", "drop"}, {"ctrl+d/u", "scroll"}}
	for _, c := range []struct {
		w    int
		want string
	}{
		{80, "enter open · y copy · a keep · d drop · esc close · ctrl+d/u scroll"},
		{55, "enter open · y copy · a keep · d drop · esc close"},
		{40, "enter open · a keep · d drop · esc close"},
		{5, "enter open · esc close"},
	} {
		if got := hostapi.SegsText(hint(c.w, pairs...)); got != c.want {
			t.Errorf("%d cells: %q, want %q", c.w, got, c.want)
		}
	}
}

// TestOverlayFilterKeepsSize: filtering, down to no match, never resizes
// the box or moves the description column, and the list grows with the screen.
func TestOverlayFilterKeepsSize(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 40)
	var items []any
	for i := range 15 {
		items = append(items, map[string]any{"text": fmt.Sprintf("item %02d", i), "desc": "about"})
	}
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: append(items, map[string]any{"text": "a much longer row than the others", "desc": "x"})})
	st := &app.overlays[0]
	box := app.overlayBox(st, 80, 40)
	if !strings.Contains(fmt.Sprint(box), "item 14") {
		t.Fatalf("a tall screen still scrolls: %v", box)
	}
	col := func(box [][]hostapi.Segment) int { return strings.Index(hostapi.SegsText(box[2]), "about") }
	for _, q := range []string{"item", "xxx"} {
		st.active, st.query = true, q
		st.apply()
		got := app.overlayBox(st, 80, 40)
		if hostapi.SegsWidth(got[0]) != hostapi.SegsWidth(box[0]) || len(got) != len(box) {
			t.Fatalf("filter %q: %dx%d, want %dx%d", q, hostapi.SegsWidth(got[0]), len(got), hostapi.SegsWidth(box[0]), len(box))
		}
		if q == "item" && col(got) != col(box) {
			t.Fatalf("filter %q moved the description column from %d to %d", q, col(box), col(got))
		}
	}
}

// TestOverlayRowsBuiltOnce: a frame that moved nothing builds no list rows,
// so an open picker costs the same at 10 items and 10000.
func TestOverlayRowsBuiltOnce(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	var items []any
	for i := range 50 {
		items = append(items, fmt.Sprintf("item %02d", i))
	}
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: items})
	st := &app.overlays[0]
	for _, c := range []struct {
		name  string
		step  func()
		built bool
	}{
		{"first frame", func() {}, true},
		{"same frame", func() {}, false},
		{"move", func() { st.selected = 30 }, true},
		{"filter", func() { st.query = "item 4"; st.apply() }, true},
		{"resize", func() {}, true},
	} {
		c.step()
		before := st.rows
		app.overlayBox(st, map[bool]int{true: 60, false: 80}[c.name == "resize"], 20)
		if built := before == nil || &before[0] != &st.rows[0]; built != c.built {
			t.Errorf("%s: rows built %v, want %v", c.name, built, c.built)
		}
	}
	if got := hostapi.SegsText(st.rows[0]); !strings.Contains(got, "item 04") {
		t.Errorf("filtered list starts with %q", got)
	}
}

// TestOverlayClosesOnOutsideClick: a click beside a menu closes it, a click
// inside does not, and a blocking ask ignores both.
func TestOverlayClosesOnOutsideClick(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"alpha", "beta"}})
	app.renderSync(t)
	app.Frame()
	box := app.chromeRect
	clickAt(app, box.Min.X+1, box.Min.Y+1)
	if len(app.overlays) != 1 {
		t.Fatal("a click inside the box closed it")
	}
	clickAt(app, 0, 0)
	if len(app.overlays) != 0 {
		t.Fatal("a click outside the box left it open")
	}

	go app.AskPermission(hostapi.PermRequest{Subject: "tool.shell", Arg: "make test"})
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	app.Frame()
	clickAt(app, 0, 0)
	if app.permAsk == nil {
		t.Fatal("a stray click answered a permission ask")
	}
	app.answerPerm(false, "user")
}

// TestListDescUnderLabel: a list too narrow for its description column
// puts each description under its label instead of dropping them all.
func TestListDescUnderLabel(t *testing.T) {
	items := []listItem{{label: "shift+enter, alt+enter, ctrl+j", desc: "newline"}, {label: "esc", desc: "back, or twice to interrupt"}}
	var got []string
	for _, row := range listRowsW(items, items, 0, nil, 10, 0, 30) {
		got = append(got, strings.TrimRight(hostapi.SegsText(row), " "))
	}
	if want := "› shift+enter, alt+enter…|  newline|  esc|  back, or twice to interrupt"; strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
}

// TestListDescColumn: descriptions start in one column, and a row that
// does not fit gives way from its description, never its label or key.
func TestListDescColumn(t *testing.T) {
	for _, c := range []struct {
		name  string
		items []listItem
	}{
		{"a long title with a short description", []listItem{
			{label: "a much longer session title here", desc: "now · short"},
			{label: "done", desc: "now · 2 messages · hello number 3 with a longer message to title it", key: tagSeg("current")},
		}},
		{"a picker keeps its ids whole", []listItem{
			{label: "deepseek/deepseek-v4-flash", desc: "DeepSeek-V4-Flash · 1M · $0.14/$0.28"},
			{label: "fake/m", desc: "128k", key: tagSeg("current")},
			{label: "fake/deepseek-v4-flash-long-model-name", desc: "1M"},
		}},
		{"a cut after a sentence ends in the bare ellipsis", []listItem{
			{label: "explore", desc: "read-only: finds how something works. Use it before editing"},
		}},
	} {
		col := -1
		for i, row := range listRowsW(c.items, c.items, 0, nil, 10, 0, 52) {
			text, it := hostapi.SegsText(row), c.items[i]
			if !strings.Contains(text, it.label) || !strings.HasSuffix(text, it.key.Text) || strings.Contains(text, "·…") || strings.Contains(text, ".…") {
				t.Errorf("%s: row %q", c.name, text)
			}
			x := 0
			for _, s := range row {
				if s.HL == "Host:Hint" {
					if col < 0 {
						col = x
					}
					if x != col {
						t.Errorf("%s: description at %d, want %d: %q", c.name, x, col, text)
					}
					break
				}
				x += ansi.StringWidth(s.Text)
			}
		}
	}
}

// TestOverlaySwitch: an item's on and locked fields draw the switch, the
// same width in every state, so a toggle never moves a row's text.
func TestOverlaySwitch(t *testing.T) {
	width := -1
	for _, c := range []struct {
		item   map[string]any
		sw, hl string
	}{
		{map[string]any{"text": "a", "on": true}, "◆ on ", "Host:Ok"},
		{map[string]any{"text": "a", "on": false, "key": "tag"}, "◇ off", "Host:Hint"},
		{map[string]any{"text": "a", "locked": true}, "  on ", "Host:Hint"},
	} {
		its := (&App{}).overlayState(hostapi.Overlay{Items: []any{c.item}}).items
		row := listRowsW(its, its, 0, nil, 10, 0, 0)[0]
		if k := row[len(row)-1]; k.Text != c.sw || k.HL != c.hl {
			t.Errorf("%v: switch %q in %s, want %q in %s", c.item, k.Text, k.HL, c.sw, c.hl)
		}
		if width < 0 {
			width = hostapi.SegsWidth(row)
		}
		if hostapi.SegsWidth(row) != width {
			t.Errorf("%v: row %d cells, want %d", c.item, hostapi.SegsWidth(row), width)
		}
	}
}

func TestOverlayFitsShortScreen(t *testing.T) {
	a, _ := bootAgentApp(t, 80, 16)
	items := make([]any, 30)
	for i := range items {
		items[i] = fmt.Sprintf("p/model-%d", i)
	}
	a.openOverlay(hostapi.Overlay{Title: "Model", Items: items})
	a.renderSync(t)
	if f := ansi.Strip(a.Frame()); !strings.Contains(f, "esc close") || !strings.Contains(f, "└") {
		t.Fatalf("the overlay's hint or bottom border is cut:\n%s", f)
	}
}

// TestOverlaySegmentLabel: a render_item row keeps its colors, takes the
// row's style where it sets none, cuts with an ellipsis, and loses a click
// id, which would otherwise act as Go chrome.
func TestOverlaySegmentLabel(t *testing.T) {
	app, _ := bootAgentApp(t, 40, 20)
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginPlugin, Items: []any{"a", "b"}, Selected: 1, ItemText: [][]hostapi.Segment{
		{{Text: "red", HL: "ToastErr", Click: "reload"}, {Text: " plain"}},
		{{Text: "a label far too long for a forty cell wide box", HL: "Title"}},
	}})
	rows := app.overlayBox(&app.overlays[0], 40, 20)
	first, second := rows[1], rows[2]
	if first[2].HL != "ToastErr" || first[2].Click != "" || first[3].HL != "Host:Value" {
		t.Fatalf("first row: %+v", first)
	}
	if !strings.Contains(hostapi.SegsText(second), "…") || second[2].HL != "Title" {
		t.Fatalf("selected row: %+v", second)
	}
}

// TestOverlayTailLabel: a tail row too wide for the box keeps its end, with
// its desc beside or under it, at every width.
func TestOverlayTailLabel(t *testing.T) {
	path := "~/some very/long/parent/directory/names/that/go/on/and/on/alpha"
	for _, w := range []int{40, 80, 120} {
		app, _ := bootAgentApp(t, w, 20)
		app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Items: []any{
			map[string]any{"text": path, "tail": true, "desc": "undecided · 1 session · 5m ago", "key": "current"},
		}})
		var text string
		for _, r := range app.overlayBox(&app.overlays[0], w, 20) {
			text += hostapi.SegsText(r) + "\n"
		}
		if !strings.Contains(text, "…") || !strings.Contains(text, "on/alpha") || !strings.Contains(text, "5m ago") {
			t.Fatalf("at %d cells:\n%s", w, text)
		}
	}
}

// TestOverlayBodyLines: a body of lines keeps its highlights and nothing
// else, y copies its text, and markdown reaches the detail views.
func TestOverlayBodyLines(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 16)
	runLua(t, app, `zemi.ui.overlay.open({ id = "b", markdown = true,
  body = { { { text = "red", hl = "Danger", click = "quit" }, " plain" }, { "two" } },
  items = { { text = "a", detail = "**bold**" } } })
assert(not pcall(zemi.ui.overlay.open, { id = "x", body = { 1 } }), "a body that is no list of lines must raise")`)
	st := &app.overlays[0]
	row := app.overlayBox(st, 60, 16)[1]
	if st.o.Body != "red plain\ntwo\n" || row[1].Text != "red" || row[1].HL != "Danger" || row[1].Click != "" {
		t.Fatalf("body %q, row %+v", st.o.Body, row)
	}
	pressKeys(app, "enter")
	if d := app.overlays[1]; !d.o.BodyMD || strings.Contains(hostapi.SegsText(app.overlayBox(&d, 60, 16)[1]), "*") {
		t.Fatalf("detail: %+v", d.o)
	}
}

// TestOverlayItemFields: every field of a Lua item reaches the parsed row,
// and one of the wrong type raises, a detail action's id included.
func TestOverlayItemFields(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 16)
	runLua(t, app, `zemi.ui.overlay.open({ id = "f", items = { { text = "t", desc = "d", key = "k", section = "s", detail = "x", verb = "v",
  hotkey = "h", fill = "f", copy = "c", run = "r", id = "i", danger = true, tail = true, on = false, locked = true, actions = { "a" }, fields = { { "a", "b" } } } } })
for _, bad in ipairs({ { text = 1 }, { on = "yes" }, { actions = "x" }, { actions = { { id = 1 } } }, { fields = { "x" } }, { danger = "yes" }, { tail = 1 } }) do
  assert(not pcall(zemi.ui.overlay.open, { id = "x", items = { bad } }), "a field of the wrong type must raise")
end
	assert(pcall(zemi.ui.overlay.open, { id = "x", items = { { id = 1 } } }), "a list item's id is its own data")`)
	// the fields the row renderer and the key handling read, from the Lua above
	it := app.overlays[0].items[0].row
	if it.Text != "t" || it.Desc != "d" || it.Key != "k" || it.Section != "s" || it.Detail != "x" ||
		it.Verb != "v" || it.Hotkey != "h" || it.Fill != "f" || it.Copy != "c" || it.Run != "r" || it.ID != "i" ||
		!it.Danger || !it.Tail || !it.Switch || !it.Locked || fmt.Sprint(it.Actions, it.Fields) != "[a] [{a b}]" {
		t.Fatalf("row %+v", it)
	}
}

// TestOverlayActionsAndKeys: an action with an id goes to on_action with
// its item and draws in its hl, and a list key, named in the footer, runs
// its fn with the selected item; both close the list like on_select.
func TestOverlayActionsAndKeys(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 16)
	open := `zemi.ui.overlay.open({ id = "k", items = { "a", { text = "b", actions = { { text = "Delete", id = "del", danger = true }, { text = "Back" } } } },
  on_action = function(item, action) zemi.notify({ body = item.text .. ":" .. action.id }) end,
  keys = { e = { desc = "edit", fn = function(item) zemi.notify({ body = "e:" .. item.text }) end } } })`
	runLua(t, app, open+`
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { esc = { fn = function() end } } }), "a key of several characters must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { e = { desc = "no fn" } } }), "a key with no fn must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { y = { fn = function() end } } }), "a key the list takes itself must raise")`)
	box := app.overlayBox(&app.overlays[0], 60, 16)
	if hint := hostapi.SegsText(box[len(box)-2]); !strings.Contains(hint, "y copy · e edit · esc close") {
		t.Fatalf("footer: %q", hint)
	}
	for _, c := range []struct{ keys, want string }{
		{"j enter 2", "k"},       // Back has no id: it returns to the list
		{"j enter 1 1", "b:del"}, // a danger action's first press arms it
		{"j e", "e:b"},
	} {
		if len(app.overlays) == 0 {
			runLua(t, app, open)
		}
		keys := strings.Fields(c.keys)
		pressKeys(app, keys[:len(keys)-1]...)
		if len(keys) > 2 {
			box := slices.Concat(app.overlayBox(&app.overlays[1], 60, 16)...)
			if i := slices.IndexFunc(box, func(s hostapi.Segment) bool { return s.Text == "Delete" }); i < 0 || box[i].HL != "Host:Danger" {
				t.Fatalf("action rows: %+v", box)
			}
		}
		pressKeys(app, keys[len(keys)-1])
		for app.Pump() {
		}
		if c.want == "k" {
			if len(app.overlays) != 1 {
				t.Fatalf("%s: %d overlays, want the list", c.keys, len(app.overlays))
			}
			continue
		}
		if got := lastToast(app); got != c.want || len(app.overlays) != 0 {
			t.Fatalf("%s: got %q with %d overlays open, want %q and none", c.keys, got, len(app.overlays), c.want)
		}
	}
	// a rebuild that empties the list under an open detail: the action still gets the detail's item
	runLua(t, app, `zemi.ui.overlay.open({ id = "r", items = { { text = "gone", actions = { { text = "Delete", id = "del" } } } },
  reload = function() return {} end, on_select = function() end,
  on_action = function(item, action) zemi.notify({ body = item.text .. ":" .. action.id }) end })`)
	pressKeys(app, "space", "enter")
	for app.Pump() {
	}
	pressKeys(app, "1")
	if got := lastToast(app); got != "gone:del" {
		t.Fatalf("action under a rebuilt list: got %q", got)
	}

	// a row's action keys act from the list as its detail's buttons do
	ran := ""
	for _, name := range []string{"res", "del"} {
		app.host.Commands.Add(&hostapi.Command{Name: name, NArgs: "?", GoFn: func(_ *hostapi.Host, args string) error { ran = name + " " + args; return nil }})
	}
	session := func(n string) string {
		return `{ text = "s` + n + `", actions = { { text = "Resume", hotkey = "r", run = "res ` + n + `" }, { text = "Delete", hotkey = "x", danger = true, run = "del ` + n + `" } } }`
	}
	footer := func() string {
		box := app.overlayBox(&app.overlays[len(app.overlays)-1], 60, 16)
		return hostapi.SegsText(box[len(box)-2])
	}
	armed := func() bool { return app.overlays[len(app.overlays)-1].armed }
	for _, c := range []struct {
		name, items, keys string
		press             []string
		check             func() string
	}{
		{"r resumes from the list", "", "", []string{"j", "r"}, func() string {
			return fail(ran == "res 1" && len(app.overlays) == 0, fmt.Sprintf("ran %q, %d open", ran, len(app.overlays)))
		}},
		{"x opens the detail on delete, armed", "", "", []string{"j", "x"}, func() string {
			d := app.overlays[len(app.overlays)-1]
			ok := ran == "" && d.back && d.selected == 1 && strings.Contains(footer(), "x again to delete · esc cancel")
			return fail(ok, fmt.Sprintf("ran %q, %d open, selected %d, footer %q", ran, len(app.overlays), d.selected, footer()))
		}},
		{"x twice deletes", "", "", []string{"j", "x", "x"}, func() string {
			return fail(ran == "del 1" && len(app.overlays) == 0, fmt.Sprintf("ran %q, %d open", ran, len(app.overlays)))
		}},
		{"x then space deletes", "", "", []string{"j", "x", "space"}, func() string {
			return fail(ran == "del 1", fmt.Sprintf("ran %q", ran))
		}},
		{"x then enter deletes", "", "", []string{"j", "enter", "x", "enter"}, func() string {
			return fail(ran == "del 1", fmt.Sprintf("ran %q", ran))
		}},
		{"esc drops the arm and keeps the detail", "", "", []string{"j", "x", "esc"}, func() string {
			ok := ran == "" && len(app.overlays) == 2 && !armed() && strings.Contains(footer(), "enter pick")
			return fail(ok, fmt.Sprintf("ran %q, %d open, armed %v, footer %q", ran, len(app.overlays), armed(), footer()))
		}},
		{"another key drops the arm, and the action's key arms it again", "", "", []string{"j", "x", "k", "x"}, func() string {
			sel := app.overlays[len(app.overlays)-1].selected
			return fail(ran == "" && armed() && sel == 1, fmt.Sprintf("ran %q, armed %v on %d", ran, armed(), sel))
		}},
		{"a danger row arms on enter", `"current", { text = "wipe", danger = true, run = "del w" }`, "", []string{"j", "enter"}, func() string {
			return fail(ran == "" && strings.Contains(footer(), "enter again to wipe"), fmt.Sprintf("ran %q, footer %q", ran, footer()))
		}},
		{"a danger row runs on the second enter", `"current", { text = "wipe", danger = true, run = "del w" }`, "", []string{"j", "enter", "enter"}, func() string {
			return fail(ran == "del w", fmt.Sprintf("ran %q", ran))
		}},
		{"a detail view with one danger action is its confirmation", `"current", { text = "p", detail = "rewinds", actions = { { text = "Rewind", hotkey = "r", danger = true, run = "del p" } } }`, "", []string{"j", "enter", "r"}, func() string {
			return fail(ran == "del p", fmt.Sprintf("ran %q", ran))
		}},
		{"a list's own key wins over an action's", "", `keys = { r = { desc = "rename", fn = function(item) zemi.notify({ body = "key " .. item.text }) end } }`, []string{"j", "r"}, func() string {
			return fail(ran == "" && lastToast(app) == "key s1", fmt.Sprintf("ran %q, toast %q", ran, lastToast(app)))
		}},
		{"a hotkey runs its own action, not the one at that position", `"current", { text = "s1", actions = { { text = "Delete", danger = true, run = "del 1" }, { text = "Other", hotkey = "1", run = "res 1" } } }`, "", []string{"j", "1"}, func() string {
			return fail(ran == "res 1", fmt.Sprintf("ran %q", ran))
		}},
		{"an action with a detail opens it over the list", `"current", { text = "s1", actions = { { text = "Read it", hotkey = "r", detail = "body" } } }`, "", []string{"j", "r"}, func() string {
			return fail(len(app.overlays) == 2 && app.overlays[1].o.Body == "body", fmt.Sprintf("%d open", len(app.overlays)))
		}},
		{"a row without actions ignores the key", "", "", []string{"r"}, func() string {
			return fail(ran == "" && len(app.overlays) == 1, fmt.Sprintf("ran %q, %d open", ran, len(app.overlays)))
		}},
		{"the footer names no action key", "", "", nil, func() string {
			return map[bool]string{true: footer()}[strings.Contains(footer(), "resume")]
		}},
	} {
		ran = ""
		app.overlays = nil
		runLua(t, app, `zemi.ui.overlay.open({ id = "s", items = { `+cmp.Or(c.items, `"current", `+session("1")+`, `+session("2"))+` }, `+c.keys+` })`)
		pressKeys(app, c.press...)
		for app.Pump() {
		}
		if msg := c.check(); msg != "" {
			t.Errorf("%s: %s", c.name, msg)
		}
	}
	// a Go screen's declared keys: the footer names them on every row, and
	// on a row without the action the key says why
	ran, app.overlays = "", nil
	app.openOverlay(hostapi.Overlay{ID: "s", Origin: lua.OriginBuiltin, Keys: []hostapi.OverlayKey{{Key: "r", Desc: "resume"}, {Key: "x", Desc: "delete"}}, Items: []any{
		map[string]any{"text": "current", "detail": "zemi runs it"},
		map[string]any{"text": "s1", "actions": []any{map[string]any{"text": "Resume", "hotkey": "r", "run": "res 1"}}},
	}})
	if f := footer(); !strings.Contains(f, "r resume · x delete") {
		t.Fatalf("declared keys: footer %q", f)
	}
	if pressKeys(app, "r"); ran != "" || lastToast(app) != "zemi runs it" {
		t.Fatalf("declared key on a row without it: ran %q, toast %q", ran, lastToast(app))
	}
	if pressKeys(app, "j", "r"); ran != "res 1" {
		t.Fatalf("declared key on a row with it: ran %q", ran)
	}
	// a destructive command typed without --yes opens its row's detail armed,
	// as wide as the list would have drawn it; a run no row has says so
	app.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	app.renderSync(t)
	area := boxArea(app.stage, app.width, app.height)
	s2 := []any{"current", map[string]any{"text": "s2", "desc": strings.Repeat("a long description ", 4), "actions": []any{
		map[string]any{"text": "Resume", "hotkey": "r", "run": "res 2"}, map[string]any{"text": "Delete", "hotkey": "x", "danger": true, "run": "del 2"},
	}}}
	typed := func(confirm string, keys ...string) *overlayState {
		ran, app.overlays = "", nil
		app.openOverlay(hostapi.Overlay{ID: "s", Title: "Sessions", Origin: lua.OriginBuiltin, Confirm: confirm, Items: s2})
		app.overlayBox(&app.overlays[0], area.Dx(), area.Dy())
		pressKeys(app, keys...)
		return &app.overlays[len(app.overlays)-1]
	}
	boxW := func(st *overlayState) int { return hostapi.SegsWidth(app.overlayBox(st, area.Dx(), area.Dy())[0]) }
	listed := boxW(typed("", "j", "x"))
	if d := typed("del 2"); !d.back || d.selected != 1 || !d.armed || boxW(d) != listed {
		t.Fatalf("typed delete: %d open, selected %d, armed %v, width %d, want %d", len(app.overlays), d.selected, d.armed, boxW(d), listed)
	}
	pressKeys(app, "enter")
	if ran != "del 2" {
		t.Fatalf("typed delete confirmed: ran %q", ran)
	}
	// a click away and back drops the arm, as another key does
	typed("del 2")
	app.renderSync(t)
	for _, label := range []string{"Resume", "Delete"} {
		x, y := findCell(t, app, label)
		clickAt(app, x, y)
	}
	if d := app.overlays[len(app.overlays)-1]; d.selected != 1 || d.armed || ran != "" {
		t.Fatalf("clicked back: selected %d, armed %v, ran %q", d.selected, d.armed, ran)
	}
	// an action with no danger a typed command names opens armed too, drawn as a danger one
	if d := typed("res 2"); d.selected != 0 || !d.armed {
		t.Fatalf("typed resume: selected %d, armed %v", d.selected, d.armed)
	} else if box := slices.Concat(app.overlayBox(d, area.Dx(), area.Dy())...); !slices.ContainsFunc(box, func(s hostapi.Segment) bool { return s.Text == "Resume" && s.HL == hostapi.ChromeDanger }) {
		t.Fatalf("typed resume drawn: %+v", box)
	}
	typed("del 9")
	if toast := lastToast(app); !strings.Contains(toast, "/del 9 runs it without asking") {
		t.Fatalf("no row for the typed run: toast %q", toast)
	}
}

// TestLuaOverlayReload: a Lua overlay with reload stays open on space and
// enter, runs on_select, then shows the items reload returns, rendered
// again, and keeps its items when reload fails.
func TestLuaOverlayReload(t *testing.T) {
	app, host := bootAgentApp(t, 60, 16)
	runLua(t, app, `
local on = { a = false, b = false }
local function items()
  return { { text = "a", on = on.a }, { text = "b", on = on.b } }
end
zemi.ui.overlay.open({ id = "toggles", title = "Toggles", items = items(),
  render_item = function(it) return (it.on and "[x] " or "[ ] ") .. it.text end,
  on_select = function(it) on[it.text] = not on[it.text] end,
  reload = function()
    if on.a and on.b then return nil end
    return items()
  end })`)
	pressKeys(app, "j", "space")
	for app.Pump() {
	}
	if len(app.overlays) != 1 || app.overlays[0].items[1].label != "[x] b" || app.overlays[0].selected != 1 {
		t.Fatalf("after space: %d overlays, %+v", len(app.overlays), app.overlays[0].items)
	}
	pressKeys(app, "k", "enter")
	for app.Pump() {
	}
	if len(app.overlays) != 1 || app.overlays[0].items[0].label != "[ ] a" || !host.HasUnseenErrors() {
		t.Fatalf("a failed reload should keep the items and record the error: %+v", app.overlays[0].items)
	}
	// a fill row runs on_select too, then closes the list for the composer
	runLua(t, app, `zemi.ui.overlay.open({ id = "fill", items = { { text = "a", fill = "/x " } },
  on_select = function(it) zemi.notify({ body = "picked " .. it.text }) end, reload = function() return {} end })`)
	pressKeys(app, "enter")
	for app.Pump() {
	}
	if len(app.overlays) != 1 || app.composer.text != "/x " || lastToast(app) != "picked a" {
		t.Fatalf("fill row: %d overlays, composer %q, toast %q", len(app.overlays), app.composer.text, lastToast(app))
	}
}

// TestOverlayCloseRules: zemi.ui.overlay.close honors only user, builtin or
// the origin that opened the overlay, and it runs on_close, so a picker's
// callback always answers.
func TestOverlayCloseRules(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	run := func(src string, origin lua.Origin) {
		t.Helper()
		if err := host.RT.ExecuteChunk(t.Context(), src, "t.lua", origin); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
	}
	run(`zemi.ui.overlay.open({ id = "pick", title = "Pick", items = { "a" },
		on_close = function() zemi.ui.overlay.open({ id = "answered", items = { "a" } }) end })`, lua.OriginUser)
	run(`zemi.ui.overlay.close("pick")`, lua.OriginProject)
	if len(app.overlays) != 1 {
		t.Fatal("project code closed a user overlay")
	}
	run(`zemi.ui.overlay.close("pick")`, lua.OriginUser)
	for app.Pump() {
	}
	if len(app.overlays) != 1 || app.overlays[0].o.ID != "answered" {
		t.Fatalf("on_close never ran: %+v", app.overlays)
	}
}

func TestFieldRows(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields []hostapi.Field
		mark   string
		w      int
		want   []string
	}{
		{
			"an error value keeps its lines, in Danger",
			[]hostapi.Field{{Name: "url", Value: "https://x.dev/mcp"}, {Name: "error", Value: "one two three four five six seven eight nine ten\nstack traceback:"}},
			"", 40,
			[]string{"url    https://x.dev/mcp", "error  one two three four five six  Host:Danger", "       seven eight nine ten Host:Danger", "       stack traceback: Host:Danger"},
		},
		{
			"a status word takes its color, an empty value is left out",
			[]hostapi.Field{{Name: "status", Value: "ready"}, {Name: "latest", Value: ""}},
			"", 40,
			[]string{"status  ready Host:Ok"},
		},
		{
			"the view's mark leads the status word, which keeps its own color",
			[]hostapi.Field{{Name: "status", Value: "running"}, {Name: "agent", Value: "explore"}},
			"spin", 40,
			[]string{"status  ⠋ running Dim", "agent   explore"},
		},
		{
			"too narrow for a column, a field is a line",
			[]hostapi.Field{{Name: "status", Value: "failed"}},
			"err", 24,
			[]string{"status: ✗ failed Danger Host:Danger"},
		},
	} {
		var got []string
		for _, r := range fieldRows(c.fields, c.mark, c.w) {
			row := hostapi.SegsText(r)
			for _, s := range r[1:] {
				if s.HL != "" {
					row += " " + s.HL
				}
			}
			got = append(got, row)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: rows %q, want %q", c.name, got, c.want)
		}
	}
}

// TestListPreview: the selected row's preview shows under the list in a
// pane as tall as the longest preview, so moving or filtering never changes
// the box's height, and the filter matches preview words.
func TestListPreview(t *testing.T) {
	short, long := "one short line", strings.Repeat("a preview that wraps ", 6)+"xylophone"
	for _, c := range []struct {
		keys []string
		want string // the preview's first words, "" for a blank pane
	}{
		{nil, short},
		{[]string{"j"}, "a preview that wraps"},
		{[]string{"j", "j"}, ""},
		{[]string{"/", "xylo"}, "a preview that wraps"},
	} {
		app, _ := bootAgentApp(t, 60, 30)
		runLua(t, app, `zemi.ui.overlay.open({ id = "p", items = { { text = "a", preview = "`+short+`" }, { text = "b", preview = "`+long+`" }, { text = "c" }, { text = "d" }, { text = "e" } } })`)
		rows := len(app.overlayBox(&app.overlays[0], 60, 30))
		pressKeys(app, c.keys...)
		box := app.overlayBox(&app.overlays[0], 60, 30)
		// the pane's first row stays put above the footer, a filter or not
		pane, top := hostapi.SegsText(slices.Concat(box[len(box)-6:len(box)-3]...)), strings.TrimLeft(hostapi.SegsText(box[len(box)-6]), "│ ")
		name := strings.Join(c.keys, " ")
		if len(box) != rows {
			t.Errorf("%q: %d rows, want %d", name, len(box), rows)
		}
		if got := strings.Contains(pane, "a preview") || strings.Contains(pane, short); c.want == "" && got || c.want != "" && !strings.HasPrefix(top, c.want) {
			t.Errorf("%q: pane %q, want %q", name, pane, c.want)
		}
	}
}

func TestPhaseMark(t *testing.T) {
	for _, c := range []struct {
		name  string
		marks []string
		want  string
	}{
		{"a failure outranks a running row", []string{"ok", "spin", "err"}, "err"},
		{"a running row outranks a queued one", []string{"wait", "spin", "ok"}, "spin"},
		{"a paused phase waits", []string{"ok", "wait"}, "wait"},
		{"a stopped row marks the phase stopped", []string{"ok", "stop"}, "stop"},
		{"done when every row is", []string{"ok", "ok"}, "ok"},
	} {
		if got := phaseMark(c.marks); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
