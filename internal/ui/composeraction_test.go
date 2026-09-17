package ui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/lua"
)

// TestComposerActions: the composer's history and insert run from Lua and
// from keymap strings, and only user config may bind them.
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
	err := host.RT.ExecuteChunk(context.Background(), `zemi.keymap.set("ctrl+u", "zemi.composer.submit")`, "p.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("a plugin bound a composer action: %v", err)
	}
}
