package ui

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestScreenshotPlainText: the screen zemi reads back is the frame it drew,
// without the styling or the padding, and it is empty before the first one.
func TestScreenshotPlainText(t *testing.T) {
	t.Parallel()
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

var _ hostapi.UIHooks = (*App)(nil)
