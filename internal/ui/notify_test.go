package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestNotifyWhenAway: a finished turn notifies through the terminal only
// while the terminal reports zemi unfocused.
func TestNotifyWhenAway(t *testing.T) {
	log := logging.New("")
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	app := NewApp(host, log)
	done := agentEventMsg{ev: hostapi.AgentEvent{Kind: "turn_done"}}
	for _, c := range []struct {
		name   string
		focus  tea.Msg
		notify bool
	}{
		{"focused", nil, false},
		{"away", tea.BlurMsg{}, true},
		{"back", tea.FocusMsg{}, false},
	} {
		if c.focus != nil {
			app.Update(c.focus)
		}
		_, cmd := app.Update(done)
		if (cmd != nil) != c.notify {
			t.Fatalf("%s: notification = %v", c.name, cmd != nil)
		}
		if cmd != nil {
			raw, ok := cmd().(tea.RawMsg)
			if !ok || !strings.Contains(fmt.Sprint(raw.Msg), "\x1b]9;zemi finished") {
				t.Fatalf("%s: cmd = %#v", c.name, raw)
			}
		}
	}
}
