package ui

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// TestMessagesOpenKeepsTheTail: opening /messages shows the newest rows and
// must not scroll the chat behind it — the reader was at the tail, the picker
// opens there, and the head jumping in behind the popup reads as losing the
// place. A repeated reply text shares its markdown rows between messages, so
// an anchor stamped in place would move every copy and fan the jump out.
func TestMessagesOpenKeepsTheTail(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	dirs := config.Dirs{StateHome: resolved(t, t.TempDir()), DataHome: resolved(t, t.TempDir()), ProjectDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dirs.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	_, seed, err := store.OpenForDirs(dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	// the same reply text every turn: the shared markdown rows are what
	// scrambled the anchors
	for i := range 24 {
		if _, err := seed.AppendLog(store.KindUser, fmt.Sprintf("turn %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.AppendLog(store.KindAgent, "done, here is the reply.", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	_, cfg := tempHome(t)
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}, RuntimeFS: zemiruntime.FS, Clock: tipClock})
	host := hostapi.NewHost(rt, log, config.NewStore(), config.Dirs{ConfigHome: cfg, DataHome: dirs.DataHome, StateHome: dirs.StateHome, ProjectDir: dirs.ProjectDir})
	host.SetProjectTrusted(true)
	app := NewApp(host, log)
	host.SetUIHooks(app)
	app.CaptureHooks()
	if err := rt.Boot(t.Context(), []lua.BootStep{host.NamespacesStep()}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	app.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	app.renderSync(t)
	app.Frame()
	if st := app.scrolls[regionChat]; !st.follow {
		t.Fatalf("the chat should follow the tail before the open: top=%d rows=%d", st.top, st.rows)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.View() // the paint applies the jump the open asked for
	if st := app.scrolls[regionChat]; !st.follow {
		t.Fatalf("opening messages scrolled the chat behind it: top=%d rows=%d", st.top, st.rows)
	}
}
