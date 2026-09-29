package ui

import "testing"

// TestRelaunch: a relaunch the quit veto stopped leaves no dir for the next
// /quit to restart in, and one past it names its dir.
func TestRelaunch(t *testing.T) {
	for _, c := range []struct {
		name, veto, want string
	}{
		{"a vetoed relaunch", `return { decision = "deny", reason = "busy" }`, ""},
		{"a relaunch past the veto", `return nil`, "/tmp/other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 80, 20)
			runLua(t, app, `zemi.event.on("app.quit", function() `+c.veto+` end)`)
			app.Relaunch("/tmp/other")
			if got := app.RelaunchDir(); got != c.want {
				t.Fatalf("RelaunchDir = %q, want %q", got, c.want)
			}
		})
	}
}
