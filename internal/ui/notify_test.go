package ui

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestNotifyWhenAway: a finished turn notifies in the ui.notify style only
// while the terminal reports zemi unfocused, once the agent reads idle.
func TestNotifyWhenAway(t *testing.T) {
	log := logging.New("")
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	app := NewApp(host, log)
	frame := func() string {
		_, cmd := app.Update(frameTickMsg{})
		app.wakeTick() // the tick in the batch returns at the next frame boundary
		for _, m := range runCmd(cmd) {
			if raw, ok := m.(tea.RawMsg); ok {
				return fmt.Sprint(raw.Msg)
			}
		}
		return ""
	}
	for _, c := range []struct {
		name, style string
		focus       tea.Msg
		chain       bool   // the agent still reads active on the first frame
		want        string // the raw bytes sent, "" for no notification
	}{
		{"focused", "osc", nil, false, ""},
		{"away", "osc", tea.BlurMsg{}, false, "\x1b]9;zemi finished\x07"},
		{"away as a chain goes on", "osc", nil, true, "\x1b]9;zemi finished\x07"},
		{"away with a bell", "bell", nil, false, "\a"},
		{"away and off", "off", nil, false, ""},
		{"back", "osc", tea.FocusMsg{}, false, ""},
	} {
		if _, err := host.Settings.SetQuiet("ui.notify", c.style); err != nil {
			t.Fatal(err)
		}
		if c.focus != nil {
			app.Update(c.focus)
		}
		app.Update(turnDoneMsg{})
		if c.chain {
			host.SeedStream(hostapi.StreamSnapshot{Active: true})
			if got := frame(); got != "" {
				t.Errorf("%s: sent %q while the agent is active", c.name, got)
			}
			host.SeedStream(hostapi.StreamSnapshot{})
		}
		if got := frame(); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRecapWhenIdle: once the agent went idle from a turn, a recap starts
// after recapDelay when the turn ended well with no input since, and the
// next input stops it; the reflector runs after ReflectIdle whatever came.
func TestRecapWhenIdle(t *testing.T) {
	log := logging.New("")
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, c := range []struct {
		name           string
		done           turnDoneMsg
		input          tea.Msg // after the turn ended
		chain          bool    // the agent reads active on the first frame
		recap, reflect bool
	}{
		{"away since a clean end", turnDoneMsg{}, nil, false, true, true},
		{"a key since", turnDoneMsg{}, key, false, false, true},
		{"focus back since", turnDoneMsg{}, tea.FocusMsg{}, false, false, true},
		{"hover since is not input", turnDoneMsg{}, tea.MouseMotionMsg{}, false, true, true},
		{"stopped", turnDoneMsg{stopped: true}, nil, false, false, true},
		{"failed", turnDoneMsg{err: "boom"}, nil, false, false, true},
		{"a chain goes on", turnDoneMsg{}, nil, true, false, false},
	} {
		app := NewApp(host, log)
		start := time.Unix(1000, 0)
		now := start
		app.now = func() time.Time { return now }
		app.Update(c.done)
		if c.chain {
			host.SeedStream(hostapi.StreamSnapshot{Active: true})
		}
		app.Update(frameTickMsg{})
		host.SeedStream(hostapi.StreamSnapshot{})
		if c.input != nil {
			app.Update(c.input)
		}
		now = start.Add(recapDelay - time.Second)
		app.Update(frameTickMsg{})
		early := app.recapStop != nil
		now = start.Add(recapDelay)
		app.Update(frameTickMsg{})
		if started := app.recapStop != nil; early || started != c.recap {
			t.Errorf("%s: recap started early %v, at the deadline %v", c.name, early, started)
		}
		if c.recap {
			stop, stopped := app.recapStop, false
			app.recapStop = func() { stopped = true; stop() }
			app.Update(key)
			if !stopped {
				t.Errorf("%s: a key did not stop the recap", c.name)
			}
		}
		now = start.Add(hostapi.ReflectIdle)
		app.Update(frameTickMsg{})
		if reflected := app.reflectAt.IsZero(); reflected != c.reflect {
			t.Errorf("%s: reflected at ReflectIdle %v, want %v", c.name, reflected, c.reflect)
		}
	}
}

// TestWindowTitle: the tab title leads with a spinner stepping every 200 ms
// while a turn runs, ? while a prompt waits, and how a turn ended while the
// window was away until it is focused again.
func TestWindowTitle(t *testing.T) {
	log := logging.New("")
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	running := func(app *App) { app.turnActive = true }
	for _, c := range []struct {
		name  string
		still bool // ui.animations off
		set   func(*App)
		msgs  []tea.Msg
		at    time.Duration
		want  string
	}{
		{"idle", false, nil, nil, 0, "zemi"},
		{"running", false, running, nil, 0, "⠋ zemi"},
		{"running 100 ms on", false, running, nil, 100 * time.Millisecond, "⠋ zemi"},
		{"running 200 ms on", false, running, nil, 200 * time.Millisecond, "⠙ zemi"},
		{"animations off holds still", true, running, nil, 200 * time.Millisecond, "⠋ zemi"},
		{"asking", false, func(app *App) { running(app); app.permAsk = &permAskPrompt{} }, nil, 0, "? zemi"},
		{"done away", false, nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}}, 0, "✓ zemi"},
		{"failed away", false, nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{err: "x"}, frameTickMsg{}}, 0, "✗ zemi"},
		{"seen once focused", false, nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}, tea.FocusMsg{}}, 0, "zemi"},
		{"done here", false, nil, []tea.Msg{turnDoneMsg{}, frameTickMsg{}}, 0, "zemi"},
		{"cancelled away", false, nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{stopped: true}, frameTickMsg{}}, 0, "zemi"},
	} {
		if _, err := host.Settings.SetQuiet("ui.animations", !c.still); err != nil {
			t.Fatal(err)
		}
		app := NewApp(host, log)
		app.now = func() time.Time { return time.UnixMilli(1_000_000).Add(c.at) }
		for _, m := range c.msgs {
			app.Update(m)
		}
		if c.set != nil {
			c.set(app)
		}
		if got := app.windowTitle(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
