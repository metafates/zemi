package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestGoldenModelRoles pins /model roles, a role's model list stacked over
// it, and the roles list rebuilt with the pick once that list closes.
func TestGoldenModelRoles(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	runLua(t, app, `
		zemi.provider.add({ id = "anthropic", kind = "openai_compat", base_url = "http://x", api_key = "k", models = {
			{ id = "claude-sonnet-4-5", display = "Claude Sonnet 4.5" }, { id = "claude-haiku-4-5", display = "Claude Haiku 4.5" } } })
		zemi.model.set("anthropic/claude-sonnet-4-5")
		zemi.config.set("models.memory", "anthropic/claude-haiku-4-5")`)
	// main.go owns /model
	host.Commands.Add(&hostapi.Command{
		Name: "model", NArgs: "*", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(h *hostapi.Host, args string) error { return h.SetModel(args) },
	})
	frame := func(name string) {
		for app.Pump() {
		}
		app.renderSync(t)
		t.Run(name, func(t *testing.T) { golden.RequireEqual(t, app.Frame()) })
	}
	app.runCommand("model", "roles")
	frame("roles")

	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> titles
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	frame("titles")

	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> haiku
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	frame("picked")
	if got := host.Settings.Str("models.titles"); got != "anthropic/claude-haiku-4-5" {
		t.Fatalf("models.titles = %q", got)
	}
}
