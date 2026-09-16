package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// findCell returns the screen cell where text first appears in the frame.
func findCell(t *testing.T, app *App, text string) (x, y int) {
	t.Helper()
	for i, l := range strings.Split(ansi.Strip(app.Frame()), "\n") {
		if before, _, ok := strings.Cut(l, text); ok {
			return ansi.StringWidth(before), i
		}
	}
	t.Fatalf("%q not on screen:\n%s", text, ansi.Strip(app.Frame()))
	return 0, 0
}

func clickAt(app *App, x, y int) {
	app.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func TestMouseDragCopies(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.AppendMessage("user", "hello there, world")
	app.renderSync(t)
	x, y := findCell(t, app, "hello there")
	app.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseMotionMsg{X: x + 10, Y: y, Button: tea.MouseLeft})
	if _, cmd := app.Update(tea.MouseReleaseMsg{X: x + 10, Y: y, Button: tea.MouseLeft}); cmd == nil {
		t.Fatal("release after a drag should copy")
	}
	if got := app.selectionText(); got != "hello there" {
		t.Fatalf("selection = %q", got)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.selectionText() != "" {
		t.Fatal("a key should clear the selection")
	}
}

func TestMouseClicks(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	host.AppendMessage("user", "why?")
	host.AppendReasoning("first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	app.renderSync(t)
	x, y := findCell(t, app, "Thought for")
	clickAt(app, x+1, y)
	app.renderSync(t)
	if !strings.Contains(ansi.Strip(app.Frame()), "│ first idea") {
		t.Fatal("clicking the thought line should expand it")
	}

	done := make(chan bool)
	go func() {
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	x, y = findCell(t, app, " No ")
	clickAt(app, x+1, y)
	if <-done {
		t.Fatal("clicking No allowed the command")
	}
}

// TestMouseHover: hovering a button lights it, moving off clears it, and
// hovering a palette row then clicking it fills the composer.
func TestMouseHover(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan bool)
	go func() {
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	x, y := findCell(t, app, " No ")
	app.Update(tea.MouseMotionMsg{X: x + 1, Y: y})
	app.renderSync(t)
	if !strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: " No ", HL: "Host:BtnHover"}}, 80)) {
		t.Fatal("hovered button not lit")
	}
	app.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	app.renderSync(t)
	if strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: " No ", HL: "Host:BtnHover"}}, 80)) {
		t.Fatal("hover stayed after the pointer left")
	}
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	<-done

	if _, err := app.host.Commands.Add(&hostapi.Command{Name: "help", Desc: "show keys", GoFn: func(*hostapi.Host, string) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	app.renderSync(t)
	x, y = findCell(t, app, "/help")
	clickAt(app, x+1, y) // select
	clickAt(app, x+1, y) // open
	if app.composer.text != "/help " {
		t.Fatalf("composer = %q", app.composer.text)
	}
}

func TestWordSpan(t *testing.T) {
	for _, c := range []struct {
		text     string
		x        int
		from, to int
	}{
		{" manual  plan ", 3, 1, 7},
		{" manual  plan ", 7, 0, 0},
		{"✻ auto", 3, 2, 6},
	} {
		if from, to := wordSpan(c.text, c.x); from != c.from || to != c.to {
			t.Errorf("wordSpan(%q, %d) = %d, %d", c.text, c.x, from, to)
		}
	}
}

// TestDropPaste: pasted paths in the forms terminals use for drag and drop
// become attachments; ordinary text stays text.
func TestDropPaste(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	dir := t.TempDir()
	shot := filepath.Join(dir, "My Shot.png")
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(shot, []byte("\x89PNGfake"), 0o600)
	os.WriteFile(notes, []byte("hello"), 0o600)
	for _, c := range []struct{ paste, want string }{
		{strings.ReplaceAll(shot, " ", `\ `) + " " + notes, "@drop:My-Shot.png @drop:notes.txt "},
		{"'" + shot + "'", "@drop:My-Shot.png "},
		{"file://" + strings.ReplaceAll(shot, " ", "%20") + "\n", "@drop:My-Shot.png "},
		{"see " + notes, "see " + notes},
		{"/no/such/file.png", "/no/such/file.png"},
	} {
		app.composer.text, app.composer.cursor = "", 0
		app.Update(tea.PasteMsg{Content: c.paste})
		if app.composer.text != c.want {
			t.Errorf("paste %q: composer %q, want %q", c.paste, app.composer.text, c.want)
		}
	}
}

func TestComposerCursorColumn(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 12)
	app.composer.text, app.composer.cursor = "hello world", 2
	if segs := app.composerRows(60)[0]; segs[1].Text != "he" || segs[2].Text != "l" || segs[2].HL != "Host:Cursor" {
		t.Fatalf("block cursor not on its column: %+v", segs)
	}
}

// TestPopupClickOverWindow: a completion row drawn over a split window
// takes the click, not the window under it.
func TestPopupClickOverWindow(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	if err := host.RT.ExecuteChunk(context.Background(), `zemi.win.open({ cmd = "sleep 2", kind = "split" })`, "w.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	// the window exits on its own: closing it races inside vt's Emulator
	app.completion.files = []string{"a.go", "b.go", "c.go"}
	app.Update(tea.KeyPressMsg{Code: '@', Text: "@"})
	app.renderSync(t)
	x, y := findCell(t, app, "b.go")
	clickAt(app, x+1, y)
	if app.focusWin != 0 || app.composer.text != "b.go " {
		t.Fatalf("focusWin=%d composer=%q", app.focusWin, app.composer.text)
	}
}

func TestFilterKeys(t *testing.T) {
	for _, c := range []struct{ query, key, want string }{
		{"foo bar", "ctrl+w", "foo "},
		{"foo bar", "alt+backspace", "foo "},
		{"foo bar", "ctrl+u", ""},
		{"foo", "backspace", "fo"},
	} {
		f := listFilter{active: true, query: c.query}
		if !f.edit(c.key, "", []string{"a"}) || f.query != c.want {
			t.Errorf("%q %s -> %q, want %q", c.query, c.key, f.query, c.want)
		}
	}
}
