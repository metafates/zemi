package ui

import (
	"testing"
)

// TestComposerSubmitRewrite: a composer.submit rewrite verdict sends the
// rewritten text, and the history carries what was sent.
func TestComposerSubmitRewrite(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 12)
	runLua(t, app, `zemi.event.on("composer.submit", function(ev)
		if ev.text == "short" then return { decision = "rewrite", text = "expanded text" } end
	end)`)
	app.composer.text = "short"
	app.sendMessage("short")
	runLua(t, app, `zemi.composer.history_prev()`)
	if app.composer.text != "expanded text" {
		t.Fatalf("history holds %q, want the rewritten text", app.composer.text)
	}
	// no verdict: what was typed goes as is
	app.sendMessage("plain")
	runLua(t, app, `zemi.composer.history_prev()`)
	if app.composer.text != "plain" {
		t.Fatalf("history holds %q, want the text as typed", app.composer.text)
	}
}
