package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/lua"
)

// TestComposerActions: the composer's history and insert run from Lua and
// from keymap strings, and only user config may bind them.
// TestComposerRecallPastMultiline: an up arrow recalls history even when a
// recalled message is multiline - once the cursor reaches the top line (or
// the text is a single line), the next up keeps iterating instead of being
// trapped inside the recalled text.
func TestComposerRecallPastMultiline(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 12)
	app.composer.pushHistory("old", 100)
	app.composer.pushHistory("a\nb", 100)
	up := tea.KeyPressMsg{Code: tea.KeyUp}
	app.composerKey("up", up) // empty composer: recall the newest
	if app.composer.text != "a\nb" {
		t.Fatalf("first up = %q, want the newest multiline recall", app.composer.text)
	}
	app.composerKey("up", up) // multiline: cursor moves to the first line
	if app.composer.text != "a\nb" {
		t.Fatalf("second up left the multiline recall: %q", app.composer.text)
	}
	app.composerKey("up", up) // at the top line: keep iterating
	if app.composer.text != "old" {
		t.Fatalf("third up did not iterate past the multiline recall: %q", app.composer.text)
	}
}

func TestComposerActions(t *testing.T) {
	app, host := bootAgentApp(t, 60, 12)
	app.composer.pushHistory("first", 100)
	app.composer.pushHistory("second", 100)
	runLua(t, app, `zemi.composer.history_prev()
		zemi.composer.history_prev()`)
	if app.composer.text != "first" {
		t.Fatalf("two history_prev: %q", app.composer.text)
	}
	runLua(t, app, `zemi.composer.history_next()
		zemi.keymap.set("ctrl+y", "zemi.composer.insert !")`)
	press(t, app, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if app.composer.text != "second!" {
		t.Fatalf("history_next and the insert key: %q", app.composer.text)
	}
	err := host.RT.ExecuteChunk(t.Context(), `zemi.keymap.set("ctrl+u", "zemi.composer.submit")`, "p.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("a plugin bound a composer action: %v", err)
	}
}
