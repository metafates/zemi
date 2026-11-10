package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestCompletionSources: a Lua source fills a "#" popup and accepts into
// the draft, one for "@" goes ahead of the files, one for a command adds
// argument candidates, and a trigger with nothing to offer leaves enter to
// the composer.
func TestCompletionSources(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 14)
	runLua(t, app, `
zemi.composer.add({ id = "issues", trigger = "#", items = function(lead)
  if lead == "none" then return {} end
  return { { text = "12", desc = "fix focus " .. lead }, "40" }
end })
zemi.composer.add({ id = "people", trigger = "@", items = function() return { "alice" } end })
zemi.command.add("greet", function() end, { nargs = "?" })
zemi.composer.add({ id = "greet", command = "greet", items = function() return { "bob" } end })
local ok, err = pcall(zemi.composer.add, { id = "x", trigger = "ab", items = function() end })
assert(not ok and tostring(err):find("one punctuation"), tostring(err))`)

	pressKeys(app, "see #1")
	for app.Pump() {
	}
	if !app.completion.shown() || app.completion.count() != 2 || !strings.Contains(ansi.Strip(app.Frame()), "fix focus 1") {
		t.Fatalf("# popup: %+v\n%s", app.completion, ansi.Strip(app.Frame()))
	}
	pressKeys(app, "enter")
	if app.composer.text != "see #12 " {
		t.Fatalf("accepted %q", app.composer.text)
	}

	app.clearComposer()
	pressKeys(app, "#")
	for app.Pump() {
	}
	pressKeys(app, "4") // before the sources answer again
	if f := app.completion; len(f.extra) != 1 || f.extra[0].label != "40" {
		t.Fatalf("stale rows should narrow to the typed prefix: %v", f.extra)
	}

	app.clearComposer()
	app.completion.setFiles([]string{"main.go"})
	pressKeys(app, "@")
	for app.Pump() {
	}
	// the source's rows lead the popup, ahead of the sections
	if f := app.completion; len(f.extra) != 1 || f.extra[0].label != "alice" {
		t.Fatalf("@ popup: extra %v, files %v", f.extra, f.matches)
	}
	if i, j := strings.Index(ansi.Strip(app.Frame()), "alice"), strings.Index(ansi.Strip(app.Frame()), "main.go"); i < 0 || j < 0 || i > j {
		t.Fatalf("alice must go ahead of the files:\n%s", ansi.Strip(app.Frame()))
	}

	app.clearComposer()
	pressKeys(app, "/greet ")
	for app.Pump() {
	}
	if f := app.completion; !f.shown() || len(f.argRows) != 1 || f.argRows[0].label != "bob" {
		t.Fatalf("argument candidates: %+v", f.argRows)
	}

	app.clearComposer()
	pressKeys(app, "#none")
	for app.Pump() {
	}
	if app.completion.shown() {
		t.Fatal("an empty # popup should not show")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if app.composer.text != "#none" {
		t.Fatalf("tab changed the draft: %q", app.composer.text)
	}
}
