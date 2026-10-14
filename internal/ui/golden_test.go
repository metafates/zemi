package ui

import (
	"fmt"
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
	zemiruntime "github.com/metafates/zemi/runtime"
)

// tempConfig is a config dir under a fresh, resolved $HOME, so the splash
// names ~/.config/zemi/init.lua on every machine. Setenv rules out
// t.Parallel in the tests that boot an app.
func tempConfig(t testing.TB) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".config", "zemi")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// User init.lua rebinds a default key and adds a statusline segment; the
// composed frame is pinned as a golden.
func TestGoldenFrame(t *testing.T) {
	ctx := t.Context()
	cfg, data, state := tempConfig(t), t.TempDir(), t.TempDir()
	cfg, data, state = resolved(t, cfg), resolved(t, data), resolved(t, state)

	initLua := `
zemi.keymap.set("ctrl+t", function()
  zemi.notify({ title = "rebound", body = "ctrl+t pressed" })
end, { desc = "notify on rebound ctrl+t" })

zemi.ui.widget.add({
  id = "user.status",
  region = "statusline.left",
  priority = 90,
  render = function(ctx)
    return { { { text = "[rebound]", hl = "Dim" } } }
  end,
})
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
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiruntime.FS})
	host := hostapi.NewHost(rt, log, settings, dirs)
	app := NewApp(host, log)
	host.SetUIHooks(app)

	steps := []lua.BootStep{
		host.NamespacesStep(),
		lua.RequireStep("zemi.tui"),
		lua.LoadFileStep("init", filepath.Join(cfg, "init.lua"), lua.OriginUser),
	}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}

	app.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	app.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}) // rebound insert-mode key fires the toast

	app.timeStr = "12:34" // pin the clock (host-provided UI state, DESIGN §8)
	app.renderSync(t)

	golden.RequireEqual(t, app.Frame())
}

// renderSync renders every visible widget synchronously on the caller's
// goroutine (the frame loop is not running in tests) and fills the caches.
func (a *App) renderSync(tb testing.TB) {
	gen := a.frameGen
	for _, w := range a.shownWidgets() {
		for more := true; more; {
			var lines [][]hostapi.Segment
			var err error
			if lines, more, err = a.host.RenderWidget(tb.Context(), w.Widget, w.width, w.height, a.uiState()); err != nil {
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

// The composer with an open @-completion popup and a two-line draft,
// pinned as a golden (files injected; no filesystem walk).
func TestGoldenComposerCompletion(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 12)
	app.completion.setFiles([]string{"internal/ui/app.go", "main.go"})
	for _, k := range []string{"s", "e", "e", " ", "@", "a", "p"} {
		app.Update(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	}

	app.timeStr = "09:41"
	app.renderSync(t)

	golden.RequireEqual(t, app.Frame())
}

// TestGoldenComposerFade pins a draft taller than the composer's window
// (§8): the rows past either edge sink toward the panel, deepest at the edge,
// while the cursor row stays clear of the ramp with a row of headroom below
// it.
func TestGoldenComposerFade(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 20)
	var draft strings.Builder
	for i := range 14 {
		fmt.Fprintf(&draft, "draft line %d\n", i+1)
	}
	app.composer.text = strings.TrimSuffix(draft.String(), "\n")
	app.composer.cursor = strings.Index(app.composer.text, "draft line 9")

	app.timeStr = "09:41"
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}

// TestGoldenReasoningCollapse pins the persisted thinking block (§8): a
// thought alone keeps its rail row between the user message and the reply,
// its text one click away.
func TestGoldenReasoningCollapse(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)

	host.AppendMessage("user", "why does the build fail?")
	appendReasoning(host, "checked go.mod, then the fixture dir", hostapi.TurnStat{ThoughtMS: 4200, ElapsedMS: 38000})
	appendReply(host, "The test binary exits 1 because the fixture path moved.", hostapi.TurnStat{
		In: 12340, Out: 1234, ThoughtMS: 4200, ElapsedMS: 38000, Ended: 1_700_000_000,
	})

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}

// TestGoldenSlashPalette pins the slash-command popup (§6.6): typing "/"
// as the first character lists the registered commands, filtered as you
// type, with the first match highlighted.
func TestGoldenSlashPalette(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
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

// TestPopupKeepsSize: both composer popups keep their title, size and hint
// row while a query narrows them down to no match, where the hint names no
// key that picks.
func TestPopupKeepsSize(t *testing.T) {
	for _, c := range []struct{ open, narrow, title string }{{"/mo", "zz", "Commands"}, {"@ap", "zzzq", "Files"}} {
		app, host := bootAgentApp(t, 80, 14)
		host.Commands.Add(&hostapi.Command{Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1)})
		app.completion.setFiles([]string{"internal/ui/app.go", "main.go"})
		popup := func(keys string) string {
			for _, k := range keys {
				app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
			}
			app.renderSync(t)
			f := ansi.Strip(app.Frame())
			return f[strings.Index(f, "┌"):strings.Index(f, "┘")]
		}
		want := popup(c.open)
		if got := popup(c.narrow); !strings.Contains(got, "No matches.") || !strings.Contains(got, "esc close") || strings.Contains(got, "complete") || strings.Contains(got, "pick") ||
			!strings.Contains(got, "┌ "+c.title) ||
			strings.Count(got, "\n") != strings.Count(want, "\n") || strings.Index(got, "┐") != strings.Index(want, "┐") {
			t.Fatalf("%s%s:\n%s\nwant the size of\n%s", c.open, c.narrow, got, want)
		}
	}
}

// TestGoldenChatScroll pins the transcript scrollback (§8): pgup moves the
// viewport off the tail to earlier lines, which stay anchored while the
// tail keeps filling underneath.
func TestGoldenChatScroll(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame() // establish the viewport metrics

	for app.Pump() {
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	for app.Pump() {
	}

	app.timeStr = "12:34"
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}
