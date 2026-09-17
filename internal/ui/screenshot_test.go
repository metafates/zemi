package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/win"
)

// TestScreenshotPlainText: the screen zemi reads back is the frame it drew,
// without the styling or the padding, and it is empty before the first one.
func TestScreenshotPlainText(t *testing.T) {
	app, host := bootAgentApp(t, 60, 12)
	if got := app.Screenshot(); got != "" {
		t.Fatalf("no frame drawn yet, got %q", got)
	}

	host.AppendMessage("user", "why does the build fail?")
	app.renderSync(t)
	app.Frame()

	shot := app.Screenshot()
	if !strings.Contains(shot, "why does the build fail?") {
		t.Fatalf("the transcript is missing:\n%s", shot)
	}
	if strings.ContainsRune(shot, 0x1b) || strings.Contains(shot, "\r") {
		t.Fatalf("styling leaked into the screenshot:\n%q", shot)
	}
	for line := range strings.SplitSeq(shot, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("row padded to the frame width: %q", line)
		}
	}
}

// TestScreenshotBlanksEditor: what the user types in an editor window never
// reaches the screenshot, while other windows stay readable.
func TestScreenshotBlanksEditor(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	app.CaptureHooks()
	defer host.Wins.CloseAll()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for _, c := range []struct{ owner, text string }{{"editor", "typed-secret"}, {"model", "model-output"}} {
		wn, err := host.Wins.Open(win.Spec{Cmd: "echo " + c.text + "; sleep 5", Kind: win.KindSplit, Owner: c.owner})
		if err != nil {
			t.Fatal(err)
		}
		if text, ok := wn.WaitText(ctx, c.text); !ok {
			t.Fatalf("%s window: %s", c.owner, text)
		}
	}
	app.Frame()
	shot := app.Screenshot()
	if strings.Contains(shot, "typed-secret") || !strings.Contains(shot, "model-output") {
		t.Fatalf("screenshot:\n%s", shot)
	}
}

var _ hostapi.UIHooks = (*App)(nil)
