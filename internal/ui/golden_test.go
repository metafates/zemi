package ui

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/runtime"
)

// tempHome lays a fresh, resolved home with an empty .config/zemi. Tests
// that pin rendered ~ paths point $HOME at home, which rules out
// t.Parallel for them.
func tempHome(t testing.TB) (home, cfg string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(home, ".config", "zemi")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

// User init.lua rebinds a default key and adds a statusline segment; the
// composed frame is pinned as a golden.
func TestGoldenFrame(t *testing.T) {
	ctx := t.Context()
	home, cfg := tempHome(t)
	t.Setenv("HOME", home)
	data, state := resolved(t, t.TempDir()), resolved(t, t.TempDir())

	initLua := `
zemi.keymap.set("ctrl+t", function()
  zemi.notify({ title = "rebound", body = "ctrl+t pressed" })
end, { desc = "notify on rebound ctrl+t" })
`
	if err := os.WriteFile(filepath.Join(cfg, "init.lua"), []byte(initLua), 0o600); err != nil {
		t.Fatal(err)
	}

	dirs := config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state, ProjectDir: "/tmp/zemi-golden-project"}
	log := slog.New(slog.DiscardHandler)
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "plugins"), Origin: lua.OriginPlugin},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: runtime.FS})
	host := hostapi.NewHost(rt, log, settings, dirs)
	t.Cleanup(host.Close)
	app := NewApp(host, log)
	host.SetUIHooks(app)

	steps := []lua.BootStep{
		host.NamespacesStep(),
		lua.LoadFileStep("init", filepath.Join(cfg, "init.lua"), lua.OriginUser),
	}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}

	// a plugin's own statusline segment, as the golden pins it
	addWidget(app, &widget{id: "user.status", region: _regionStatusLeft, priority: 90, render: func(*hostapi.ScreenState) ([][]hostapi.Segment, bool) {
		return [][]hostapi.Segment{{{Text: "[rebound]", HL: "Dim"}}}, false
	}})
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	app.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}) // rebound insert-mode key fires the toast

	app.timeStr = "12:34" // pin the clock (host-provided UI state, DESIGN §8)
	app.renderSync(t)
	app.View()        // the toast's rise starts
	app.settleRamps() // land it, the golden pins the settled frame

	golden.RequireEqual(t, app.Frame())
}

// renderSync renders every visible widget synchronously on the caller's
// goroutine (the frame loop is not running in tests) and fills the caches.
func (a *App) renderSync(tb testing.TB) {
	// the frame draws settled: a pinned clock would freeze a pill's fade
	a.settleRamps()
	gen := a.frameGen
	a.renderShown(tb, gen)
	// a pinned clock freezes a bar's or the splash's boot rise mid-ramp:
	// settle what the first paint set rising, then redraw
	if a.settleBars() || a.settleRamps() {
		a.renderShown(tb, gen)
	}
}

func (a *App) renderShown(tb testing.TB, gen int64) {
	for _, w := range a.shownWidgets() {
		for more := true; more; {
			var lines [][]hostapi.Segment
			var err error
			state := a.uiState()
			if lines, more, err = a.renderWidget(w, a.host.ScreenState(w.width, w.height, state)); err != nil {
				tb.Fatalf("render widget %s: %v", w.ID, err)
			}
			a.onWidgetRendered(widgetRender{id: w.ID, lines: lines, more: more, width: w.width, height: w.height, gen: gen})
		}
	}
}

// CaptureHooks redirects hook dispatch into a queue drained by Pump
// instead of inline processing. For tests that answer hooks from other
// goroutines: with capture on, all state mutation happens on the single
// goroutine that calls Pump, so no locks are needed.
func (a *App) CaptureHooks() {
	if a.testQueue == nil {
		a.testQueue = make(chan tea.Msg, 64)
	}
}

// stopWork ends the background worker a fake program started: a real one
// runs for the life of the app.
func (a *App) stopWork() {
	if a.work != nil {
		close(a.work)
	}
}

// Pump delivers one queued hook message; it reports whether one was
// processed. Call from the owning goroutine until it returns false.
func (a *App) Pump() bool {
	if a.testQueue == nil {
		return false
	}
	select {
	case msg := <-a.testQueue:
		a.Update(msg)
		return true
	default:
		return false
	}
}

// TestGoldenSlashPalette pins the slash-command popup (§6.6): typing "/"
// as the first character lists the registered commands, filtered as you
// type, with the first match highlighted.
func TestGoldenSlashPalette(t *testing.T) {
	app, host := bootAgentAppPinned(t, 80, 14)
	for _, c := range []*hostapi.Command{
		{Name: "help", Desc: "show keys and commands"},
		{Name: "model", Desc: "choose the model"},
		{Name: "memory", Desc: "review, forget, or supersede memories"},
	} {
		if _, err := host.Commands.Add(&hostapi.Command{
			Name: c.Name, Desc: c.Desc, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, k := range []string{"/", "m", "o"} {
		app.Update(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	}

	app.timeStr = "09:41"
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}

// TestPopupKeepsSize: the @ popup keeps its title, size and hint row while
// a query narrows it to no match, where the hint names no key that picks;
// a slash draft naming no command closes its popup instead.
func TestPopupKeepsSize(t *testing.T) {
	typeKeys := func(app *App, keys string) {
		for _, k := range keys {
			app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
		}
		app.renderSync(t)
	}
	popup := func(app *App) string {
		f := ansi.Strip(app.Frame())
		return f[strings.Index(f, "┌"):strings.Index(f, "┘")]
	}
	t.Run("attach keeps its empty list", func(t *testing.T) {
		app, _ := bootAgentApp(t, 80, 14)
		app.completion.setFiles([]string{"internal/ui/app.go", "main.go"})
		typeKeys(app, "@ap")
		want := popup(app)
		typeKeys(app, "zzzq")
		got := popup(app)
		if !strings.Contains(got, "No matches.") || !strings.Contains(got, "esc close") || strings.Contains(got, "pick") ||
			!strings.Contains(got, "┌ Attach") ||
			strings.Count(got, "\n") != strings.Count(want, "\n") || strings.Index(got, "┐") != strings.Index(want, "┐") {
			t.Fatalf("@apzzzq:\n%s\nwant the size of\n%s", got, want)
		}
	})
	t.Run("a slash draft naming no command closes the popup", func(t *testing.T) {
		app, host := bootAgentApp(t, 80, 14)
		host.Commands.Add(&hostapi.Command{Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1)})
		typeKeys(app, "/mo")
		if !app.completion.shown() {
			t.Fatal("/mo: popup closed")
		}
		typeKeys(app, "zzzq")
		if app.completion.shown() {
			t.Fatal("/mozzzq: popup still drawn")
		}
	})
}
