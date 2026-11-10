package ui

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// fail is "" when ok, and what the check saw otherwise: a check states its own
// condition, where a map keyed by it can have its polarity flipped into a pass.
func fail(ok bool, seen any) string {
	if ok {
		return ""
	}
	return fmt.Sprint(seen)
}

// TestOverlayFilter: "/" types a query that narrows the rows (all words,
// any case, a row's section and value too), enter keeps it and gives the
// keys back to the list, esc clears it before closing, and a new query
// selects its first match.
func TestOverlayFilter(t *testing.T) {
	var fixture []hostapi.OverlayItem
	for _, s := range []string{"alpha one", "Beta two", "gamma three", "xyz", "qqq"} { // filterMin rows
		fixture = append(fixture, hostapi.OverlayItem{Text: s, Fill: s})
	}
	keyed := slices.Clone(fixture)
	keyed[2] = hostapi.OverlayItem{Text: "gamma three", Keys: []hostapi.ItemKey{{Key: "3", Desc: "gamma three", Fill: "gamma three"}}}
	models := []hostapi.OverlayItem{
		{Text: "gpt", Section: "openai"},
		{Text: "sonnet", Section: "anthropic"},
		{Text: "opus", Section: "anthropic", Copy: "anthropic/opus"},
		{Text: "llama", Section: "meta"},
		{Text: "qwen", Section: "meta"},
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
		{"enter on a typed filter acts on the row", hostapi.Overlay{}, runes("/t", "enter"), func(a *App) string { return closedWith(a, "Beta two") }},
		{"esc while typing clears and keeps the row", hostapi.Overlay{}, runes("/ga", "esc"), func(a *App) string { return state(a, false, "", 5, 2) }},
		{"backspace on an empty query stops typing", hostapi.Overlay{}, runes("/", "backspace"), func(a *App) string { return state(a, false, "", 5, 0) }},
		{"the composer's editing keys work in a filter", hostapi.Overlay{}, runes("/gamma three", "ctrl+a", "right", "alt+f", "ctrl+e", "ctrl+a", "delete"), func(a *App) string {
			st := a.overlays[0]
			if st.query != "amma three" || st.cursor != 0 {
				return fmt.Sprintf("query=%q cursor=%d", st.query, st.cursor)
			}
			return state(a, true, "amma three", len(st.visible), st.selected)
		}},
		{"j moves while no filter types", hostapi.Overlay{}, runes("jj"), func(a *App) string { return state(a, false, "", 5, 2) }},
		{"a bare letter with no bind is a no-op", hostapi.Overlay{}, runes("b"), func(a *App) string { return state(a, false, "", 5, 0) }},
		{"a no-match footer drops enter, esc still clears", hostapi.Overlay{}, runes("/zz"), func(a *App) string {
			if s := state(a, true, "zz", 0, 0); s != "" {
				return s
			}
			box := a.overlayBox(&a.overlays[0], 80, 20, true)
			foot := hostapi.SegsText(box[len(box)-2])
			if strings.Contains(foot, "enter") {
				return foot
			}
			return fail(strings.Contains(foot, "esc clear"), foot)
		}},
		{"enter on a kept query acts on the row", hostapi.Overlay{}, runes("/e", "enter", "enter"), func(a *App) string { return closedWith(a, "Beta two") }},
		{"a typing filter takes the key", hostapi.Overlay{Items: keyed}, runes("/3"), func(a *App) string { return state(a, true, "3", 0, 0) }},
		{"a row's key acts on the selected row", hostapi.Overlay{Items: keyed}, runes("jj3"), func(a *App) string { return closedWith(a, "gamma three") }},
		{"/ on a list of fewer than filterMin rows is inert and draws no filter row", hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}}}, runes("/"), func(a *App) string {
			if box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20, true)); strings.Contains(box, "filter") {
				return box
			}
			return state(a, false, "", 2, 0)
		}},
		{"a section name narrows to its rows and drops the other header", hostapi.Overlay{Items: models}, runes("/anthropic"), func(a *App) string {
			if v := a.overlays[0].visible; !slices.Equal(v, []int{2, 1}) || strings.Contains(fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20, true)), "openai") {
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
		{"a render_item row keeps its spaces, so the highlight lands on what matched", hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "x"}, {Text: "q1"}, {Text: "q2"}, {Text: "q3"}, {Text: "q4"}, {Text: "sonnet", Row: []hostapi.Segment{{Text: "●  "}, {Text: "sonnet", HL: "Ok"}}}}}, runes("/net"), func(a *App) string {
			var lit []string
			for _, row := range a.overlayBox(&a.overlays[0], 80, 20, true) {
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
				text := ""
				if k == "space" {
					text = " "
				}
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

// TestOverlayHoldsTheKeys: an open box holds every key, so typing lands
// nowhere and the draft under it keeps its text, while the box's own keys
// still act: a reading box closes on esc and enter, and a short list's
// letters die instead of drafting.
func TestOverlayHoldsTheKeys(t *testing.T) {
	t.Parallel()
	typeKey := func(a *App, k string) {
		a.handleKey(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	}
	a := newTestApp(t, t.TempDir())
	a.composer.text, a.composer.cursor = "kept", 4

	// a reading box (a /btw answer): typing dies, enter closes it read
	a.openOverlay(hostapi.Overlay{ID: "r", Title: "Reply", Origin: lua.OriginBuiltin, Body: "the answer"})
	typeKey(a, "x")
	if a.composer.text != "kept" || len(a.overlays) != 1 {
		t.Fatalf("a reading box let a key through: %q, %d boxes", a.composer.text, len(a.overlays))
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(a.overlays) != 0 || a.composer.text != "kept" {
		t.Fatalf("enter on a reading box: %d boxes, draft %q", len(a.overlays), a.composer.text)
	}

	// a list too short to filter: letters drop, the draft is kept
	a.openOverlay(hostapi.Overlay{
		ID: "s", Title: "Short", Origin: lua.OriginBuiltin,
		Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}},
	})
	typeKey(a, "z")
	if a.composer.text != "kept" {
		t.Fatalf("a short list handed a key to the draft: %q", a.composer.text)
	}
	if st := a.overlays[0]; st.active || st.query != "" {
		t.Fatalf("a short list started a filter: %+v", st.listFilter)
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(a.overlays) != 0 {
		t.Fatal("esc did not close the box")
	}
	typeKey(a, "!")
	if a.composer.text != "kept!" {
		t.Fatalf("the composer did not take the keys back: %q", a.composer.text)
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

	a.openOverlay(hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}, {Text: "c"}}})
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
	t.Cleanup(app.stopWork) // a fake program's worker ends with the test
	busy, hold := make(chan struct{}), make(chan struct{})
	go app.host.RT.Do(t.Context(), lua.ClassUI, 0, 0, "test", func(*glua.LState) ([]any, error) {
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
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.ui.select({ "a" }, {}, function() end)`)
	defer pressWhileLuaBusy(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})()
	if len(app.overlays) != 0 {
		t.Fatal("the picker should close at once")
	}
}

// TestPaletteLuaCompleterOffKeyLoop: typing past "/name " returns while the
// interpreter is busy, and the Lua completer's answer fills the popup once
// it is free.
func TestPaletteLuaCompleterOffKeyLoop(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.command.add("greet", function() end, { nargs = "?", complete = function() return { "alice", "bob" } end })`)
	var keys []tea.KeyPressMsg
	for _, r := range "/greet b" {
		keys = append(keys, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	pressWhileLuaBusy(t, app, keys...)()
	deadline := time.After(2 * time.Second)
	for len(app.completion.argRows) == 0 {
		select {
		case msg := <-app.hooks:
			app.Update(msg)
		case <-deadline:
			t.Fatal("the completer never answered")
		}
	}
	if len(app.completion.argRows) != 1 || app.completion.argRows[0].label != "bob" {
		t.Fatalf("argument rows %v", app.completion.argRows)
	}
	// the next key keeps the popup and its rows until the completer answers
	app.renderSync(t)
	size := app.completion.size
	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if !app.completion.shown() || app.completion.size != size {
		t.Fatalf("shown=%v size %v, was %v", app.completion.shown(), app.completion.size, size)
	}
}

// TestBootFailedBox: a boot that failed leaves a runtime with no theme, no
// widgets and no Lua at all, where the only screen left is Go's. The box
// states the error and the log, the composer gives way to it, `c` copies the
// error, and a boot that succeeds takes it away.
func TestBootFailedBox(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 90, 24)
	host.PurgeGeneration() // what a failed boot leaves behind: no theme, no widgets
	app.SignalBoot(errors.New("boot step init.lua: init.lua:12: attempt to call a nil value (global 'pending')"))
	for app.Pump() {
	}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"Boot failed", "boot step init.lua:", "The whole traceback is in the log", "c copy the error", "q quit"} {
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
	detail := func(keys ...hostapi.ItemKey) hostapi.OverlayItem {
		return hostapi.OverlayItem{Text: "err", Desc: "short", Detail: "line one\nline two", Keys: keys}
	}
	key := func(k, desc, run string) hostapi.ItemKey {
		return hostapi.ItemKey{Key: k, Desc: desc, Run: run}
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
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}}},
			[]string{"enter"},
			func(a *App) string { return fail(len(a.overlays) == 1, "closed") },
		},
		{
			"enter on a copy row toasts and keeps the list open",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "deepseek", Copy: "export X"}}},
			[]string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 1 || len(a.toasts) != 1 || a.toasts[0].title != "Copied" {
					return fmt.Sprintf("overlays=%d toasts=%+v", len(a.overlays), a.toasts)
				}
				return ""
			},
		},
		{
			"a value picker's enter copies and closes",
			hostapi.Overlay{CloseOnCopy: true, Items: []hostapi.OverlayItem{{Text: "1  go", Copy: "x := 1"}}},
			[]string{"enter"},
			func(a *App) string {
				if len(a.overlays) != 0 || len(a.toasts) != 1 || a.toasts[0].title != "Copied" {
					return fmt.Sprintf("overlays=%d toasts=%+v", len(a.overlays), a.toasts)
				}
				return ""
			},
		},
		{
			"a value picker's y closes it too",
			hostapi.Overlay{CloseOnCopy: true, Items: []hostapi.OverlayItem{{Text: "1  go", Copy: "x := 1"}}},
			nil,
			func(a *App) string {
				a.overlayKey("y", "")
				if len(a.overlays) != 0 || len(a.toasts) != 1 || a.toasts[0].title != "Copied" {
					return fmt.Sprintf("overlays=%d toasts=%+v", len(a.overlays), a.toasts)
				}
				return ""
			},
		},
		{
			"y copies a row's value in place of its text, and enter still runs the row",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "gpt-5", Desc: "400k", Run: "nope", Copy: "openai/gpt-5\x1b[2J"}}},
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
			"a verb only names the hint: a row's run still runs",
			hostapi.Overlay{ID: "copy", Origin: lua.OriginBuiltin, Verb: "copy", Items: []hostapi.OverlayItem{{Text: "Whole reply", Run: "copy reply"}}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "copy", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
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
				if len(a.overlays) != 1 || !strings.Contains(fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20, true)), "Nothing to see.") {
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
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20, true))
				return fail(strings.Contains(box, "header") && strings.Contains(box, "Nothing cached yet."), box)
			},
		},
		{
			"a reload list whose rows only open says nothing of space",
			hostapi.Overlay{Items: []hostapi.OverlayItem{detail()}, Reload: func() []hostapi.OverlayItem { return nil }},
			nil,
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[0], 80, 20, true))
				if strings.Contains(box, "toggle") {
					return box
				}
				return ""
			},
		},
		{
			"enter opens the detail and esc goes back with the selection kept",
			hostapi.Overlay{Title: "Errors", Items: []hostapi.OverlayItem{{Text: "a"}, detail()}},
			[]string{"down", "enter", "esc"},
			func(a *App) string {
				if len(a.overlays) != 1 || a.overlays[0].selected != 1 {
					return fmt.Sprintf("overlays=%d selected=%d", len(a.overlays), a.overlays[0].selected)
				}
				return ""
			},
		},
		{
			"going back from a toggle list's detail, by esc or enter, does not rebuild it",
			hostapi.Overlay{Items: []hostapi.OverlayItem{detail()}, Reload: func() []hostapi.OverlayItem { backs++; return nil }},
			[]string{"enter", "esc", "enter", "esc"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && backs == 0, fmt.Sprintf("overlays=%d rebuilds=%d", len(a.overlays), backs))
			},
		},
		{
			"the detail view has a breadcrumb title",
			hostapi.Overlay{Title: "Errors", Items: []hostapi.OverlayItem{detail()}},
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
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "trust", Detail: "trusted: ~/x"}}},
			[]string{"enter"},
			func(a *App) string {
				box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20, true))
				return fail(strings.Contains(box, "trusted: ~/x"), box)
			},
		},
		{
			"without a copy field, y copies the label alone, not its desc",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "deepseek", Desc: "400k  3m ago"}}},
			nil,
			func(a *App) string {
				batch, _ := a.overlayKey("y", "")().(tea.BatchMsg)
				return fail(fmt.Sprint(batch[0]()) == "deepseek", fmt.Sprintf("y copied %q", fmt.Sprint(batch[0]())))
			},
		},
		{
			"an empty row copies nothing, and Y skips it",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{}, {Text: "b"}}},
			nil,
			func(a *App) string {
				if cmd := a.overlayKey("y", ""); cmd != nil || len(a.toasts) != 0 {
					return fmt.Sprintf("y on an empty row copied: cmd=%v toasts=%d", cmd != nil, len(a.toasts))
				}
				batch, _ := a.overlayKey("Y", "")().(tea.BatchMsg)
				return fail(fmt.Sprint(batch[0]()) == "b", fmt.Sprintf("Y copied %q", fmt.Sprint(batch[0]())))
			},
		},
		{
			"a view's pending line sits pinned under a long body",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "run", Detail: strings.Repeat("line\n", 40), Empty: "thinking"}}},
			[]string{"enter"},
			func(a *App) string {
				d := &a.overlays[1]
				at := func() (int, string) {
					for i, r := range a.overlayBox(d, 80, 20, true) {
						for _, sg := range r {
							if sg.Text == "thinking" {
								return i, sg.HL
							}
						}
					}
					return -1, ""
				}
				i, style := at()
				a.wheel(0, 0, 1)
				d.selected = 1
				j, _ := at()
				return fail(i > 0 && style == hostapi.ChromeHint && i == j, fmt.Sprintf("pending row %d -> %d, hl %q", i, j, style))
			},
		},
		{
			"a rebuilt view's pending line drops once the answer landed",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "run", Detail: "the task", Empty: "thinking"}}},
			[]string{"enter"},
			func(a *App) string {
				next := a.overlays[0].o
				next.Items = []hostapi.OverlayItem{{Text: "run", Detail: "the task\n\nthe answer"}}
				a.replaceOverlay(0, next)
				a.refreshDetail(0)
				var box string
				for _, r := range a.overlayBox(&a.overlays[1], 80, 20, true) {
					box += hostapi.SegsText(r) + "\n"
				}
				return fail(!strings.Contains(box, "thinking") && strings.Contains(box, "the answer"), box)
			},
		},
		{
			"y copies a view's body alone, not the facts table before it",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "mcp", Detail: "prose", Fields: []hostapi.Field{{Name: "status", Value: "ready"}}}}},
			[]string{"enter"},
			func(a *App) string {
				if box := fmt.Sprint(a.overlayBox(&a.overlays[1], 80, 20, true)); !strings.Contains(box, "{ready Host:Ok") {
					return box
				}
				batch, _ := a.overlayKey("y", "")().(tea.BatchMsg)
				return fail(fmt.Sprint(batch[0]()) == "prose", fmt.Sprintf("y copied %q", fmt.Sprint(batch[0]())))
			},
		},
		{
			"a view with no body copies its fields",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "mcp", Fields: []hostapi.Field{{Name: "status", Value: "ready"}}}}},
			[]string{"enter"},
			func(a *App) string {
				batch, _ := a.overlayKey("y", "")().(tea.BatchMsg)
				return fail(fmt.Sprint(batch[0]()) == "status: ready", fmt.Sprintf("y copied %q", fmt.Sprint(batch[0]())))
			},
		},
		{
			"a detail view takes its own height and widens to its breadcrumb",
			hostapi.Overlay{Title: "A long parent title", Items: []hostapi.OverlayItem{{Text: strings.Repeat("x", 50), Detail: "d"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"}, {Text: "f"}, {Text: "g"}}},
			[]string{"enter"},
			func(a *App) string {
				list, detail := a.overlayBox(&a.overlays[0], 120, 20, false), a.overlayBox(&a.overlays[1], 120, 20, true)
				if len(detail) >= len(list) || hostapi.SegsWidth(detail[0]) <= hostapi.SegsWidth(list[0]) || strings.Contains(hostapi.SegsText(detail[0]), "…") {
					return fmt.Sprintf("list %d rows %d wide, detail %d rows %q", len(list), hostapi.SegsWidth(list[0]), len(detail), hostapi.SegsText(detail[0]))
				}
				return ""
			},
		},
		{
			"a field repeating the breadcrumb shows only when the title is cut",
			hostapi.Overlay{Title: "Projects", Items: []hostapi.OverlayItem{{Text: "~/code/app", Fields: []hostapi.Field{{Name: "trust", Value: "trusted"}, {Name: "path", Value: "~/code/app"}}}}},
			[]string{"enter"},
			func(a *App) string {
				body := func(w int) string {
					var out []string
					for _, r := range a.overlayBox(&a.overlays[1], w, 20, true) {
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
			"the wheel scrolls a detail body, and the box keeps its width",
			hostapi.Overlay{Title: "Errors", Items: []hostapi.OverlayItem{detail(key("a", "Ask", "nope"), key("b", "Back", "nope"))}},
			[]string{"enter"},
			func(a *App) string {
				d := &a.overlays[1]
				d.o.Body = strings.Repeat("line\n", 60)
				before := hostapi.SegsWidth(a.overlayBox(d, 80, 20, true)[0])
				a.wheel(0, 0, 1)
				d.selected = 1
				if after := hostapi.SegsWidth(a.overlayBox(d, 80, 20, true)[0]); d.body.top != 1 || after != before {
					return fmt.Sprintf("scroll=%d width %d -> %d", d.body.top, before, after)
				}
				return ""
			},
		},
		{
			"a view lists nothing, so typing and enter leave it as it was",
			hostapi.Overlay{Items: []hostapi.OverlayItem{detail(key("a", "Ask", "nope"))}},
			[]string{"enter"},
			func(a *App) string {
				for _, k := range []string{"/", "x", "enter", "j"} {
					a.overlayKey(k, k)
				}
				d := a.overlays[len(a.overlays)-1]
				return fail(len(a.overlays) == 2 && len(d.visible) == 0 && !d.active && d.back, fmt.Sprintf("%d overlays, visible=%v filtering=%v", len(a.overlays), d.visible, d.active))
			},
		},
		{
			"a row key that fills closes the list",
			hostapi.Overlay{Items: []hostapi.OverlayItem{detail(hostapi.ItemKey{Key: "u", Desc: "Use", Fill: "/use "})}},
			[]string{"u"},
			func(a *App) string {
				if len(a.overlays) != 0 || a.composer.text != "/use " {
					return fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text)
				}
				return ""
			},
		},
		{
			"a row that opens a screen leaves its list under it, for esc to come back to",
			hostapi.Overlay{ID: "m", Items: []hostapi.OverlayItem{{Text: "Choose a model per role", Verb: "open", Run: "nope"}}},
			[]string{"enter"},
			func(a *App) string { return fail(len(a.overlays) == 1, "closed") },
		},
		{
			"a row key in a toggle list rebuilds the list",
			hostapi.Overlay{Items: []hostapi.OverlayItem{detail(key("t", "Toggle", "nope"))}, Reload: func() []hostapi.OverlayItem { reloads++; return []hostapi.OverlayItem{detail()} }},
			[]string{"t"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && reloads == 1, "not rebuilt")
			},
		},
		{
			"space passes by a toggle list row with its own verb",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "Choose a model per role", Verb: "open", Run: "nope"}}, Reload: func() []hostapi.OverlayItem { spaced++; return nil }},
			[]string{"space"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && spaced == 0, "space ran the row")
			},
		},
		{
			"a fill row closes a toggle list, so the composer has the keys",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "Install", Verb: "install", Fill: "/plugin install "}}, Reload: func() []hostapi.OverlayItem { return nil }},
			[]string{"enter"},
			func(a *App) string {
				return fail(len(a.overlays) == 0 && a.composer.text == "/plugin install ", fmt.Sprintf("overlays=%d composer=%q", len(a.overlays), a.composer.text))
			},
		},
		{
			"a toggle list rebuilds when the overlay stacked on it closes, after its pick",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "old"}}},
			nil,
			func(a *App) string {
				a.overlays[0].o.Reload = func() []hostapi.OverlayItem { return []hostapi.OverlayItem{{Text: a.composer.text}} }
				a.openOverlay(hostapi.Overlay{ID: "c", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "pick", Fill: "new"}}})
				a.overlayKey("enter", "")
				return fail(len(a.overlays) == 1 && a.overlays[0].labels[0] == "new", fmt.Sprintf("overlays=%d", len(a.overlays)))
			},
		},
		{
			"opening an open id replaces it in place with the selection on its row, else at its place",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}}},
			[]string{"down"},
			func(a *App) string {
				for _, c := range []struct {
					items []hostapi.OverlayItem
					sel   int
				}{{[]hostapi.OverlayItem{{Text: "x"}, {Text: "y"}, {Text: "b"}}, 2}, {[]hostapi.OverlayItem{{Text: "c"}, {Text: "d"}, {Text: "e"}}, 2}} {
					a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: c.items})
					if len(a.overlays) != 1 || a.overlays[0].selected != c.sel {
						return fmt.Sprintf("%v: overlays=%d selected=%d", c.items, len(a.overlays), a.overlays[0].selected)
					}
				}
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginUser, Items: []hostapi.OverlayItem{{Text: "e"}}})
				return fail(len(a.overlays) == 1 && a.overlays[0].labels[0] == "e", "a user open did not replace the list")
			},
		},
		{
			"a refresh replaces an open id and opens nothing once it closed",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "a"}}},
			[]string{"esc"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "b"}}, Refresh: true})
				return fail(len(a.overlays) == 0, "a closed list came back")
			},
		},
		{
			"a refresh of a closed list builds nothing and wakes no frame",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "a"}}},
			[]string{"esc"},
			func(a *App) string {
				gen, built := a.frameGen, false
				a.RefreshOverlay("p", func() hostapi.Overlay { built = true; return hostapi.Overlay{} })
				return fail(!built && a.frameGen == gen, fmt.Sprintf("built %v, frame moved %d", built, a.frameGen-gen))
			},
		},
		{
			"a refresh rebuilds the view open over the list from its item, and drops it with the item",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{detail(hostapi.ItemKey{Key: "a"}, hostapi.ItemKey{Key: "b"}), {Text: "x"}}},
			[]string{"enter"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []hostapi.OverlayItem{{Text: "x"}, {Text: "err", Detail: "line three", Keys: []hostapi.ItemKey{{Key: "a", Run: "x"}, {Key: "b", Run: "y"}}}}})
				if d := a.overlays[len(a.overlays)-1]; len(a.overlays) != 2 || d.o.Body != "line three" {
					return fmt.Sprintf("overlays=%d body=%q", len(a.overlays), d.o.Body)
				}
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []hostapi.OverlayItem{{Text: "x"}}})
				return fail(len(a.overlays) == 1, "the gone item's view stayed")
			},
		},
		{
			"a refresh keeps an armed danger key while the same key runs the same command",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "run", Keys: []hostapi.ItemKey{{Key: "x", Desc: "Stop", Danger: true, Run: "stop 1"}}}}},
			[]string{"x"},
			func(a *App) string {
				item := func(run string) hostapi.Overlay {
					return hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []hostapi.OverlayItem{{Text: "run", Keys: []hostapi.ItemKey{{Key: "x", Desc: "Stop", Danger: true, Run: run}}}}}
				}
				if a.openOverlay(item("stop 1")); a.overlays[0].keyArmed != "x" {
					return "the arm was dropped"
				}
				a.openOverlay(item("stop 2"))
				return fail(a.overlays[0].keyArmed == "", "an arm outlived its command")
			},
		},
		{
			"a refresh keeps an armed danger row of the list itself",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "Stop", Danger: true, Run: "stop 1"}}},
			[]string{"enter"},
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Refresh: true, Items: []hostapi.OverlayItem{{Text: "Stop", Danger: true, Run: "stop 1"}}})
				return fail(a.overlays[0].armed, "the list's arm was dropped")
			},
		},
		{
			"a list key runs its command and its screen takes over, whatever opened the list",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "x"}}, Keys: []hostapi.OverlayKey{{Key: "p", Desc: "pause", Run: "zz pause"}}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
				})
				if a.overlayKey("p", "p"); ran != "pause" || len(a.overlays) != 0 {
					return "ran " + ran
				}
				ran = ""
				a.openOverlay(hostapi.Overlay{ID: "q", Origin: lua.OriginPlugin, Items: []hostapi.OverlayItem{{Text: "x"}}, Keys: []hostapi.OverlayKey{{Key: "p", Desc: "pause", Run: "zz pause"}}})
				a.overlayKey("p", "p")
				return fail(ran == "pause" && len(a.overlays) == 0, "a plugin list's key ran "+ran)
			},
		},
		{
			"a list key that rebuilds keeps the list open",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "x"}}, Keys: []hostapi.OverlayKey{{Key: "p", Desc: "pause", Run: "zz pause"}}, Reload: func() []hostapi.OverlayItem { return []hostapi.OverlayItem{{Text: "x"}} }},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
				})
				a.overlayKey("p", "p")
				return fail(ran == "pause" && len(a.overlays) == 1, "ran "+ran)
			},
		},
		{
			"a go row key closes a list that rebuilds",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "x", Keys: []hostapi.ItemKey{{Key: "r", Desc: "resume", Run: "zz resume", Verb: hostapi.VerbGo}}}}, Reload: func() []hostapi.OverlayItem {
				return []hostapi.OverlayItem{{Text: "x", Keys: []hostapi.ItemKey{{Key: "r", Desc: "resume", Run: "zz resume", Verb: hostapi.VerbGo}}}}
			}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
				})
				a.overlayKey("r", "r")
				return fail(ran == "resume" && len(a.overlays) == 0, "ran "+ran)
			},
		},
		{
			"a danger list key acts on its second press, which esc and a wheel drop and a refresh keeps",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "x"}}},
			nil,
			func(a *App) string {
				ran := ""
				a.host.Commands.Add(&hostapi.Command{
					Name: "zz", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
					GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
				})
				o := hostapi.Overlay{ID: "p", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "x"}}, Keys: []hostapi.OverlayKey{{Key: "x", Desc: "stop workflow", Run: "zz stop", Danger: true}}}
				a.openOverlay(o)
				a.overlayKey("x", "x")
				a.overlayKey("esc", "")
				a.overlayKey("x", "x")
				a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
				if a.overlayKey("x", "x"); ran != "" || len(a.overlays) != 1 {
					return "the first press ran " + ran
				}
				if foot := hostapi.SegsText(a.overlayBox(&a.overlays[0], 80, 20, true)[3]); !strings.Contains(foot, "x again to stop workflow") {
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
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "x"}}, Wide: true},
			nil,
			func(a *App) string {
				a.overlayBox(&a.overlays[0], 120, 30, true)
				return fail(a.overlays[0].size.w == boxInner(120), fmt.Sprintf("%d cells wide", a.overlays[0].size.w))
			},
		},
		{
			"closing the parent drops its detail view",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{detail()}},
			[]string{"enter"},
			func(a *App) string {
				a.closeOverlayByID("p")
				return fail(len(a.overlays) == 0, "detail left")
			},
		},
		{
			"ctrl+c on a detail view closes the whole menu",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{detail()}},
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
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "fix: empty input\n\nThe parser crashed."}}},
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
				for _, row := range a.overlayBox(&a.overlays[0], 80, 20, true) {
					if strings.Contains(hostapi.SegsText(row), "\n") {
						return fmt.Sprintf("row %q holds a newline", hostapi.SegsText(row))
					}
				}
				return ""
			},
		},
		{
			"Selected picks the first item",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}, {Text: "c"}}, Selected: 2},
			nil,
			func(a *App) string { return fail(a.overlays[0].selected == 2, "wrong selection") },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			c.o.Origin = cmp.Or(c.o.Origin, lua.OriginBuiltin)
			a.openOverlay(c.o)
			for _, k := range c.keys {
				a.overlayBox(&a.overlays[len(a.overlays)-1], 80, 20, true) // keys act on the box drawn
				a.overlayKey(k, "")
			}
			if msg := c.check(a); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// fiveRows is a filterable list: filterMin rows or more.
func fiveRows() []hostapi.OverlayItem {
	return []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"}}
}

// TestOverlayArrowKeys: left and right alias h and l on a list, and an open
// filter keeps them for its cursor, as it moves and edits the query with
// them.
func TestOverlayArrowKeys(t *testing.T) {
	for _, c := range []struct {
		name  string
		o     hostapi.Overlay
		keys  []string
		check func(a *App) string
	}{
		{
			"right opens the selected row's view and left goes back",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Detail: "view"}}},
			[]string{"right", "left"},
			func(a *App) string {
				return fail(len(a.overlays) == 1 && !a.overlays[0].back, fmt.Sprintf("overlays=%d", len(a.overlays)))
			},
		},
		{
			"left pops a screen stacked over a list",
			hostapi.Overlay{ID: "p", Items: []hostapi.OverlayItem{{Text: "a"}}},
			nil,
			func(a *App) string {
				a.openOverlay(hostapi.Overlay{ID: "s", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "b"}}})
				a.overlayKey("left", "")
				return fail(len(a.overlays) == 1 && a.overlays[0].o.ID == "p", fmt.Sprintf("overlays=%d", len(a.overlays)))
			},
		},
		{
			"an open filter moves the cursor with left and right",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Detail: "view"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"}}},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("left", "")
				a.overlayKey("right", "")
				a.overlayKey("right", "") // clamps at the end, opens no view
				st := &a.overlays[0]
				return fail(st.cursor == 1 && st.query == "a" && len(a.overlays) == 1 && !st.back,
					fmt.Sprintf("cursor=%d query=%q overlays=%d back=%v", st.cursor, st.query, len(a.overlays), st.back))
			},
		},
		{
			"typing inserts at a moved cursor",
			hostapi.Overlay{Items: fiveRows()},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("c", "c")
				a.overlayKey("left", "")
				a.overlayKey("b", "b")
				st := &a.overlays[0]
				return fail(st.query == "abc" && st.cursor == 2, fmt.Sprintf("query=%q cursor=%d", st.query, st.cursor))
			},
		},
		{
			"backspace deletes before a moved cursor",
			hostapi.Overlay{Items: fiveRows()},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("b", "b")
				a.overlayKey("left", "")
				a.overlayKey("backspace", "")
				st := &a.overlays[0]
				return fail(st.query == "b" && st.cursor == 0, fmt.Sprintf("query=%q cursor=%d", st.query, st.cursor))
			},
		},
		{
			"delete removes after a moved cursor",
			hostapi.Overlay{Items: fiveRows()},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("b", "b")
				a.overlayKey("left", "")
				a.overlayKey("delete", "")
				st := &a.overlays[0]
				return fail(st.query == "a" && st.cursor == 1, fmt.Sprintf("query=%q cursor=%d", st.query, st.cursor))
			},
		},
		{
			"left clamps at the start and a kept query types on from the end",
			hostapi.Overlay{Items: fiveRows()},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("left", "")
				a.overlayKey("left", "") // clamps at 0
				a.overlayKey("enter", "")
				a.overlayKey("/", "/") // types on from the end, not where it left off
				a.overlayKey("b", "b")
				st := &a.overlays[0]
				return fail(st.query == "ab" && st.cursor == 2 && st.active, fmt.Sprintf("query=%q cursor=%d active=%v", st.query, st.cursor, st.active))
			},
		},
		{
			"a kept filter gives left and right back to the list",
			hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Detail: "view"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"}}},
			nil,
			func(a *App) string {
				a.overlayKey("/", "/")
				a.overlayKey("a", "a")
				a.overlayKey("enter", "")
				a.overlayKey("right", "") // aliases l: opens the row's view
				st := &a.overlays[0]
				return fail(len(a.overlays) == 2 && st.query == "a" && !st.active,
					fmt.Sprintf("overlays=%d query=%q active=%v", len(a.overlays), st.query, st.active))
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			c.o.Origin = lua.OriginBuiltin
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

// TestOverlayHints: the footer names only keys that act on the selection,
// with one verb per act of enter's in every list.
func TestOverlayHints(t *testing.T) {
	reload := func() []hostapi.OverlayItem { return nil }
	run := []hostapi.OverlayItem{{Text: "a", Run: "x"}}
	keys := []hostapi.OverlayKey{{Key: "e", Desc: "edit", Run: "edit"}}
	hints := func(o hostapi.Overlay, filter string) string {
		a := newTestApp(t, t.TempDir())
		o.Origin = lua.OriginBuiltin
		a.openOverlay(o)
		// "/" types, "=" is a kept query
		a.overlays[0].active, a.overlays[0].query = filter == "/", strings.TrimPrefix(filter, "=")
		box := a.overlayBox(&a.overlays[0], 80, 20, true)
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
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "inert"}}}, "", "y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Fill: "x"}}, Verb: "run"}, "", "enter fill   y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Detail: "x"}}, Verb: "pick"}, "", "enter open   y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Run: "x"}}, Origin: lua.OriginBuiltin, Verb: "pick"}, "", "enter pick   y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Run: "x", Verb: "open"}}, Verb: "pick", Reload: reload}, "", "enter open   y copy   esc close"},
		{hostapi.Overlay{Items: run, Verb: "copy"}, "", "enter copy   esc close"},
		{hostapi.Overlay{Items: run}, "", "enter run   y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a"}}, OnSelect: &glua.LFunction{}}, "", "enter pick   y copy   esc close"},
		{hostapi.Overlay{Items: run, Reload: reload}, "", "enter toggle   y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Run: "x", Detail: "d"}}, Reload: reload}, "", "enter open   space toggle   y copy   esc close"},
		{hostapi.Overlay{Items: run, Keys: keys}, "", "enter run   y copy   e edit   esc close"},
		{hostapi.Overlay{Keys: keys}, "", "e edit   esc close"},
		{hostapi.Overlay{Keys: []hostapi.OverlayKey{{Key: "r", Desc: "resume"}}}, "", "esc close"}, // a row's key waits for a row
		{hostapi.Overlay{Items: run, Keys: keys}, "/", "enter run   esc clear"},
		{hostapi.Overlay{Items: run, Keys: keys}, "=a", "enter run   y copy   e edit   esc clear"},
		{hostapi.Overlay{Follow: true, Items: run}, "", "enter run   y copy   esc close"},
		{hostapi.Overlay{Body: "text"}, "", "y copy   esc close"},
		{hostapi.Overlay{Items: []hostapi.OverlayItem{{Text: "a", Copy: "export X"}}}, "", "enter copy   esc close"},
		{hostapi.Overlay{Body: "text", Items: []hostapi.OverlayItem{{Text: "a", Fill: "x"}}}, "/", "enter fill   esc clear"},
		{hostapi.Overlay{Body: strings.Repeat("line\n", 10), Items: slices.Repeat([]hostapi.OverlayItem{{Text: "a", Fill: "x"}}, 30)}, "", "enter fill   y copy   esc close   ctrl+d/u scroll"},
	} {
		if got := strings.Trim(hints(c.o, c.filter), "│ "); got != c.want {
			t.Errorf("%v: %q, want %q", c.o.Items, got, c.want)
		}
	}
	// esc closes a browsing menu whole from any of its levels; h is the
	// one-level back, so the level's esc verb says close all
	a := newTestApp(t, t.TempDir())
	a.openOverlay(hostapi.Overlay{ID: "menu", Follow: true, Items: run})
	a.openOverlay(hostapi.Overlay{ID: "menu 1", Follow: true, Items: run})
	for _, i := range []int{0, 1} {
		box := a.overlayBox(&a.overlays[i], 80, 20, true)
		want := "esc close"
		if i == 1 {
			want = "esc close all"
		}
		if got := strings.Trim(hostapi.SegsText(box[len(box)-2]), "│ "); !strings.HasSuffix(got, want) {
			t.Errorf("menu level %d: %q, want %q", i, got, want)
		}
	}
}

// TestListAlias: a list = true binding makes its key the move it names
// wherever a list, popup or prompt has the keys, and nowhere else.
func TestListAlias(t *testing.T) {
	many := make([]hostapi.OverlayItem, 40)
	for i := range many {
		many[i] = hostapi.OverlayItem{Text: fmt.Sprintf("row %d", i)}
	}
	for _, c := range []struct {
		name  string
		setup func(a *App)
		key   string
		check func(a *App) string
	}{
		{"ctrl+j moves an overlay's selection", func(a *App) {
			a.openOverlay(hostapi.Overlay{ID: "o", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b"}}})
		}, "ctrl+j", func(a *App) string { return fail(a.overlays[0].selected == 1, "not moved") }},
		{"ctrl+j moves the popup's selection", func(a *App) {
			runLua(t, a, `zemi.command.add("aa", function() end) zemi.command.add("ab", function() end)`)
			pressKeys(a, "/a")
		}, "ctrl+j", func(a *App) string {
			return fail(a.completion.shown() && a.completion.selected == 1, "not moved")
		}},
		{"ctrl+j on the bare composer scrolls the chat", func(a *App) { pressKeys(a, "hi") }, "ctrl+j", func(a *App) string {
			return fail(a.composer.text == "hi", fmt.Sprintf("composer %q", a.composer.text))
		}},
		{"an alias of ctrl+d moves half a page", func(a *App) {
			a.openOverlay(hostapi.Overlay{ID: "o", Origin: lua.OriginBuiltin, Items: many})
			a.overlayBox(&a.overlays[0], 80, 20, true)
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
			t.Parallel()
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
		still               bool // false when the key is a no-op the list's fit causes
	}{
		{"ctrl+d", 20, 10, 0, 5, true},
		{"ctrl+u", 20, 10, 12, 7, true},
		{"ctrl+d", 20, 10, 17, 19, true},
		{"ctrl+u", 20, 10, 3, 0, true},
		{"ctrl+d", 20, 1, 0, 1, true},
		{"ctrl+d", 5, 8, 1, 1, false}, // a list the box shows whole: nothing to scroll
		{"pgdown", 5, 8, 1, 1, false},
		{"g", 20, 10, 7, 0, true},
		{"G", 20, 10, 3, 19, true},
	} {
		if sel := c.from; moveSel(&sel, c.n, c.page, c.key) != c.still || sel != c.want {
			t.Errorf("%s from %d over %d, page %d: %d, want %d", c.key, c.from, c.n, c.page, sel, c.want)
		}
	}
}

// TestOverlayRowsBuiltOnce: a frame that moved nothing builds no list rows,
// so an open picker costs the same at 10 items and 10000.
func TestOverlayRowsBuiltOnce(t *testing.T) {
	t.Parallel()
	app, st := overlayList(t)
	// draw paints the box at w and reports whether it rebuilt the list rows.
	draw := func(w int) bool {
		before := st.rows
		app.overlayBox(st, w, 20, true)
		return before == nil || &before[0] != &st.rows[0]
	}
	if !draw(80) {
		t.Fatal("the first frame built no rows")
	}
	if draw(80) {
		t.Fatal("a frame that moved nothing built the rows again")
	}
	st.selected = 30
	if !draw(80) {
		t.Fatal("a move built no rows")
	}
	st.set("item 4")
	if !draw(80) {
		t.Fatal("a filter built no rows")
	}
	if got := hostapi.SegsText(st.rows[0]); !strings.Contains(got, "item 04") {
		t.Errorf("filtered list starts with %q", got)
	}
}

// overlayList is an open picker over 50 items, and its state.
func overlayList(t *testing.T) (*App, *overlayState) {
	t.Helper()
	app, _ := bootAgentApp(t, 80, 20)
	var items []hostapi.OverlayItem
	for i := range 50 {
		items = append(items, hostapi.OverlayItem{Text: fmt.Sprintf("item %02d", i)})
	}
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: items})
	return app, &app.overlays[0]
}

// TestOverlayClosesOnOutsideClick: a click beside a menu closes it, a click
// inside does not, and a blocking ask ignores both.
func TestOverlayClosesOnOutsideClick(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "alpha"}, {Text: "beta"}}})
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

// TestOverlaySwitch: an item's on and locked fields draw the switch, the
// same width in every state, so a toggle never moves a row's text.
func TestOverlaySwitch(t *testing.T) {
	width := -1
	for _, c := range []struct {
		item   hostapi.OverlayItem
		sw, hl string
	}{
		{hostapi.OverlayItem{Text: "a", On: new(true)}, "◆ on ", "Host:Ok"},
		{hostapi.OverlayItem{Text: "a", On: new(false), Tag: "tag"}, "◇ off", "Host:Hint"},
		{hostapi.OverlayItem{Text: "a", Locked: new(true)}, "  on ", "Host:Hint"},
	} {
		its := (&App{}).overlayState(hostapi.Overlay{Items: []hostapi.OverlayItem{c.item}}).items
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

// TestOverlayTailLabel: a tail row too wide for the box keeps its end, with
// its desc beside or under it, at every width.
func TestOverlayTailLabel(t *testing.T) {
	t.Parallel()
	path := "~/some very/long/parent/directory/names/that/go/on/and/on/alpha"
	for _, w := range []int{40, 80, 120} {
		app, _ := bootAgentApp(t, w, 20)
		app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Items: []hostapi.OverlayItem{
			{Text: path, Tail: true, Desc: "undecided  1 session  5m ago", Tag: "current"},
		}})
		var text string
		for _, r := range app.overlayBox(&app.overlays[0], w, 20, true) {
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
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 16)
	runLua(t, app, `zemi.ui.overlay.open({ id = "b", markdown = true,
  body = { { { text = "red", hl = "Danger", click = "quit" }, " plain" }, { "two" } },
  items = { { text = "a", detail = "**bold**" } } })
assert(not pcall(zemi.ui.overlay.open, { id = "x", body = { 1 } }), "a body that is no list of lines must raise")`)
	st := &app.overlays[0]
	row := app.overlayBox(st, 60, 16, true)[1]
	if st.o.Body != "red plain\ntwo\n" || row[1].Text != "red" || row[1].HL != "Danger" || row[1].Click != "" {
		t.Fatalf("body %q, row %+v", st.o.Body, row)
	}
	pressKeys(app, "enter")
	if d := app.overlays[1]; !d.o.BodyMD || strings.Contains(hostapi.SegsText(app.overlayBox(&d, 60, 16, true)[1]), "*") {
		t.Fatalf("detail: %+v", d.o)
	}
}

// TestOverlayItemFields: every field of a Lua item reaches the parsed row,
// and one of the wrong type raises, a detail action's id included.
func TestOverlayItemFields(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 16)
	runLua(t, app, `zemi.ui.overlay.open({ id = "f", items = { { text = "t", desc = "d", tag = "k", section = "s", detail = "x", verb = "run",
  fill = "f", copy = "c", run = "r", id = "i", danger = true, tail = true, on = false, locked = true, keys = { { key = "a", desc = "A" } }, fields = { { "a", "b" } } } } })
for _, bad in ipairs({ { text = 1 }, { on = "yes" }, { keys = "x" }, { keys = { { id = 1 } } }, { keys = { { key = "j" } } }, { keys = { { key = "o", run = "a" }, { key = "o", run = "b" } } }, { keys = { { key = "a", verb = "nope" } } }, { fields = { "x" } }, { danger = "yes" }, { tail = 1 }, { verb = "nope" } }) do
  assert(not pcall(zemi.ui.overlay.open, { id = "x", items = { bad } }), "a field of the wrong type must raise")
end
	assert(pcall(zemi.ui.overlay.open, { id = "x", items = { { id = 1 } } }), "a list item's id is its own data")`)
	// the fields the row renderer and the key handling read, from the Lua above
	it := app.overlays[0].items[0].row
	if it.Text != "t" || it.Desc != "d" || it.Tag != "k" || it.Section != "s" || it.Detail != "x" ||
		it.Verb != hostapi.VerbRun || it.Fill != "f" || it.Copy != "c" || it.Run != "r" || it.ID != "i" ||
		!it.Danger || !it.Tail || it.On == nil || *it.On || it.Locked == nil || !*it.Locked || len(it.Keys) != 1 || it.Keys[0].Key != "a" || it.Keys[0].Desc != "A" ||
		fmt.Sprint(it.Fields) != "[{a b}]" {
		t.Fatalf("row %+v", it)
	}
}

// TestOverlayRowKeys: a row's own keys act on it from the list, one level
// in the footer of its view too; a danger one waits for a second press; and
// a screen a key opens leaves the list under it.
func TestOverlayRowKeys(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 16)
	ran := ""
	open := func(keys string) string {
		return `zemi.ui.overlay.open({ id = "k", items = { "a", { text = "b", detail = "b's view",
  keys = { { key = "d", desc = "Delete", id = "del", danger = true }, { key = "o", desc = "Open" } } } },
  on_action = function(item, key) zemi.notify({ body = item.text .. ":" .. key.id }) end,
  ` + keys + ` })`
	}
	const editKey = `keys = { e = { desc = "edit", fn = function(item) zemi.notify({ body = "e:" .. item.text }) end } }`
	runLua(t, app, open(editKey)+`
assert(not pcall(zemi.ui.overlay.open, { id = "x", items = { { keys = { { desc = "no key" } } } } }), "a key with no character must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", items = { { keys = { { key = "y" } } } } }), "a key the list takes itself must raise")
assert(not pcall(zemi.ui.overlay.open, { id = "x", items = { { keys = { { key = "ab" } } } } }), "a key of several characters must raise")`)
	footer := func() string {
		box := app.overlayBox(&app.overlays[len(app.overlays)-1], 90, 16, true)
		return hostapi.SegsText(box[len(box)-2])
	}
	for _, c := range []struct {
		name, keys, press string
		check             func() string
	}{
		{"the row's keys are named beside the list's", "", "j", func() string {
			ok := strings.Contains(footer(), "y copy   e edit   d Delete   o Open   esc close")
			return fail(ok, fmt.Sprintf("%q", footer()))
		}},
		{"a key without an id or a run does nothing", "", "j o", func() string {
			return fail(len(app.overlays) == 1, strconv.Itoa(len(app.overlays)))
		}},
		{"a row without keys ignores the key", "", "d", func() string {
			return fail(len(app.overlays) == 1 && lastToast(app) == "", fmt.Sprintf("%d open, toast %q", len(app.overlays), lastToast(app)))
		}},
		{"a danger key arms, and says so", editKey, "j d", func() string {
			ok := len(app.overlays) == 1 && app.overlays[0].keyArmed == "d" && strings.Contains(footer(), "d again to delete   esc cancel")
			return fail(ok, fmt.Sprintf("%d open, armed %q, footer %q", len(app.overlays), app.overlays[0].keyArmed, footer()))
		}},
		{"its second press runs it, with the key", "", "j d d", func() string {
			ok := lastToast(app) == "b:del" && len(app.overlays) == 0
			return fail(ok, fmt.Sprintf("toast %q, %d open", lastToast(app), len(app.overlays)))
		}},
		{"a list's own key wins over a row's", `keys = { d = { desc = "rename", fn = function(item) zemi.notify({ body = "key " .. item.text }) end } }`, "j d", func() string {
			return fail(lastToast(app) == "key b", fmt.Sprintf("toast %q", lastToast(app)))
		}},
	} {
		app.overlays = nil
		clear(app.lastSel)
		runLua(t, app, open(cmp.Or(c.keys, editKey)))
		pressKeys(app, strings.Fields(c.press)...)
		for app.Pump() {
		}
		if msg := c.check(); msg != "" {
			t.Errorf("%s: %s", c.name, msg)
		}
	}

	// a view holds what it says, lists nothing, and the row acts under it
	app.overlays = nil
	clear(app.lastSel)
	ran = ""
	for _, name := range []string{"res", "view"} {
		app.host.Commands.Add(&hostapi.Command{Name: name, NArgs: "?", GoFn: func(_ *hostapi.Host, args string, confirm bool) error {
			ran += name + " " + args
			if name == "view" {
				app.openOverlay(hostapi.Overlay{ID: "view " + args, Title: "View", Origin: lua.OriginBuiltin, Body: "text"})
			}
			return nil
		}})
	}
	app.openOverlay(hostapi.Overlay{ID: "s", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{
		{Text: "s1", Detail: "about s1", Keys: []hostapi.ItemKey{{Key: "v", Desc: "View", Run: "view 1", Verb: "open"}, {Key: "r", Desc: "Resume", Run: "res 1"}}},
	}})
	pressKeys(app, "enter")
	d := app.overlays[len(app.overlays)-1]
	if !d.back || len(d.visible) != 0 || d.o.Body != "about s1" || !strings.Contains(footer(), "esc back") {
		t.Fatalf("a view: back %v, %d rows, body %q, footer %q", d.back, len(d.visible), d.o.Body, footer())
	}
	// a key that opens a screen keeps the list under it
	pressKeys(app, "esc", "v")
	for app.Pump() {
	}
	if ran != "view 1" || len(app.overlays) != 2 || app.overlays[1].o.ID != "view 1" {
		t.Fatalf("a key opening a screen: ran %q, %d open", ran, len(app.overlays))
	}
	// a key that opens nothing closes the list
	pressKeys(app, "esc", "r")
	for app.Pump() {
	}
	if !strings.HasPrefix(ran, "view 1res 1") || len(app.overlays) != 0 {
		t.Fatalf("a key that closes: ran %q, %d open", ran, len(app.overlays))
	}

	// the view acts on its row's keys, and names them: i installs from a
	// plugin entry's detail
	app.overlays, ran = nil, ""
	app.openOverlay(hostapi.Overlay{ID: "s", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{
		{Text: "s1", Detail: "about s1", Keys: []hostapi.ItemKey{{Key: "r", Desc: "Resume", Run: "res 1"}}},
	}})
	pressKeys(app, "enter")
	if foot := footer(); !strings.Contains(foot, "r Resume") || !strings.Contains(foot, "esc back") {
		t.Errorf("a view's footer: %q, want the row's keys beside esc back", foot)
	}
	pressKeys(app, "r")
	for app.Pump() {
	}
	if ran != "res 1" || len(app.overlays) != 0 {
		t.Errorf("a key in the view: ran %q, %d open", ran, len(app.overlays))
	}

	// a row key the list keeps for itself is dropped, not named and never
	// run: an overlay built in Go skips the parse that refuses one, and the
	// list's move would swallow the key unseen
	app.overlays, ran = nil, ""
	app.openOverlay(hostapi.Overlay{
		ID: "s", Origin: lua.OriginBuiltin,
		Keys:  []hostapi.OverlayKey{{Key: "j", Desc: "jump", Run: "res 3"}},
		Items: []hostapi.OverlayItem{{Text: "s1", Keys: []hostapi.ItemKey{{Key: "k", Desc: "Kill", Danger: true, Run: "res 4"}, {Key: "x", Desc: "Stop", Danger: true, Run: "res 5"}}}},
	})
	if foot := footer(); strings.Contains(foot, "jump") || strings.Contains(foot, "Kill") {
		t.Errorf("a reserved key is named: %q", foot)
	}
	pressKeys(app, "j", "k", "k")
	for app.Pump() {
	}
	if ran != "" || len(app.overlays) != 1 {
		t.Fatalf("a reserved key ran: %q, %d open", ran, len(app.overlays))
	}
	pressKeys(app, "x", "x")
	for app.Pump() {
	}
	if ran != "res 5" || len(app.overlays) != 0 {
		t.Fatalf("a kept row key must run on its armed press: ran %q, %d open", ran, len(app.overlays))
	}

	// a destructive command typed without --yes selects its row and arms the
	// key that runs it
	app.overlays, ran = nil, ""
	app.openOverlay(hostapi.Overlay{ID: "s", Title: "Sessions", Origin: lua.OriginBuiltin, Confirm: "res 2", Items: []hostapi.OverlayItem{
		{Text: "current"}, {Text: "s2", Keys: []hostapi.ItemKey{{Key: "r", Desc: "Resume", Run: "res 2"}}},
	}})
	top := &app.overlays[len(app.overlays)-1]
	if !top.back && top.selected != 1 || top.keyArmed != "r" {
		t.Fatalf("typed resume: selected %d, armed %q", top.selected, top.keyArmed)
	}
	// the typed command was the first ask: the footer completes it, it never
	// coaches a second press of the key
	if foot := footer(); !strings.Contains(foot, "r to resume") || strings.Contains(foot, "again") {
		t.Fatalf("the typed confirm's footer: %q", foot)
	}
	pressKeys(app, "r")
	for app.Pump() {
	}
	if ran != "res 2" || len(app.overlays) != 0 {
		t.Fatalf("typed resume confirmed: ran %q, %d open", ran, len(app.overlays))
	}
	app.openOverlay(hostapi.Overlay{ID: "s", Title: "Sessions", Origin: lua.OriginBuiltin, Confirm: "del 9", Items: []hostapi.OverlayItem{{Text: "current"}}})
	if toast := lastToast(app); !strings.Contains(toast, "/del 9 runs it without asking") {
		t.Fatalf("no key for the typed run: toast %q", toast)
	}

	// a danger row still arms on enter, its label the confirmation
	app.overlays = nil
	app.host.Commands.Add(&hostapi.Command{Name: "del", NArgs: "?", GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = "del " + args; return nil }})
	ran = ""
	app.openOverlay(hostapi.Overlay{ID: "d", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "current"}, {Text: "wipe", Danger: true, Run: "del w"}}})
	pressKeys(app, "j", "enter")
	if top := &app.overlays[len(app.overlays)-1]; !top.armed || !strings.Contains(footer(), "enter again to wipe") {
		t.Fatalf("a danger row: armed %v, footer %q", top.armed, footer())
	}
	pressKeys(app, "enter")
	for app.Pump() {
	}
	if ran != "del w" {
		t.Fatalf("a danger row's second enter: ran %q", ran)
	}

	// the burst after a danger fire is the confirm's echo: a repeat of the
	// firing key eats once, any other key types
	ran = ""
	app.openOverlay(hostapi.Overlay{ID: "d", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "current"}, {Text: "wipe", Danger: true, Run: "del w"}}})
	pressKeys(app, "j", "enter", "enter", "enter", "x")
	for app.Pump() {
	}
	if ran != "del w" || app.composer.text != "x" {
		t.Fatalf("danger fire then burst: ran %q, composer %q", ran, app.composer.text)
	}

	// l arms a danger row like enter's first press does: the run stays
	// enter's to confirm, and l again leaves the arm alone
	ran = ""
	app.openOverlay(hostapi.Overlay{ID: "d", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "current"}, {Text: "wipe", Danger: true, Run: "del w"}}})
	pressKeys(app, "j", "l")
	if top := &app.overlays[len(app.overlays)-1]; !top.armed || ran != "" {
		t.Fatalf("l on a danger row: armed %v, ran %q", top.armed, ran)
	}
	pressKeys(app, "l", "enter")
	for app.Pump() {
	}
	if ran != "del w" {
		t.Fatalf("enter after l armed: ran %q", ran)
	}
}

// TestLuaOverlayReload: a Lua overlay with reload stays open on space and
// enter, runs on_select, then shows the items reload returns, rendered
// again, and keeps its items when reload fails.
func TestLuaOverlayReload(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestOverlayOwnership: the isolation rule the types promise - user config,
// builtin code or the origin that opened an overlay may close or replace it;
// of the plugins only the one that opened it, and a Go screen's overlay
// (Gen 0) is out of every plugin's reach. A refusal is named in /errors.
func TestOverlayOwnership(t *testing.T) {
	t.Parallel()
	pluginPath := func(host *hostapi.Host, name string) string {
		return filepath.Join(host.Dirs.PluginsRoot(), "local", name, ".zemi", "init.lua")
	}
	runAs := func(origin lua.Origin, name string) func(t *testing.T, app *App, host *hostapi.Host, src string) {
		return func(t *testing.T, app *App, host *hostapi.Host, src string) {
			t.Helper()
			if err := host.RT.ExecuteChunk(t.Context(), src, pluginPath(host, name), origin); err != nil {
				t.Fatal(err)
			}
			for app.Pump() {
			}
		}
	}
	alpha := runAs(lua.OriginPlugin, "alpha")
	beta := runAs(lua.OriginPlugin, "beta")
	user := runAs(lua.OriginUser, "alpha")
	openGo := func(t *testing.T, app *App, host *hostapi.Host) {
		app.openOverlay(hostapi.Overlay{ID: "g", Title: "Go", Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "go"}}})
	}
	cases := []struct {
		name    string
		open    func(t *testing.T, app *App, host *hostapi.Host)
		act     func(t *testing.T, app *App, host *hostapi.Host)
		text    string // the first row's text after act; "" once the act closed or replaced it
		refused bool   // the act must be refused, and named in /errors
	}{
		{
			"a plugin closes its own",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "o", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) { alpha(t, app, host, `zemi.ui.overlay.close("o")`) },
			"", false,
		},
		{
			"a plugin cannot close another plugin's",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "o", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) { beta(t, app, host, `zemi.ui.overlay.close("o")`) },
			"a", true,
		},
		{
			"a plugin cannot close a Go screen's",
			openGo,
			func(t *testing.T, app *App, host *hostapi.Host) { beta(t, app, host, `zemi.ui.overlay.close("g")`) },
			"go", true,
		},
		{
			"user config closes a plugin's",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "o", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) { user(t, app, host, `zemi.ui.overlay.close("o")`) },
			"", false,
		},
		{
			"user config closes a Go screen's",
			openGo,
			func(t *testing.T, app *App, host *hostapi.Host) { user(t, app, host, `zemi.ui.overlay.close("g")`) },
			"", false,
		},
		{
			"a plugin cannot replace another plugin's",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) {
				beta(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "b" } })`)
			},
			"a", true,
		},
		{
			"a plugin replaces its own",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "b" } })`)
			},
			"b", false,
		},
		{
			"a plugin cannot stand in for a Go screen's id",
			openGo,
			func(t *testing.T, app *App, host *hostapi.Host) {
				beta(t, app, host, `zemi.ui.overlay.open({ id = "g", items = { "p" } })`)
			},
			"go", true,
		},
		{
			"user config replaces a plugin's",
			func(t *testing.T, app *App, host *hostapi.Host) {
				alpha(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "a" } })`)
			},
			func(t *testing.T, app *App, host *hostapi.Host) {
				user(t, app, host, `zemi.ui.overlay.open({ id = "r", items = { "u" } })`)
			},
			"u", false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, 80, 24)
			c.open(t, app, host)
			if len(app.overlays) != 1 {
				t.Fatalf("open: %+v", app.overlays)
			}
			c.act(t, app, host)
			if c.text == "" {
				if len(app.overlays) != 0 {
					t.Fatalf("act did nothing: %+v", app.overlays)
				}
				return
			}
			if len(app.overlays) != 1 || app.overlays[0].o.Items[0].Text != c.text {
				t.Fatalf("act: %+v", app.overlays)
			}
			if c.refused && !host.HasUnseenErrors() {
				t.Fatal("a refusal left nothing in /errors")
			}
		})
	}
}

// TestListPreview: the selected row's preview shows under the list in a
// pane as tall as the longest preview, so moving or filtering never changes
// the box's height, and the filter matches preview words.
func TestListPreview(t *testing.T) {
	t.Parallel()
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
		rows := len(app.overlayBox(&app.overlays[0], 60, 30, true))
		pressKeys(app, c.keys...)
		box := app.overlayBox(&app.overlays[0], 60, 30, true)
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

// TestStepHoldsTheView: a step keeps the open view on screen while its run
// opens the next one and the two swap in one mutation, so the walk never
// paints the stack between two views.
func TestStepHoldsTheView(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	_, err := host.Commands.Add(&hostapi.Command{
		Name: "hop", NArgs: hostapi.NArgsAny, Hidden: true, Origin: lua.OriginBuiltin,
		GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			if args == "none" {
				return errors.New("no view to open")
			}
			if args == "two" && (len(app.overlays) != 1 || app.overlays[0].o.ID != "one") {
				t.Error("the step dropped the view before its own opened")
			}
			app.OpenOverlay(hostapi.Overlay{
				ID: args, Title: args, Origin: lua.OriginBuiltin,
				Prev: "hop one", Next: "hop two",
				Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b", Run: "x"}, {Text: "c"}},
			})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin, Next: "hop two"})
	pressKeys(app, "ctrl+j")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "two" })
	if len(app.overlays) != 1 || app.overlays[0].o.ID != "two" {
		t.Fatalf("the step left %d overlays", len(app.overlays))
	}

	// two presses before either lands walk both: the first landing becomes
	// the hold the second one closes, so none is left buried under the last
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin, Next: "hop two", Prev: "hop three"})
	pressKeys(app, "ctrl+j", "ctrl+k")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "three" })
	if n := len(app.overlays); n != 1 || app.overlays[0].o.ID != "three" {
		t.Fatalf("two steps left %d overlays", n)
	}

	// a walk whose view the reader dismissed still lands: the landing finds
	// nothing to close and the ended run leaves the hold inert, so what the
	// reader opens after the landing is never closed by it
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin, Next: "hop two"})
	pressKeys(app, "ctrl+j", "esc")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "two" })
	app.openOverlay(hostapi.Overlay{ID: "three", Title: "three", Origin: lua.OriginBuiltin})
	if n := len(app.overlays); n != 2 || app.overlays[0].o.ID != "two" || app.overlays[1].o.ID != "three" {
		t.Fatalf("the stale landing closed the view the reader opened: %d overlays", n)
	}

	// a landing on a view already open lower replaces it in place, and the
	// held view leaves with it: none is left buried under the landing
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin})
	app.openOverlay(hostapi.Overlay{ID: "two", Title: "two", Origin: lua.OriginBuiltin, Prev: "hop one"})
	pressKeys(app, "ctrl+k")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "one" })
	if len(app.overlays) != 1 || app.overlays[0].o.ID != "one" {
		t.Fatalf("the step onto an open view left %d overlays", len(app.overlays))
	}

	// a run that opens nothing drops the hold: the view stays, a newer step's
	// claim survives the failed settle, and its own landing swaps properly
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin, Prev: "hop none", Next: "hop two"})
	pressKeys(app, "ctrl+k")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "one" })
	pressKeys(app, "ctrl+j")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "two" })
	if n := len(app.overlays); n != 1 || app.overlays[0].o.ID != "two" {
		t.Fatalf("the step after a failed one left %d overlays", n)
	}
	// the held view's row survives the walk out and back: a view with rows
	// reopens on the row it held, not the first one
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{
		ID: "one", Title: "one", Origin: lua.OriginBuiltin, Next: "hop two",
		Items: []hostapi.OverlayItem{{Text: "a"}, {Text: "b", Run: "x"}, {Text: "c"}}, Selected: 1,
	})
	pressKeys(app, "ctrl+j")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "two" })
	pressKeys(app, "ctrl+k")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 && app.overlays[0].o.ID == "one" })
	if app.overlays[0].selected != 1 || app.overlays[0].o.Items[app.overlays[0].visible[app.overlays[0].selected]].Text != "b" {
		t.Fatalf("the walk back lost the row: selected %d", app.overlays[0].selected)
	}

	// a run that posted more than one view raced an open: the fail-safe
	// stacks them and closes nothing, so no view the reader holds is lost
	app.overlays = nil
	_, err = host.Commands.Add(&hostapi.Command{
		Name: "both", NArgs: hostapi.NArgsNone, Hidden: true, Origin: lua.OriginBuiltin,
		GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			app.OpenOverlay(hostapi.Overlay{ID: "x", Title: "x", Origin: lua.OriginBuiltin})
			app.OpenOverlay(hostapi.Overlay{ID: "y", Title: "y", Origin: lua.OriginBuiltin})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	app.openOverlay(hostapi.Overlay{ID: "one", Title: "one", Origin: lua.OriginBuiltin, Next: "both"})
	pressKeys(app, "ctrl+j")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 3 })
}

// TestBodyPartsWell: a part naming a surface carries its own padding row
// above and below, on that surface, so a command and what it answered join as
// two blocks of one, nothing between them but their own padding; a part
// outside keeps one blank row, and an empty one draws nothing at all.
func TestBodyPartsWell(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 20)
	rows := func(parts ...hostapi.BodyPart) []string {
		var got []string
		for _, row := range app.bodyParts(parts, 20) {
			var text strings.Builder
			fill := hostapi.FillAuto
			for _, sg := range row {
				fill = cmp.Or(fill, sg.Fill)
				text.WriteString(sg.Text)
			}
			got = append(got, fmt.Sprintf("%d:%s", fill, strings.TrimSpace(text.String())))
		}
		return got
	}
	row := func(fill hostapi.Fill, text string) string { return fmt.Sprintf("%d:%s", fill, text) }
	want := []string{
		row(hostapi.FillCanvas, ""), // the command's padding
		row(hostapi.FillCanvas, "go test ./..."),
		row(hostapi.FillCanvas, ""),
		row(hostapi.FillWell, ""), // the answer's, joined straight to it
		row(hostapi.FillWell, "ok"),
		row(hostapi.FillWell, ""),
		row(hostapi.FillAuto, ""), // the gap before a part outside
		row(hostapi.FillAuto, "done"),
	}
	if got := rows(
		hostapi.BodyPart{Text: "go test ./...", Lang: "bash", Fill: hostapi.FillCanvas},
		hostapi.BodyPart{Text: "ok", Fill: hostapi.FillWell},
		hostapi.BodyPart{Text: "done", Lang: "markdown"},
	); !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
	// a call that has answered nothing yet is its command alone
	if got := rows(
		hostapi.BodyPart{Text: "sleep 9", Lang: "bash", Fill: hostapi.FillCanvas},
		hostapi.BodyPart{Fill: hostapi.FillWell},
	); !slices.Equal(got, []string{
		row(hostapi.FillCanvas, ""), row(hostapi.FillCanvas, "sleep 9"), row(hostapi.FillCanvas, ""),
	}) {
		t.Fatalf("a call with no output yet: %q", got)
	}
	// a surface is the body's width whole: the text wraps inside its padding
	for _, row := range app.bodyParts([]hostapi.BodyPart{{Text: strings.Repeat("x ", 40), Fill: hostapi.FillWell}}, 20) {
		if w := hostapi.SegsWidth(row); len(row) > 0 && w != 20 {
			t.Fatalf("a row of the well is %d wide: %v", w, row)
		}
	}
}

// TestMemoriesDrillBack: l on a merged line opens its children as an overlay
// of their own, h climbs back to the view whole, esc closes the whole menu.
func TestMemoriesDrillBack(t *testing.T) {
	_, cfg := tempHome(t)
	app, host := bootAgentAppOn(t, 140, 40, _tipClock, cfg)
	if _, err := host.Commands.Add(&hostapi.Command{
		Name: "memories", NArgs: hostapi.NArgsAny,
		GoFn: func(h *hostapi.Host, args string, confirm bool) error { return h.MemoriesCommand(args) },
	}); err != nil {
		t.Fatal(err)
	}
	db := testStore(t, host)
	for i := range 6 {
		kind := store.KindAgent
		if i%2 == 0 {
			kind = store.KindUser
		}
		if _, err := db.AppendLog(kind, fmt.Sprintf("msg %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.InsertNode(1, 0, "msgs 0-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertNode(1, 1, "msgs 2-3"); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := db.InsertNode(0, int64(i), fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.MergeViewLines([]store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}}); err != nil || n != 2 {
		t.Fatalf("merges: %d, %v", n, err)
	}
	if err := host.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	rows := func() []string {
		var out []string
		for _, it := range app.overlays[len(app.overlays)-1].items {
			out = append(out, it.row.Text)
		}
		return out
	}
	if got, want := rows(), []string{"msgs 0-1", "msgs 2-3", "msg 4", "(not summarized yet: zoom it)"}; !slices.Equal(got, want) {
		t.Fatalf("root rows %q, want %q", got, want)
	}
	// l opens the first line's children as an overlay of their own, the
	// line's own text above them
	app.overlayKey("g", "")
	app.overlayKey("l", "")
	for app.Pump() {
	}
	if len(app.overlays) != 2 {
		t.Fatalf("%d overlays open after l, want the browser and the drill-in", len(app.overlays))
	}
	if got, want := rows(), []string{"msg 0", "msg 1"}; !slices.Equal(got, want) {
		t.Fatalf("drill-in rows %q, want %q", got, want)
	}
	st := &app.overlays[len(app.overlays)-1]
	if st.o.ID != "memories zoom 1 0" || st.o.Title != "Memories / 0+2" || st.o.Body != "msgs 0-1" {
		t.Fatalf("the drill-in = %s %q body %q", st.o.ID, st.o.Title, st.o.Body)
	}
	// h climbs back to the view whole
	app.overlayKey("h", "")
	for app.Pump() {
	}
	if len(app.overlays) != 1 {
		t.Fatalf("%d overlays open after h, want the browser alone", len(app.overlays))
	}
	if got, want := rows(), []string{"msgs 0-1", "msgs 2-3", "msg 4", "(not summarized yet: zoom it)"}; !slices.Equal(got, want) {
		t.Fatalf("root rows after h %q, want %q", got, want)
	}
	// esc closes the whole menu from a drill-in
	app.overlayKey("g", "")
	app.overlayKey("l", "")
	for app.Pump() {
	}
	app.overlayKey("esc", "")
	for app.Pump() {
	}
	if len(app.overlays) != 0 {
		t.Fatalf("%d overlays open after esc", len(app.overlays))
	}
}

// TestKeysPasteRekey: p on a stored key row opens the paste box over the
// list, and k only moves the list - k was the row's paste key until the
// list's move ate it first, leaving the footer's promise dead.
func TestKeysPasteRekey(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.host.Providers.Reset(agent.Catalog{
		Specs: []agent.Spec{
			{ID: "aa", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "AA_KEY", Models: []agent.Model{{ID: "m"}}},
			{ID: "bb", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "BB_KEY", Models: []agent.Model{{ID: "m"}}},
		},
		Keys: map[string]string{},
	})
	if err := app.host.Providers.ApplyStored("bb", "kp", true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.host.Commands.Add(&hostapi.Command{
		Name: "keys", NArgs: hostapi.NArgsAny,
		GoFn: func(h *hostapi.Host, args string, confirm bool) error { return h.KeysCommand(args) },
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.host.KeysCommand(""); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	st := &app.overlays[len(app.overlays)-1]
	if st.o.ID != "keys" {
		t.Fatalf("the screen = %s", st.o.ID)
	}
	app.overlayKey("end", "")
	if st.selected != 1 {
		t.Fatalf("the stored row selected %d", st.selected)
	}
	app.overlayKey("k", "")
	if st.selected != 0 || len(app.overlays) != 1 {
		t.Fatalf("k selected %d with %d overlays open", st.selected, len(app.overlays))
	}
	app.overlayKey("end", "")
	app.overlayKey("p", "p")
	for app.Pump() {
	}
	top := &app.overlays[len(app.overlays)-1]
	if len(app.overlays) != 2 || top.o.ID != "keys paste" || !top.o.Input || top.o.Title != "Paste the bb API key" {
		t.Fatalf("p opened %d overlays, top %s input=%v title %q", len(app.overlays), top.o.ID, top.o.Input, top.o.Title)
	}
}

// TestCancelMenuClosesTheWholeMenu: ctrl+c and a click outside cancel a
// browsing menu from any level or detail view, the way esc does.
func TestCancelMenuClosesTheWholeMenu(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.openOverlay(hostapi.Overlay{
		ID: "menu", Origin: lua.OriginBuiltin, Follow: true,
		Items: []hostapi.OverlayItem{{Text: "a", Anchor: 1}},
	})
	app.openOverlay(hostapi.Overlay{
		ID: "menu sub", Origin: lua.OriginBuiltin, Follow: true,
		Items: []hostapi.OverlayItem{{Text: "b", Anchor: 2, Detail: "the row whole"}},
	})
	app.openDetail(&app.overlays[len(app.overlays)-1], 0)
	if len(app.overlays) != 3 {
		t.Fatalf("%d overlays open before the cancel", len(app.overlays))
	}
	app.cancelMenu()
	if len(app.overlays) != 0 {
		t.Fatalf("%d overlays survived the cancel", len(app.overlays))
	}
}
