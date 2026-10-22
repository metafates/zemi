package ui

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/lua"
)

// TestReloadKeepsTheFrame: a reload's window has the old generation's
// widgets and theme purged, and a frame drawn there flashes without them;
// the screen keeps the last frame drawn until the new generation draws,
// like a reload keeps the old layout.
func TestReloadKeepsTheFrame(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	seedLongChat(host, 1)
	app.renderSync(t)
	before := ansi.Strip(app.Frame())

	started, release := make(chan struct{}), make(chan struct{})
	straddle := lua.BootStep{Name: "straddle", Run: func(ctx context.Context, _ *lua.Runtime, _ *lua.Generation) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}}
	steps := append([]lua.BootStep{straddle}, host.NamespacesStep(), host.BuiltinCommandsStep(), lua.RequireStep("zemi.tui"))
	errc := make(chan error, 1)
	go func() { errc <- host.ForceReload(t.Context(), steps) }()
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("reload: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never reached the straddle step")
	}
	for range 20 {
		if got := ansi.Strip(app.Frame()); got != before {
			t.Fatalf("a reload redrew the screen mid-reload:\n%s", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}
