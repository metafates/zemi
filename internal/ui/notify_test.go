package ui

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// notifyApp is a bare zemi for the tests that drive ticks and turn ends
// alone: no config tree, no boot.
func notifyApp(t *testing.T) (*App, *hostapi.Host) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	return NewApp(host, log), host
}

// TestNotifyWhenAway: a finished turn notifies in the ui.notify style only
// while the terminal reports zemi unfocused, once the agent reads idle; a
// loop's run ends quietly, and the loop's end notifies.
func TestNotifyWhenAway(t *testing.T) {
	t.Parallel()
	app, host := notifyApp(t)
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
		loop        bool   // a loop run began the chain
		looped      bool   // a loop was on, and is no more
		want        string // the raw bytes sent, "" for no notification
	}{
		{"focused", "osc", nil, false, false, false, ""},
		{"away", "osc", tea.BlurMsg{}, false, false, false, "\x1b]9;zemi finished\x07"},
		{"away as a chain goes on", "osc", nil, true, false, false, "\x1b]9;zemi finished\x07"},
		{"away after a loop's run", "osc", nil, false, true, false, ""},
		{"away as the loop ends", "osc", nil, false, true, true, "\x1b]9;zemi: the loop ended\x07"},
		{"away with a bell", "bell", nil, false, false, false, "\a"},
		{"away and off", "off", nil, false, false, false, ""},
		{"back", "osc", tea.FocusMsg{}, false, false, false, ""},
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
		}
		host.SeedStream(hostapi.StreamSnapshot{ByLoop: c.loop})
		app.looping = c.looped
		if got := frame(); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRecapWhenIdle: once the agent went idle from a turn, a recap starts
// after recapDelay when the turn ended well with no input since, and the
// next input stops it; the reflector runs after ReflectIdle whatever came.
func TestRecapWhenIdle(t *testing.T) {
	t.Parallel()
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, c := range []struct {
		name           string
		done           turnDoneMsg
		input          tea.Msg // after the turn ended
		chain          bool    // the agent reads active on the first frame
		loop           bool    // a loop run began the chain
		recap, reflect bool
	}{
		{"away since a clean end", turnDoneMsg{}, nil, false, false, true, true},
		{"a key since", turnDoneMsg{}, key, false, false, false, true},
		{"focus back since", turnDoneMsg{}, tea.FocusMsg{}, false, false, false, true},
		{"hover since is not input", turnDoneMsg{}, tea.MouseMotionMsg{}, false, false, true, true},
		{"stopped", turnDoneMsg{stopped: true}, nil, false, false, false, true},
		{"failed", turnDoneMsg{err: "boom"}, nil, false, false, false, true},
		{"a chain goes on", turnDoneMsg{}, nil, true, false, false, false},
		{"a loop's run", turnDoneMsg{}, nil, false, true, false, true},
	} {
		app, host := notifyApp(t)
		start := time.Unix(1000, 0)
		now := start
		app.now = func() time.Time { return now }
		app.Update(c.done)
		host.SeedStream(hostapi.StreamSnapshot{Active: c.chain, ByLoop: c.loop})
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
	t.Parallel()
	running := func(app *App) { app.turnActive = true }
	for _, c := range []struct {
		name string
		set  func(*App)
		msgs []tea.Msg
		at   time.Duration
		want string
	}{
		{"idle", nil, nil, 0, "zemi"},
		{"running", running, nil, 0, "⠋ zemi"},
		{"running 100 ms on", running, nil, 100 * time.Millisecond, "⠋ zemi"},
		{"running 200 ms on", running, nil, 200 * time.Millisecond, "⠙ zemi"},
		{"asking", func(app *App) { running(app); app.permAsk = &permAskPrompt{} }, nil, 0, "? zemi"},
		{"done away", nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}}, 0, "✓ zemi"},
		{"failed away", nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{err: "x"}, frameTickMsg{}}, 0, "✗ zemi"},
		{"seen once focused", nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}, tea.FocusMsg{}}, 0, "zemi"},
		{"done here", nil, []tea.Msg{turnDoneMsg{}, frameTickMsg{}}, 0, "zemi"},
		{"cancelled away", nil, []tea.Msg{tea.BlurMsg{}, turnDoneMsg{stopped: true}, frameTickMsg{}}, 0, "zemi"},
	} {
		app, _ := notifyApp(t)
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

// TestToastTray: live toasts draw as one box of a fixed width, a section per
// level and title, newest first; a twin restarts its row, an expired toast's
// rows leave, and a short stage cuts the tray with its frame closed.
func TestToastTray(t *testing.T) {
	t.Parallel()
	app, _ := notifyApp(t)
	type push struct {
		level       slog.Level
		title, body string
	}
	signIn := func(server string) push { return push{slog.LevelWarn, "Sign-in needed", server + "; open /mcp"} }
	for _, c := range []struct {
		name   string
		pushes []push
		after  time.Duration // pruned this long after the pushes
		w, h   int
		want   []string
	}{
		{"one toast", []push{{slog.LevelInfo, "Copied", "12 bytes"}}, 0, 30, 24, []string{"┌ Copied ──────────────────┐", "│ 12 bytes                 │", "└──────────────────────────┘"}},
		{"a wide stage keeps the width", []push{{slog.LevelInfo, "Copied", "12 bytes"}}, 0, 100, 24, []string{"┌ Copied ──────────────────────────────────────┐", "│ 12 bytes                                     │", "└──────────────────────────────────────────────┘"}},
		{"two titles are two sections", []push{{slog.LevelInfo, "Copied", "12 bytes"}, signIn("jit")}, 0, 30, 24, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "├ Copied ──────────────────┤", "│ 12 bytes                 │", "└──────────────────────────┘"}},
		{"one title is one section, newest first", []push{signIn("chatzone"), signIn("jit"), signIn("confluence")}, 0, 30, 24, []string{"┌ Sign-in needed ──────────┐", "│ confluence; open /mcp    │", "│ jit; open /mcp           │", "│ chatzone; open /mcp      │", "└──────────────────────────┘"}},
		{"a twin restarts its row", []push{signIn("jit"), signIn("chatzone"), signIn("jit")}, 0, 30, 24, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "│ chatzone; open /mcp      │", "└──────────────────────────┘"}},
		{"an expired toast leaves", []push{signIn("jit"), {slog.LevelInfo, "Copied", "12 bytes"}}, 5 * time.Second, 30, 24, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "└──────────────────────────┘"}},
		{"a short stage cuts the tray", []push{{slog.LevelInfo, "Copied", "12 bytes"}, signIn("jit")}, 0, 30, 3, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "└──────────────────────────┘"}},
	} {
		app.toasts = nil
		for _, p := range c.pushes {
			app.pushToast(p.level, p.title, p.body)
		}
		app.pruneToasts(time.Now().Add(c.after))
		var got []string
		for _, r := range toastTray(app.toasts, c.w, c.h) {
			got = append(got, hostapi.SegsText(r))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// TestToastNotCopied: a drag over a box a toast stands on copies the box's own
// text, never the toast's words, which would paste two boxes as one.
func TestToastNotCopied(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	go app.AskPermission(hostapi.PermRequest{Subject: "tool.shell", Arg: "rm -rf build && go test ./..."})
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	// three sections: the tray reaches the ask's first text row, where the
	// oldest one shares its row with the ask's own facts
	for _, who := range []string{"a", "b", "c"} {
		app.pushToast(slog.LevelError, "background agent "+who+" failed", "the tool was denied")
	}
	app.renderSync(t)
	_, y := findCell(t, app, "background agent a failed")
	text := boxText(app.chromeRect)
	if y < text.Min.Y || y >= text.Max.Y {
		t.Fatalf("the toast's row %d is not among the box's text rows %v", y, text)
	}
	// the ask's facts row is where the oldest toast shares it, and the
	// box's own text there copies alone
	_, cy := findCell(t, app, "rm -rf build &&")
	dragTo(app, text.Min.X, cy, text.Max.X-1, cy)
	got := app.selectionText()
	if strings.Contains(got, "failed") || strings.Contains(got, "denied") {
		t.Fatalf("the drag copied the toast: %q", got)
	}
	if !strings.Contains(got, "command") {
		t.Fatalf("the drag copied nothing of the box: %q", got)
	}
}
