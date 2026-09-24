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
)

// TestToggleOverlayStaysOpen: in a Reload overlay (skills) space and enter
// run the row and rebuild the list in place, keeping the selection, the
// filter and the box's size; ctrl+n moves like j.
func TestToggleOverlayStaysOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	on := false
	items := func() []any {
		mark := "○ b"
		if on {
			mark = "● b"
		}
		return []any{map[string]any{"text": "a"}, map[string]any{"text": mark}}
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
	if got := a.overlays[0].texts[1]; got != "○ b" {
		t.Fatalf("row=%q", got)
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
// its first match, which enter runs. A row's hotkey runs it at once, except
// in a filter, which takes the key.
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
	items[2].(map[string]any)["hotkey"] = "3"
	a.openOverlay(hostapi.Overlay{ID: "f", Origin: lua.OriginBuiltin, Items: items})
	typeText("/3")
	if a.overlays[0].query != "3" {
		t.Fatalf("a filter takes the hotkey: query %q", a.overlays[0].query)
	}
	a.overlayKey("esc", "")
	a.overlayKey("3", "3")
	if len(a.overlays) != 0 || a.composer.text != "gamma three" {
		t.Fatalf("hotkey: overlays=%d composer=%q", len(a.overlays), a.composer.text)
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
	a.memReview = &memReviewPrompt{items: make([]hostapi.MemoryRow, 3)}
	a.memReview.seek(0)
	a.wheel(0, 0, 1)
	if a.memReview.at() != 1 || a.overlays[0].selected != 0 {
		t.Fatalf("wheel: memory sel=%d overlay sel=%d", a.memReview.at(), a.overlays[0].selected)
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
	if len(app.palette.args) != 1 || app.palette.args[0].Text != "bob" {
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
	if got := hl("untrusted"); got == "Host:Dim" {
		t.Fatal("the palette dimmed the screen")
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
		{"enter on an inert row keeps the box open",
			hostapi.Overlay{Items: []any{"a", "b"}}, []string{"enter"},
			func(a *App) string { return map[bool]string{false: "closed"}[len(a.overlays) == 1] }},
		{"enter on a copy row toasts and keeps the list open",
			hostapi.Overlay{Items: []any{map[string]any{"text": "deepseek", "copy": "export X"}}}, []string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || len(a.toasts) != 1 || a.toasts[0].title != "Copied" {
					return fmt.Sprintf("overlays=%d toasts=%+v", len(a.overlays), a.toasts)
				}
				return ""
			}},
		{"an empty overlay shows its empty line and enter does nothing",
			hostapi.Overlay{Empty: "Nothing to see."}, []string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || !strings.Contains(fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20)), "Nothing to see.") {
					return "no empty line"
				}
				return ""
			}},
		{"an empty list under a body keeps its empty line",
			hostapi.Overlay{Body: "header", Empty: "Nothing cached yet."}, nil,
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20))
				return map[bool]string{false: box}[strings.Contains(box, "header") && strings.Contains(box, "Nothing cached yet.")]
			}},
		{"a reload list whose rows only open says nothing of space",
			hostapi.Overlay{Items: []any{detail()}, Reload: func() ([]any, [][]hostapi.Segment) { return nil, nil }}, nil,
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20))
				return map[bool]string{true: box}[strings.Contains(box, "toggle")]
			}},
		{"enter opens the detail and esc goes back with the selection kept",
			hostapi.Overlay{Title: "Errors", Items: []any{"a", detail()}}, []string{"down", "enter", "esc"},
			func(a *App) string {
				if len(a.overlays) != 1 || a.overlays[0].selected != 1 {
					return fmt.Sprintf("overlays=%d selected=%d", len(a.overlays), a.overlays[0].selected)
				}
				return ""
			}},
		{"going back from a toggle list's detail, by esc or an inert action, does not rebuild it",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Nothing"})}, Reload: func() ([]any, [][]hostapi.Segment) { backs++; return nil, nil }},
			[]string{"enter", "esc", "enter", "enter"},
			func(a *App) string {
				return map[bool]string{false: fmt.Sprintf("overlays=%d rebuilds=%d", len(a.overlays), backs)}[len(a.overlays) == 1 && backs == 0]
			}},
		{"the detail view has a breadcrumb title",
			hostapi.Overlay{Title: "Errors", Items: []any{detail()}}, []string{"enter"},
			func(a *App) string {
				d := a.overlays[len(a.overlays)-1]
				if len(a.overlays) != 2 || d.o.Title != "Errors / err" || !d.back {
					return fmt.Sprintf("%+v", d)
				}
				return ""
			}},
		{"a detail is text unless its list holds fields",
			hostapi.Overlay{Items: []any{map[string]any{"text": "trust", "detail": "trusted: ~/x"}}}, []string{"enter"},
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20))
				return map[bool]string{false: box}[strings.Contains(box, "trusted: ~/x")]
			}},
		{"a fields list's detail sets them as a table",
			hostapi.Overlay{Fields: true, Items: []any{map[string]any{"text": "mcp", "detail": "status: ready"}}}, []string{"enter"},
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20))
				return map[bool]string{false: box}[strings.Contains(box, "{ready Host:Ok")]
			}},
		{"a breadcrumb longer than the list is cut, never widening the detail view",
			hostapi.Overlay{Title: "A long parent title", Items: []any{map[string]any{"text": strings.Repeat("x", 50), "detail": "d"}}}, []string{"enter"},
			func(a *App) string {
				list, detail := segsWidth(a.overlayBox(&a.overlays[0], 120, 20)[0]), a.overlayBox(&a.overlays[1], 120, 20)[0]
				return map[bool]string{false: fmt.Sprintf("list %d wide, detail %q", list, plainSegs(detail))}[segsWidth(detail) == list]
			}},
		{"the wheel scrolls a detail body with actions, and the box keeps its width",
			hostapi.Overlay{Title: "Errors", Items: []any{detail(map[string]any{"text": "Ask", "fill": "x"}, map[string]any{"text": "Back"})}}, []string{"enter"},
			func(a *App) string {
				d := &a.overlays[1]
				d.o.Body = strings.Repeat("line\n", 60)
				before := segsWidth(a.overlayBox(d, 80, 20)[0])
				a.wheel(0, 0, 1)
				d.selected = 1
				if after := segsWidth(a.overlayBox(d, 80, 20)[0]); d.body.top != 1 || after != before {
					return fmt.Sprintf("scroll=%d width %d -> %d", d.body.top, before, after)
				}
				return ""
			}},
		{"a detail view draws no filter, so typing hides no action",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Ask", "fill": "x"})}}, []string{"enter"},
			func(a *App) string {
				for _, k := range []string{"/", "x"} {
					a.overlayKey(k, k)
				}
				d := a.overlays[len(a.overlays)-1]
				return map[bool]string{false: fmt.Sprintf("visible=%v filtering=%v", d.visible, d.active)}[len(d.visible) == 1 && !d.active]
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
		{"a row that opens a screen leaves its list under it, for esc to come back to",
			hostapi.Overlay{ID: "m", Items: []any{map[string]any{"text": "Roles", "verb": "open", "run": "nope"}}}, []string{"enter"},
			func(a *App) string { return map[bool]string{false: "closed"}[len(a.overlays) == 1] }},
		{"a run action in a toggle list rebuilds the list",
			hostapi.Overlay{Items: []any{detail(map[string]any{"text": "Toggle", "run": "nope"})}, Reload: func() ([]any, [][]hostapi.Segment) { reloads++; return []any{detail()}, nil }},
			[]string{"enter", "enter"},
			func(a *App) string {
				return map[bool]string{false: "not rebuilt"}[len(a.overlays) == 1 && reloads == 1]
			}},
		{"space passes by a toggle list row with its own verb",
			hostapi.Overlay{Items: []any{map[string]any{"text": "Roles", "verb": "open", "run": "nope"}}, Reload: func() ([]any, [][]hostapi.Segment) { spaced++; return nil, nil }},
			[]string{"space"},
			func(a *App) string {
				return map[bool]string{false: "space ran the row"}[len(a.overlays) == 1 && spaced == 0]
			}},
		{"a fill row closes a toggle list, so the composer has the keys",
			hostapi.Overlay{Items: []any{map[string]any{"text": "Install", "verb": "install", "fill": "/plugins install "}}, Reload: func() ([]any, [][]hostapi.Segment) { return nil, nil }},
			[]string{"enter"},
			func(a *App) string {
				return map[bool]string{false: fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text)}[len(a.overlays) == 0 && a.composer.text == "/plugins install "]
			}},
		{"a toggle list rebuilds when the overlay stacked on it closes, after its pick",
			hostapi.Overlay{ID: "p", Items: []any{"old"}}, nil,
			func(a *App) string {
				a.overlays[0].o.Reload = func() ([]any, [][]hostapi.Segment) { return []any{a.composer.text}, nil }
				a.openOverlay(hostapi.Overlay{ID: "c", Origin: lua.OriginBuiltin, Items: []any{map[string]any{"text": "pick", "fill": "new"}}})
				a.overlayKey("enter", "")
				return map[bool]string{false: fmt.Sprintf("overlays=%d", len(a.overlays))}[len(a.overlays) == 1 && a.overlays[0].texts[0] == "new"]
			}},
		{"opening an open id replaces it in place with the selection kept, unless a lower origin opens it",
			hostapi.Overlay{ID: "p", Items: []any{"a", "b"}}, []string{"down"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []any{"c", "d"}})
				if len(a.overlays) != 1 || a.overlays[0].selected != 1 || a.overlays[0].texts[1] != "d" {
					return fmt.Sprintf("overlays=%d selected=%d", len(a.overlays), a.overlays[0].selected)
				}
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginPlugin, Items: []any{"e"}})
				return map[bool]string{false: "a plugin replaced a builtin list"}[len(a.overlays) == 2 && a.overlays[0].texts[0] == "c"]
			}},
		{"a refresh replaces an open id and opens nothing once it closed",
			hostapi.Overlay{ID: "p", Items: []any{"a"}}, []string{"esc"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []any{"b"}, Refresh: true})
				return map[bool]string{false: "a closed list came back"}[len(a.overlays) == 0]
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
				return map[bool]string{false: "still open"}[len(a.overlays) == 0 && a.armed == armed{}]
			}},
		{"the list keys scroll a body with no list",
			hostapi.Overlay{Body: strings.Repeat("line\n", 50)}, []string{"down"},
			func(a *App) string { return map[bool]string{false: "no scroll"}[a.overlays[0].body.top == 1] }},
		{"a multi-line label stays on one row",
			hostapi.Overlay{Items: []any{"fix: empty input\n\nThe parser crashed."}}, nil,
			func(a *App) string {
				return map[bool]string{false: "label kept its newlines"}[a.overlays[0].items[0].label == "fix: empty input The parser crashed."]
			}},
		{"a newline in the title or empty text adds no terminal line",
			hostapi.Overlay{Title: "a\nb", Empty: "p\nq"}, nil,
			func(a *App) string {
				for _, row := range a.overlayBox(&a.overlays[0], 80, 20) {
					if strings.Contains(plainSegs(row), "\n") {
						return fmt.Sprintf("row %q holds a newline", plainSegs(row))
					}
				}
				return ""
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
				a.overlayBox(&a.overlays[len(a.overlays)-1], 80, 20) // keys act on the box drawn
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
		box := a.overlayBox(&a.overlays[0], 80, 20)
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
		{hostapi.Overlay{Items: []any{"inert"}}, false, "y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "fill": "x"}}, Verb: "run"}, false, "enter fill · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "detail": "x"}}, Verb: "pick"}, false, "enter open · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "run": "x"}}, Origin: lua.OriginBuiltin, Verb: "pick"}, false, "enter pick · y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "run": "x", "verb": "open"}}, Verb: "pick", Reload: func() ([]any, [][]hostapi.Segment) { return nil, nil }}, false, "enter open · y copy · esc close"},
		{hostapi.Overlay{Body: "text"}, false, "y copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "copy": "export X"}}}, false, "enter copy · esc close"},
		{hostapi.Overlay{Items: []any{map[string]any{"text": "a", "fill": "x"}}}, true, "enter fill · esc back"},
		{hostapi.Overlay{Body: "text", Items: []any{map[string]any{"text": "a", "fill": "x"}}}, true, "enter fill · esc back"},
		{hostapi.Overlay{Body: strings.Repeat("line\n", 10), Items: slices.Repeat([]any{map[string]any{"text": "a", "fill": "x"}}, 30)}, false, "enter fill · pgup/pgdn scroll · y copy · esc close"},
	} {
		if got := strings.Trim(hints(c.o, c.filtering), "│ "); got != c.want {
			t.Errorf("%v: %q, want %q", c.o.Items, got, c.want)
		}
	}
}

// TestHintOrder: pairs sort into their slots, and a narrow row drops the
// middle pairs last first, never enter or esc.
func TestHintOrder(t *testing.T) {
	pairs := []hintPair{{"esc", "close"}, {"y", "copy"}, {"a", "keep"}, {"enter", "open"}, {"d", "drop"}, {"pgup/pgdn", "scroll"}}
	for _, c := range []struct {
		w    int
		want string
	}{
		{80, "enter open · a keep · d drop · pgup/pgdn scroll · y copy · esc close"},
		{40, "enter open · a keep · d drop · esc close"},
		{5, "enter open · esc close"},
	} {
		if got := plainSegs(hint(c.w, pairs...)); got != c.want {
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
	col := func(box [][]hostapi.Segment) int { return strings.Index(plainSegs(box[2]), "about") }
	for _, q := range []string{"item", "xxx"} {
		st.active, st.query = true, q
		st.apply(st.texts)
		got := app.overlayBox(st, 80, 40)
		if segsWidth(got[0]) != segsWidth(box[0]) || len(got) != len(box) {
			t.Fatalf("filter %q: %dx%d, want %dx%d", q, segsWidth(got[0]), len(got), segsWidth(box[0]), len(box))
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
		{"filter", func() { st.query = "item 4"; st.refilter() }, true},
		{"resize", func() {}, true},
	} {
		c.step()
		before := st.rows
		app.overlayBox(st, map[bool]int{true: 60, false: 80}[c.name == "resize"], 20)
		if built := before == nil || &before[0] != &st.rows[0]; built != c.built {
			t.Errorf("%s: rows built %v, want %v", c.name, built, c.built)
		}
	}
	if got := plainSegs(st.rows[0]); !strings.Contains(got, "item 04") {
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

	go app.AskPermission(hostapi.PermRequest{Subject: "tool.bash", Arg: "make test"})
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	app.Frame()
	clickAt(app, 0, 0)
	if app.permAsk == nil {
		t.Fatal("a stray click answered a permission ask")
	}
	app.answerPerm(false, "user")
}

// TestListDescColumn: descriptions start in one column, and a row that
// does not fit gives way from its description, never its label or key.
// TestListDescUnderLabel: a list too narrow for its description column
// puts each description under its label instead of dropping them all.
func TestListDescUnderLabel(t *testing.T) {
	items := []listItem{{label: "shift+enter, alt+enter, ctrl+j", desc: "newline"}, {label: "esc", desc: "back, or twice to interrupt"}}
	var got []string
	for _, row := range listRows(items, 0, nil, 10, 30) {
		got = append(got, strings.TrimRight(plainSegs(row), " "))
	}
	if want := "› shift+enter, alt+enter…|  newline|  esc|  back, or twice to interrupt"; strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
}

func TestListDescColumn(t *testing.T) {
	for _, c := range []struct {
		name  string
		items []listItem
	}{
		{"a long title with a short description", []listItem{
			{label: "a much longer session title here", desc: "now · short"},
			{label: "done", desc: "now · 2 messages · hello number 3 with a longer message to title it", key: "current"},
		}},
		{"a picker keeps its ids whole", []listItem{
			{label: "deepseek/deepseek-v4-flash", desc: "DeepSeek-V4-Flash · 1M · $0.14/$0.28"},
			{label: "fake/m", desc: "128k", key: "current"},
			{label: "fake/deepseek-v4-flash-long-model-name", desc: "1M"},
		}},
	} {
		col := -1
		for i, row := range listRows(c.items, 0, nil, 10, 52) {
			text, it := plainSegs(row), c.items[i]
			if !strings.Contains(text, it.label) || !strings.HasSuffix(text, it.key) || strings.Contains(text, "·…") {
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
	first, second := rows[2], rows[3]
	if first[2].HL != "ToastErr" || first[2].Click != "" || first[3].HL != "Host:Value" {
		t.Fatalf("first row: %+v", first)
	}
	if !strings.Contains(plainSegs(second), "…") || second[2].HL != "Title" {
		t.Fatalf("selected row: %+v", second)
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
	if d := app.overlays[1]; !d.o.BodyMD || strings.Contains(plainSegs(app.overlayBox(&d, 60, 16)[1]), "*") {
		t.Fatalf("detail: %+v", d.o)
	}
}

// TestOverlayActionsAndKeys: an action with an id goes to on_action with
// its item and draws in its hl, and a list key, named in the footer, runs
// its fn with the selected item; both close the list like on_select.
func TestOverlayActionsAndKeys(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 16)
	open := `zemi.ui.overlay.open({ id = "k", items = { "a", { text = "b", actions = { { text = "Delete", id = "del", hl = "Danger" }, { text = "Back" } } } },
  on_action = function(item, action) zemi.notify({ body = item.text .. ":" .. action.id }) end,
  keys = { e = { desc = "edit", fn = function(item) zemi.notify({ body = "e:" .. item.text }) end } } })`
	runLua(t, app, open+`
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { esc = { fn = function() end } } }), "a key of several characters must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { e = { desc = "no fn" } } }), "a key with no fn must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", keys = { y = { fn = function() end } } }), "a key the list takes itself must raise")`)
	box := app.overlayBox(&app.overlays[0], 60, 16)
	if hint := plainSegs(box[len(box)-2]); !strings.Contains(hint, "e edit · y copy · esc close") {
		t.Fatalf("footer: %q", hint)
	}
	for _, c := range []struct{ keys, want string }{
		{"j enter 2", "k"}, // Back has no id: it returns to the list
		{"j enter 1", "b:del"},
		{"j e", "e:b"},
	} {
		if len(app.overlays) == 0 {
			runLua(t, app, open)
		}
		keys := strings.Fields(c.keys)
		pressKeys(app, keys[:len(keys)-1]...)
		if len(keys) == 3 {
			box := slices.Concat(app.overlayBox(&app.overlays[1], 60, 16)...)
			if i := slices.IndexFunc(box, func(s hostapi.Segment) bool { return s.Text == "Delete" }); i < 0 || box[i].HL != "Danger" {
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
    if on.a and on.b then error("boom") end
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
		if err := host.RT.ExecuteChunk(context.Background(), src, "t.lua", origin); err != nil {
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
		name, body string
		want       []string
	}{
		{"an error's value runs to the blank line", "url: https://x.dev/mcp\nerror: one two three four five six seven eight nine ten\nstack traceback:\n\nplain text",
			[]string{"url    https://x.dev/mcp ", "error  one two three four five six  Host:Danger", "       seven eight nine ten Host:Danger", "       stack traceback: Host:Danger", "", "plain text"}},
		{"a key inside an error is its text", "at: x.lua:1\nerror: boom\npanic: runtime error",
			[]string{"at     x.lua:1 ", "error  boom Host:Danger", "       panic: runtime error Host:Danger"}},
		{"only the opening lines are fields", "status: ready\n\nsecurity: audits the auth code\n\nsource: user",
			[]string{"status  ready Host:Ok", "", "security: audits the auth code", "", "source: user"}},
	} {
		var got []string
		for _, r := range fieldRows(c.body, 40) {
			hl := ""
			if len(r) > 1 {
				hl = " " + r[1].HL
			}
			got = append(got, plainSegs(r)+hl)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: rows %q, want %q", c.name, got, c.want)
		}
	}
}
