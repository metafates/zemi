package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
)

// findCell returns the screen cell where text first appears in the frame.
func findCell(t *testing.T, app *App, text string) (x, y int) {
	t.Helper()
	for i, l := range strings.Split(ansi.Strip(app.Frame()), "\n") {
		if at := strings.Index(l, text); at >= 0 {
			return ansi.StringWidth(l[:at]), i
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
	app, host := bootAgentApp(t, 80, 20)
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
	_ = host
}
