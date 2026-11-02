package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/store"
)

// TestSplashTakesBootWork: the pending row of a boot's MCP starts is drawn in
// the splash's own block, which the chat region still fits at every size, so
// nothing scrolls; where the logo has room, it keeps its place as the work
// comes and goes.
func TestSplashTakesBootWork(t *testing.T) {
	for _, c := range []struct {
		w, h int
		logo bool // a chat with room for the logo and the pending row
	}{
		{80, 24, true},
		{40, 9, false},
	} {
		t.Run(fmt.Sprintf("%dx%d", c.w, c.h), func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, c.w, c.h)
			app.screen(t)
			x, y := -1, -1
			if c.logo {
				x, y = findCell(t, app, "+#%#@%@%%#%#:")
			}
			stop := host.Busy("Starting mcp_filesystem", nil)
			stop2 := host.Busy("Starting mcp_github", nil)
			app.frameGen++
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			i := slices.IndexFunc(app.shownWidgets(), func(w shownWidget) bool { return w.ID == "splash" })
			if i < 0 {
				t.Fatal("no splash is shown")
			}
			w := app.shownWidgets()[i]
			if rows := app.regionLines(regionChat, w.width, w.height); len(rows) > w.height {
				t.Fatalf("the chat's widgets draw %d rows into %d:\n%s", len(rows), w.height, frame)
			}
			if !c.logo {
				return
			}
			if !strings.Contains(frame, "starting mcp_filesystem, and 1 more") {
				t.Fatalf("the boot work is not drawn collapsed in the chat:\n%s", frame)
			}
			if nx, ny := findCell(t, app, "+#%#@%@%%#%#:"); nx != x || ny != y {
				t.Fatalf("the logo moved from %d,%d to %d,%d", x, y, nx, ny)
			}
			stop()
			stop2()
			app.frameGen++
			app.renderSync(t)
			if nx, ny := findCell(t, app, "+#%#@%@%%#%#:"); nx != x || ny != y {
				t.Fatalf("the logo moved back to %d,%d from %d,%d", nx, ny, x, y)
			}
		})
	}
}

// TestSplashProjects: with another project known the splash lists it where
// recent sessions go, and a click on its path switches there.
func TestSplashProjects(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	root := t.TempDir()
	g, p, err := store.OpenForDirs(host.Dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	p.Close()
	app.frameGen++
	app.renderSync(t)
	// a long path keeps its tail
	x, y := findCell(t, app, root[len(root)-20:])
	if seg := dropCells(app.lastRows[y], x)[0]; seg.Click != "projects switch "+root || seg.Widget != "splash" {
		t.Fatalf("the path's segment: %+v", seg)
	}
}

// TestSplashWidePath: a project path of wide characters keeps its tail: the
// cut counts cells, not runes, so the name at the path's end is what is left.
func TestSplashWidePath(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	const name = "プロジェクトのディレクトリ"
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	g, p, err := store.OpenForDirs(host.Dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	p.Close()
	app.frameGen++
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, name) {
		t.Fatalf("the path's name is cut away:\n%s", frame)
	}
}
