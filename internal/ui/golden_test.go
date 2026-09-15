package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

// The M0 exit test (DESIGN §13): user init.lua rebinds a default key and
// adds a statusline segment; the composed frame is pinned as a golden.
func TestGoldenM0Frame(t *testing.T) {	ctx := context.Background()
	cfg, data, state, project := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	// Classify on EvalSymlinks-resolved prefixes (DESIGN §5): on macOS
	// t.TempDir() hands out /var/... while resolved paths are /private/var/...
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cfg, data, state = resolve(cfg), resolve(data), resolve(state)
	projectResolved := resolve(project)

	initLua := `
zemi.keymap.bind("normal", "tab", function()
  zemi.notify({ title = "rebound", body = "tab pressed" })
end, { desc = "notify on rebound tab" })

zemi.ui.widget.register({
  id = "user.status",
  region = "statusline.left",
  priority = 90,
  render = function(ctx)
    return { { { text = " [rebound]", hl = "Comment" } } }
  end,
})
`
	if err := os.WriteFile(filepath.Join(cfg, "init.lua"), []byte(initLua), 0o600); err != nil {
		t.Fatal(err)
	}

	dirs := config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state, ProjectDir: "/tmp/zemi-golden-project"}
	log := logging.New("")
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "site", "pack"), Origin: lua.OriginPlugin},
		{Prefix: filepath.Join(projectResolved, ".zemi"), Origin: lua.OriginProject},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiRuntime.FS})
	host := hostapi.NewHost(rt, log, settings, dirs)
	app := NewApp(host, log)
	host.SetUIHooks(app)

	steps := []lua.BootStep{
		host.NamespacesStep(),
		lua.RequireStep("zemi.defaultui"),
		lua.LoadFileStep("init", filepath.Join(cfg, "init.lua"), lua.OriginUser),
	}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}

	app.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // insert -> normal
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})    // rebound key fires the toast

	app.timeStr = "12:34" // pin the clock (host-provided UI state, DESIGN §8)
	app.renderSync(t)

	golden.RequireEqual(t, app.Frame())
}

// renderSync renders every visible widget synchronously on the caller's
// goroutine (the frame loop is not running in tests) and fills the caches.
func (a *App) renderSync(tb testing.TB) {
	gen := a.frameGen
	for _, w := range a.visibleWidgets() {
		width := a.widgetWidth(w)
		lines, err := a.host.RenderWidget(context.Background(), w, width, a.uiState())
		if err != nil {
			tb.Fatalf("render widget %s: %v", w.ID, err)
		}
		a.caches[w.ID] = &widgetCache{lines: lines, width: width, gen: gen}
	}
}

// The composer with an open @-completion popup and a two-line draft,
// pinned as a golden (files injected; no filesystem walk).
func TestGoldenComposerCompletion(t *testing.T) {
	ctx := context.Background()
	cfg, data, state := t.TempDir(), t.TempDir(), t.TempDir()
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cfg, data, state = resolve(cfg), resolve(data), resolve(state)

	log := logging.New("")
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "site", "pack"), Origin: lua.OriginPlugin},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiRuntime.FS})
	host := hostapi.NewHost(rt, log, settings, config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state})
	app := NewApp(host, log)
	host.SetUIHooks(app)

	steps := []lua.BootStep{host.NamespacesStep(), lua.RequireStep("zemi.defaultui")}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}

	app.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	app.completion.files = []string{"internal/ui/app.go", "main.go"}
	app.completion.scanning = true
	for _, k := range []string{"s", "e", "e", " ", "@", "a", "p"} {
		app.Update(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	}

	app.timeStr = "09:41"
	app.renderSync(t)

	golden.RequireEqual(t, app.Frame())
}
