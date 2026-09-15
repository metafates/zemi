package ui

import (
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
