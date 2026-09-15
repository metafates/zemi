package ui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestGoldenHelpOverlay pins the help box; enter on a command row closes
// it and fills the composer.
func TestGoldenHelpOverlay(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.Commands.Add(&hostapi.Command{Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1)})
	host.OpenHelp()
	for app.Pump() {
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	st := &app.overlays[0]
	st.selected = len(st.itemText) - 1
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(app.overlays) != 0 || app.composer.text != "/model " {
		t.Fatalf("enter on a command row: overlays=%d composer=%q", len(app.overlays), app.composer.text)
	}
}

// TestResizeSweep resizes one busy frame (markdown reply, multi-line toast,
// permission box) through degenerate and normal sizes; no row may ever be
// wider than the terminal.
func TestResizeSweep(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "show me")
	host.AppendTurnResult("# Heading\n\nsome **bold** and `code` with a verylongunbreakableidentifierthatneverfits\n\n```go\nfunc main() { println(\"a long line of code that overflows\") }\n```\n\n- item one\n- item two\n", hostapi.TurnStat{Out: 10})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Tools: []hostapi.StreamTool{
		{Name: "bash", Input: strings.Repeat("ёж ", 60), Status: "run"},
	}})

	// Before any popup covers it: the long tool input stays on one row, cut
	// by character with "…".
	app.renderSync(t)
	var toolRows []string
	for _, row := range strings.Split(ansi.Strip(app.Frame()), "\r\n") {
		if strings.Contains(row, "ёж") {
			toolRows = append(toolRows, row)
		}
	}
	if len(toolRows) != 1 || !strings.Contains(toolRows[0], "• bash") || !strings.HasSuffix(strings.TrimRight(toolRows[0], " "), "…") {
		t.Fatalf("tool row must be one truncated row, got %q", toolRows)
	}
	app.pushToast("warn", "a rather long toast title that must truncate", "line one of the body\nline two which is long enough to need wrapping at small widths")
	host.OpenHelp()
	for app.Pump() {
	}
	if len(app.overlays) != 1 {
		t.Fatal("help overlay did not open")
	}
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.bash", Arg: "rm -rf build && go test ./..."}}

	sizes := []struct{ w, h int }{
		{1, 1}, {2, 3}, {5, 2}, {12, 6}, {20, 10}, {40, 8}, {80, 24}, {33, 50}, {200, 60}, {7, 40},
	}
	for _, sz := range sizes {
		app.Update(tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		app.renderSync(t)
		frame := app.Frame()
		if !utf8.ValidString(frame) {
			t.Fatalf("%dx%d: frame is not valid UTF-8", sz.w, sz.h)
		}
		for i, row := range strings.Split(frame, "\r\n") {
			if n := ansi.StringWidth(row); n > sz.w {
				t.Fatalf("%dx%d: row %d is %d cells wide: %q", sz.w, sz.h, i, n, ansi.Strip(row))
			}
		}
	}
}
