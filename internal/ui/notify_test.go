package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
	done := turnDoneMsg{}
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

// TestRecapWhenIdle: a recap starts once recapDelay passes after a turn that
// ended well with no input since, and the next input stops it.
func TestRecapWhenIdle(t *testing.T) {
	log := logging.New("")
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, c := range []struct {
		name  string
		done  turnDoneMsg
		input tea.Msg // after the turn ended
		want  bool
	}{
		{"away since a clean end", turnDoneMsg{}, nil, true},
		{"a key since", turnDoneMsg{}, key, false},
		{"focus back since", turnDoneMsg{}, tea.FocusMsg{}, false},
		{"hover since is not input", turnDoneMsg{}, tea.MouseMotionMsg{}, true},
		{"stopped", turnDoneMsg{stopped: true}, nil, false},
		{"failed", turnDoneMsg{err: "boom"}, nil, false},
	} {
		app := NewApp(host, log)
		now := time.Unix(1000, 0)
		app.now = func() time.Time { return now }
		app.Update(c.done)
		if c.input != nil {
			app.Update(c.input)
		}
		now = now.Add(recapDelay - time.Second)
		app.Update(frameTickMsg{})
		early := app.recapStop != nil
		now = now.Add(time.Second)
		app.Update(frameTickMsg{})
		if started := app.recapStop != nil; early || started != c.want {
			t.Errorf("%s: started early %v, at the deadline %v", c.name, early, started)
			continue
		}
		if c.want {
			stop, stopped := app.recapStop, false
			app.recapStop = func() { stopped = true; stop() }
			app.Update(key)
			if !stopped {
				t.Errorf("%s: a key did not stop the recap", c.name)
			}
		}
	}
}
