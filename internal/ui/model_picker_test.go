package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestModelPickerFitsItsRows: the picker opens at the width its own rows
// take, so a long model id draws whole from the first frame and the box is
// no wider than the rows need.
func TestModelPickerFitsItsRows(t *testing.T) {
	const id = "prov/an-extremely-long-model-identifier"
	app, host := bootAgentApp(t, 100, 24)
	runLua(t, app, `
		zemi.provider.add({ id = "prov", kind = "openai_compat", base_url = "http://x", api_key = "k", models = {
			{ id = "`+id+`", display = "A very long display name indeed" }, { id = "short", display = "Short" } } })`)
	host.Commands.Add(&hostapi.Command{
		Name: "model", NArgs: "*", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(h *hostapi.Host, args string, confirm bool) error { return h.SetModel(args) },
	})
	app.runCommand("model", "", false)
	whole := func() bool { return strings.Contains(ansi.Strip(app.Frame()), id) }
	for i := range 3 {
		for app.Pump() {
		}
		app.renderSync(t)
		if !whole() {
			t.Fatalf("frame %d trims the model id", i)
		}
		// the box hugs its rows: at this width a full-screen box is 96 wide
		width := 0
		for line := range strings.SplitSeq(ansi.Strip(app.Frame()), "\n") {
			if j := strings.Index(line, "┌ Model"); j >= 0 {
				width = len([]rune(strings.TrimRight(line, " \r"))) - j
			}
		}
		if width == 0 || width > 92 {
			t.Fatalf("frame %d: the box is %d wide, wider than its rows", i, width)
		}
		app.Update(tea.KeyPressMsg{}) // nudge a frame
	}
}
