package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

func typeSecret(t *testing.T, app *App, s string) {
	t.Helper()
	for _, r := range s {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// TestSecretInput: a masked input draws bullets only, saves on enter off
// the command path, reopens with the text on a failed save, and esc saves
// nothing. The value rules live in pasteKey; the box only ever shows
// bullets.
func TestSecretInput(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	var saved []string
	fail := false
	open := func() {
		app.openOverlay(hostapi.Overlay{
			ID: "keys paste", Title: "Paste the deepseek API key", Input: true, Secret: true,
			Placeholder: "the API key", Note: "Saves in the OS keychain.",
			OnInput: func(text string) error {
				if fail {
					return errors.New("the chain refused")
				}
				saved = append(saved, text)
				return nil
			},
		})
	}
	open()
	app.renderSync(t)
	f := ansi.Strip(app.Frame())
	if !strings.Contains(f, "Saves in the OS keychain.") || !strings.Contains(f, "enter save") {
		t.Fatalf("the paste box:\n%s", f)
	}
	typeSecret(t, app, "sk-secret")
	app.renderSync(t)
	if f = ansi.Strip(app.Frame()); strings.Contains(f, "sk-secret") {
		t.Fatalf("the key echoes:\n%s", f)
	}
	if !strings.Contains(f, "•••••••••") {
		t.Fatalf("no mask:\n%s", f)
	}
	// esc closes and saves nothing
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(saved) != 0 {
		t.Fatalf("esc saved %v", saved)
	}
	// enter hands OnInput the text, off the command path; in tests the
	// background worker runs inline
	open()
	typeSecret(t, app, "sk-deep-123")
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for app.Pump() {
	}
	if len(saved) != 1 || saved[0] != "sk-deep-123" {
		t.Fatalf("saved %v", saved)
	}
	if app.overlayIndex("keys paste") >= 0 {
		t.Fatal("the box survived a save")
	}
	// a failed save reopens the box with the text back, still masked
	fail = true
	open()
	typeSecret(t, app, "sk-secret")
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if app.overlayIndex("keys paste") < 0 {
		t.Fatal("the failed save did not reopen the box")
	}
	app.renderSync(t)
	if f = ansi.Strip(app.Frame()); strings.Contains(f, "sk-secret") {
		t.Fatalf("the reopened box echoes the key:\n%s", f)
	}
}
