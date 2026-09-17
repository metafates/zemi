package ui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/win"
)

// TestToggleOverlayStaysOpen: in a Reload overlay (skills) space and enter
// run the row and rebuild the list in place, keeping the selection and the
// filter; ctrl+j moves like j.
func TestToggleOverlayStaysOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	on := false
	items := func() []any {
		mark := "○ b"
		if on {
			mark = "● b"
		}
		return []any{map[string]any{"text": "a"}, map[string]any{"text": mark, "fill": "picked"}}
	}
	a.openOverlay(hostapi.Overlay{ID: "t", Origin: lua.OriginBuiltin, Items: items(), Reload: func() []any { on = !on; return items() }})
	a.overlayKey("ctrl+j", "")
	for _, key := range []string{"space", "enter"} {
		wasOn := on
		a.overlayKey(key, map[string]string{"space": " "}[key])
		if len(a.overlays) != 1 || a.overlays[0].selected != 1 || on == wasOn {
			t.Fatalf("%s: overlays=%d selected=%d on=%v", key, len(a.overlays), a.overlays[0].selected, on)
		}
	}
	if got := a.overlays[0].texts[1]; got != "○ b" || a.composer.text != "picked" {
		t.Fatalf("row=%q composer=%q", got, a.composer.text)
	}
	a.overlayKey("", "/")
	a.overlayKey("", "b")
	a.overlayKey("space", " ")
	if st := a.overlays[0]; !st.active || st.query != "b" || len(st.visible) != 1 {
		t.Fatalf("filter after toggle: %+v", st.listFilter)
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

// TestOverlayFilter: "/" starts a filter that narrows the rows (all words,
// any case), backspace widens again, esc ends the filter before closing and
// keeps the matched row, j moves outside a filter, and a new query selects
// its first match, which enter runs.
func TestOverlayFilter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	var items []any
	for _, s := range []string{"alpha one", "Beta two", "gamma three"} {
		items = append(items, map[string]any{"text": s, "fill": s})
	}
	a.openOverlay(hostapi.Overlay{ID: "f", Origin: lua.OriginBuiltin, Items: items})
	typeText := func(s string) {
		for _, r := range s {
			a.overlayKey(string(r), string(r))
		}
	}
	visible := func() int { return len(a.overlays[0].visible) }

	typeText("/t")
	if visible() != 2 {
		t.Fatalf("t: %d rows", visible())
	}
	typeText("wo b")
	if visible() != 1 {
		t.Fatalf("'two b': %d rows", visible())
	}
	a.overlayKey("backspace", "")
	a.overlayKey("backspace", "")
	if q := a.overlays[0].query; q != "two" || visible() != 1 {
		t.Fatalf("backspace: query %q, %d rows", q, visible())
	}
	a.overlayKey("esc", "")
	if len(a.overlays) != 1 || visible() != 3 {
		t.Fatalf("esc clears the filter first: overlays=%d rows=%d", len(a.overlays), visible())
	}
	a.overlayKey("j", "j")
	if a.overlays[0].selected != 2 || a.overlays[0].query != "" {
		t.Fatalf("j outside a filter: selected=%d query=%q", a.overlays[0].selected, a.overlays[0].query)
	}
	typeText("/e")
	a.overlayKey("enter", "")
	if len(a.overlays) != 0 || a.composer.text != "alpha one" {
		t.Fatalf("enter: overlays=%d composer=%q", len(a.overlays), a.composer.text)
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

	a.openModelPick(make([]hostapi.ModelOption, 3), "", make(chan modelPick, 1))
	a.memReview = &memReviewPrompt{items: make([]hostapi.MemoryRow, 3)}
	a.wheelChrome(1)
	if a.memReview.sel != 1 || a.modelPick.selected != 0 {
		t.Fatalf("wheel: memory sel=%d picker sel=%d", a.memReview.sel, a.modelPick.selected)
	}
}

// TestModelPickerFilter: typing narrows the picker by id or display name
// and enter picks the highlighted match.
func TestModelPickerFilter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	answer := make(chan modelPick, 1)
	a.openModelPick([]hostapi.ModelOption{
		{ID: "anthropic/claude-sonnet", Display: "Sonnet"},
		{ID: "openai/gpt-5", Display: "GPT-5"},
		{ID: "deepseek/deepseek-chat", Display: "DeepSeek V3"},
	}, "", answer)
	for _, r := range "/v3" {
		a.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if n := len(a.modelPick.visible); n != 1 {
		t.Fatalf("v3 matches %d models", n)
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-answer; !got.ok || got.id != "deepseek/deepseek-chat" {
		t.Fatalf("picked %+v", got)
	}
}

// pressWhileLuaBusy attaches a program that never runs, so Lua work goes to
// the worker, and presses keys while the interpreter is held; it fails when
// a key waits on Lua. release frees the interpreter.
func pressWhileLuaBusy(t *testing.T, app *App, keys ...tea.KeyPressMsg) (release func()) {
	t.Helper()
	app.program = tea.NewProgram(nil)
	busy, hold := make(chan struct{}), make(chan struct{})
	go app.host.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(*glua.LState) ([]any, error) {
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
	runLua(t, app, `zemi.command.add("greet", function() end, { complete = function() return { "alice", "bob" } end })`)
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
	if len(app.palette.args) != 1 || app.palette.args[0] != "bob" {
		t.Fatalf("palette args %v", app.palette.args)
	}
}

// TestModalDimsBackground: a modal dims what lies under it; a toast and
// the palette are not modal and do not.
func TestModalDimsBackground(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	host.AppendMessage("user", "hello there")
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"a"}})
	app.pushToast("info", "Copied to the clipboard", "")
	hl := func(text string) string {
		app.renderSync(t)
		app.Frame()
		for _, row := range app.lastRows {
			for _, s := range row {
				if strings.Contains(s.Text, text) {
					return s.HL
				}
			}
		}
		t.Fatalf("%q not on screen", text)
		return ""
	}
	if got := hl("hello"); got != "Host:Dim" {
		t.Fatalf("under an overlay: %q", got)
	}
	if got := hl("Copied"); got == "Host:Dim" {
		t.Fatal("the overlay dimmed a toast")
	}
	app.overlays = nil
	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if got := hl("hello"); got == "Host:Dim" {
		t.Fatal("the palette dimmed the transcript")
	}
}

// TestPermissionOverFloat: a float window never covers the permission
// prompt.
func TestPermissionOverFloat(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	defer host.Wins.CloseAll()
	if _, err := host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindFloat}); err != nil {
		t.Fatal(err)
	}
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"}, answer: make(chan permAnswer, 1)}
	findCell(t, app, "make deploy")
	x, y := findCell(t, app, "No ")
	if _, _, ok := app.target(x, y); !ok {
		t.Fatal("the No choice under a float is not a target")
	}
}

func TestOverlayNavigation(t *testing.T) {
	detail := func(actions ...any) map[string]any {
		return map[string]any{"text": "err", "desc": "short", "detail": "line one\nline two", "actions": actions}
	}
	reloads := 0
	for _, c := range []struct {
		name  string
		o     hostapi.Overlay
		keys  []string
		check func(a *App) string // "" when the state is right
	}{
		{"enter on an inert row keeps the box open",
			hostapi.Overlay{Items: []any{"a", "b"}}, []string{"enter"},
			func(a *App) string { return map[bool]string{false: "closed"}[len(a.overlays) == 1] }},
		{"an empty overlay shows its empty line and enter does nothing",
			hostapi.Overlay{Empty: "Nothing to see."}, []string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || !strings.Contains(fmt.Sprint(overlayBox(&a.overlays[0], 80, 20)), "Nothing to see.") {
					return "no empty line"
				}
				return ""
			}},
		{"enter opens the detail and esc goes back with the selection kept",
			hostapi.Overlay{Title: "Errors", Items: []any{"a", detail()}}, []string{"down", "enter", "esc"},
			func(a *App) string {
				if len(a.overlays) != 1 || a.overlays[0].selected != 1 {
					return fmt.Sprintf("overlays=%d selected=%d", len(a.overlays), a.overlays[0].selected)
				}
				return ""
			}},
		{"the detail view has a breadcrumb title and scrolls its body",
			hostapi.Overlay{Title: "Errors", Items: []any{detail()}}, []string{"enter", "down"},
			func(a *App) string {
				d := a.overlays[len(a.overlays)-1]
				if len(a.overlays) != 2 || d.o.Title != "Errors › err" || d.scroll != 1 || !d.back {
					return fmt.Sprintf("%+v", d)
				}
				return ""
			}},
		{"an action that fills closes the stack",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Use", "fill": "/use "})}}, []string{"enter", "enter"},
			func(a *App) string {
				if len(a.overlays) != 0 || a.composer.text != "/use " {
					return fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text)
				}
				return ""
			}},
		{"an action with nothing to do goes back",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Cancel"})}}, []string{"enter", "enter"},
			func(a *App) string {
				return map[bool]string{false: "not back"}[len(a.overlays) == 1 && !a.overlays[0].back]
			}},
		{"a plugin's run action goes back, as its hint says",
			hostapi.Overlay{Origin: lua.OriginPlugin, Items: []any{detail(map[string]any{"text": "Restart", "run": "help"})}}, []string{"enter", "enter"},
			func(a *App) string {
				return map[bool]string{false: "not back"}[len(a.overlays) == 1 && !a.overlays[0].back]
			}},
		{"a run action in a toggle list rebuilds the list",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Toggle", "run": "nope"})}, Reload: func() []any { reloads++; return []any{detail()} }},
			[]string{"enter", "enter"},
			func(a *App) string {
				return map[bool]string{false: "not rebuilt"}[len(a.overlays) == 1 && reloads == 1]
			}},
		{"closing the parent drops its detail view",
			hostapi.Overlay{ID: "p", Items: []any{detail()}}, []string{"enter"},
			func(a *App) string {
				a.closeOverlayByID("p")
				return map[bool]string{false: "detail left"}[len(a.overlays) == 0]
			}},
		{"ctrl+c on a detail view closes the whole menu",
			hostapi.Overlay{ID: "p", Items: []any{detail()}}, []string{"enter"},
			func(a *App) string {
				a.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
				return map[bool]string{false: "still open"}[len(a.overlays) == 0 && a.quitArmed.IsZero()]
			}},
		{"a read-only body scrolls by line",
			hostapi.Overlay{Body: strings.Repeat("line\n", 50)}, []string{"down", "pgdown"},
			func(a *App) string { return map[bool]string{false: "no scroll"}[a.overlays[0].scroll == 11] }},
		{"a multi-line label stays on one row",
			hostapi.Overlay{Items: []any{"fix: empty input\n\nThe parser crashed."}}, nil,
			func(a *App) string {
				return map[bool]string{false: "label kept its newlines"}[a.overlays[0].items[0].label == "fix: empty input The parser crashed."]
			}},
		{"Selected picks the first item",
			hostapi.Overlay{Items: []any{"a", "b", "c"}, Selected: 2}, nil,
			func(a *App) string { return map[bool]string{false: "wrong selection"}[a.overlays[0].selected == 2] }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			c.o.Origin = cmp.Or(c.o.Origin, lua.OriginBuiltin)
			a.openOverlay(c.o)
			for _, k := range c.keys {
				a.overlayKey(k, "")
			}
			if msg := c.check(a); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// TestOverlayHints: the footer names only keys that act on the selection.
func TestOverlayHints(t *testing.T) {
	hints := func(o hostapi.Overlay, filtering bool) string {
		a := newTestApp(t, t.TempDir())
		o.Origin = lua.OriginBuiltin
		a.openOverlay(o)
		a.overlays[0].active = filtering
		box := overlayBox(&a.overlays[0], 80, 20)
		if len(box) > 20 {
			t.Errorf("%d rows do not fit 20", len(box))
		}
		return plainSegs(box[len(box)-2])
	}
	for _, c := range []struct {
		o         hostapi.Overlay
		filtering bool
		want      string
	}{
		{hostapi.Overlay{Items: []any{"inert"}}, false, "esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "fill": "x"}}}, false, "enter select · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "detail": "x"}}}, false, "enter open · esc close"},
		{hostapi.Overlay{Body: "text"}, false, "y copy · esc close"},
		{hostapi.Overlay{Body: strings.Repeat("line\n", 30), Items: []any{map[string]any{"text": "a", "fill": "x"}}}, false, "enter select · pgup/pgdn scroll · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "fill": "x"}}}, true, "enter select · esc clear"},
		{hostapi.Overlay{Body: strings.Repeat("line\n", 10), Items: slices.Repeat([]any{map[string]any{"text": "a", "fill": "x"}}, 30)}, false, "enter select · / filter · pgup/pgdn scroll · y copy · esc close"},
	} {
		if got := strings.Trim(hints(c.o, c.filtering), "│ "); got != c.want {
			t.Errorf("%v: %q, want %q", c.o.Items, got, c.want)
		}
	}
}

// TestOverlayFilterKeepsWidth: filtering, down to no match, never narrows
// the box, and the list grows with the screen.
func TestOverlayFilterKeepsWidth(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 40)
	var items []any
	for i := range 15 {
		items = append(items, fmt.Sprintf("item %02d", i))
	}
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: append(items, "a much longer row than the others")})
	st := &app.overlays[0]
	box := overlayBox(st, 80, 40)
	if !strings.Contains(fmt.Sprint(box), "item 14") {
		t.Fatalf("a tall screen still scrolls: %v", box)
	}
	for _, q := range []string{"item", "xxx"} {
		st.active, st.query = true, q
		st.apply(st.texts)
		if got := overlayBox(st, 80, 40); segsWidth(got[0]) != segsWidth(box[0]) {
			t.Fatalf("filter %q: width %d, want %d", q, segsWidth(got[0]), segsWidth(box[0]))
		}
	}
}

// TestOverlayLongLabelKeepsDescAndKey: a label too long for the box is cut
// before its description, and the key column stays.
func TestOverlayLongLabelKeepsDescAndKey(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{
		map[string]any{"text": "Done. The fixture path moved, so the test binary exits 1", "desc": "now · 2 messages", "key": "current"},
		map[string]any{"text": "please run bash", "desc": "1m ago · 18 messages · and a first line that runs far past the edge"},
	}})
	rows := overlayBox(&app.overlays[0], 80, 20)
	box := fmt.Sprint(rows)
	if !strings.Contains(box, "now") || !strings.Contains(box, "current") || !strings.Contains(box, "…") {
		t.Fatalf("box: %s", box)
	}
	// descriptions start in one column, the cut label's included
	col := func(row []hostapi.Segment, s string) int {
		before, _, _ := strings.Cut(plainSegs(row), s)
		return ansi.StringWidth(before)
	}
	if a, b := col(rows[1], "now"), col(rows[2], "1m ago"); a != b {
		t.Fatalf("descriptions at %d and %d:\n%s\n%s", a, b, plainSegs(rows[1]), plainSegs(rows[2]))
	}
}
