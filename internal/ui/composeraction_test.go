package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/lua"
)

func TestComposerActions(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 60, 12)
	app.composer.pushHistory("first")
	app.composer.pushHistory("second")
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
	if err := host.RT.ExecuteChunk(t.Context(), `zemi.keymap.set("ctrl+u", "zemi.composer.submit")`, "p.lua", lua.OriginPlugin); err != nil {
		t.Fatalf("a plugin bound a composer action: %v", err)
	}
}
