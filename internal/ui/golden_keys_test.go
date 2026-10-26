package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// registerKeys adds /keys the way the builtin-commands step would, so the
// golden tests drive the real dispatch.
func registerKeys(t *testing.T, host *hostapi.Host) {
	t.Helper()
	if _, err := host.Commands.Add(&hostapi.Command{
		Name: "keys", NArgs: hostapi.NArgsAny, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(h *hostapi.Host, args string, confirm bool) error { return h.KeysCommand(args) },
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGoldenKeys pins /keys: a provider keyed from the catalog's env, one
// config set, and the builtins waiting for a key.
func TestGoldenKeys(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	registerKeys(t, host)
	host.Providers.Reset(agent.Catalog{
		Specs: []agent.Spec{
			{ID: "anthropic", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "ANTHROPIC_API_KEY", BaseURL: "https://api.anthropic.com", Models: []agent.Model{{ID: "claude"}}},
		},
		Keys:    map[string]string{"anthropic": "env-key"},
		Missing: []agent.MissingKey{{ID: "deepseek", Env: "DEEPSEEK_API_KEY", Spec: agent.Spec{ID: "deepseek", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "DEEPSEEK_API_KEY", DefaultModel: "deepseek-chat"}}},
	})
	if err := host.Providers.Register(agent.Spec{ID: "gw", Kind: agent.KindOpenAICompat, BaseURL: "https://gw.internal/v1"}, "cfg-key", false); err != nil {
		t.Fatal(err)
	}
	app.runCommand("keys", "", false)
	for app.Pump() {
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}

// TestGoldenPasteKey pins the masked paste input: bullets under the cursor,
// the destination note, and the save hint.
func TestGoldenPasteKey(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	registerKeys(t, host)
	host.Providers.Reset(agent.Catalog{
		Missing: []agent.MissingKey{{ID: "deepseek", Env: "DEEPSEEK_API_KEY", Spec: agent.Spec{ID: "deepseek", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "DEEPSEEK_API_KEY", DefaultModel: "deepseek-chat"}}},
	})
	app.runCommand("keys", "paste deepseek", false)
	for app.Pump() {
	}
	for _, r := range "sk-deep-123" {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}
