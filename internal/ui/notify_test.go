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
	t.Cleanup(host.Close)
	return NewApp(host, log), host
}

// notifyFrame runs one frame and returns the bytes it sent as a desktop
// notification, "" for none; the program-status report rides the same batch.
func notifyFrame(app *App) string {
	_, cmd := app.Update(frameTickMsg{})
	app.wakeTick() // the tick in the batch returns at the next frame boundary
	for _, m := range runCmd(cmd) {
		if raw, ok := m.(tea.RawMsg); ok {
			if s := fmt.Sprint(raw.Msg); !strings.Contains(s, "\x1b]7501;") {
				return s
			}
		}
	}
	return ""
}

// notifyStyled is a bare zemi whose ui.notify style is style.
func notifyStyled(t *testing.T, style string) (*App, *hostapi.Host) {
	t.Helper()
	app, host := notifyApp(t)
	if _, err := host.Settings.SetQuiet("ui.notify", style); err != nil {
		t.Fatal(err)
	}
	return app, host
}

// notifyAwayApp is a bare zemi whose terminal is unfocused and whose
// ui.notify style is style.
func notifyAwayApp(t *testing.T, style string) (*App, *hostapi.Host) {
	t.Helper()
	app, host := notifyStyled(t, style)
	app.Update(tea.BlurMsg{})
	return app, host
}

// TestNotifyWhenAway: a finished turn notifies in the ui.notify style, its
// words following how the turn ended and what the reply said.
func TestNotifyWhenAway(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		style   string
		support bool        // the terminal answered the OSC 7501 feature query
		done    turnDoneMsg // how the turn ended
		reply   string      // the chat row the turn left behind
		want    string      // the raw bytes sent, "" for no notification
	}{
		{"away", "osc", false, turnDoneMsg{}, "", "\x1b]9;zemi finished\x07"},
		{"away with support", "osc", true, turnDoneMsg{}, "", ""},
		{"away with a failing turn", "osc", false, turnDoneMsg{err: "provider refused\n\tand again"}, "", "\x1b]9;zemi failed: provider refused and again\x07"},
		{"away with a failing turn and support", "osc", true, turnDoneMsg{err: "provider refused"}, "", ""},
		{"away with a long failure", "osc", false, turnDoneMsg{err: strings.Repeat("x", 300)}, "", "\x1b]9;zemi failed: " + strings.Repeat("x", 79) + "…\x07"},
		{"away with a reply", "osc", false, turnDoneMsg{}, "all 9 tests pass", "\x1b]9;all 9 tests pass\x07"},
		{"away with a long reply", "osc", false, turnDoneMsg{}, strings.Repeat("x", 300), "\x1b]9;" + strings.Repeat("x", 199) + "…\x07"},
		{"away with a headed reply", "osc", false, turnDoneMsg{}, "# Deploy notes\nall green", "\x1b]9;Deploy notes all green\x07"},
		{"away with a tldr reply", "osc", false, turnDoneMsg{}, "# Deploy notes\n" + strings.Repeat("it checks every suite and reports each one green. ", 16) + "\n<tldr>deploy notes all green</tldr>", "\x1b]9;deploy notes all green\x07"},
		{"away with a bell", "bell", false, turnDoneMsg{}, "", "\a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := notifyAwayApp(t, c.style)
			app.statusSupport = c.support
			app.Update(c.done)
			host.SeedStream(hostapi.StreamSnapshot{})
			if c.reply != "" {
				host.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: c.reply})
			}
			if got := notifyFrame(app); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// TestNotifyAsTheLoopEnds: a loop's runs end quietly; the loop's end
// notifies, unless the terminal already carries the status record.
func TestNotifyAsTheLoopEnds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		looped  bool // a loop was on, and is no more
		support bool // the terminal answered the OSC 7501 feature query
		want    string
	}{
		{"away after a loop's run", false, false, ""},
		{"away as the loop ends", true, false, "\x1b]9;zemi: the loop ended\x07"},
		{"away as the loop ends with support", true, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := notifyAwayApp(t, "osc")
			app.statusSupport = c.support
			app.Update(turnDoneMsg{})
			host.SeedStream(hostapi.StreamSnapshot{ByLoop: true})
			app.looping = c.looped
			if got := notifyFrame(app); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// TestAlertWhenAway: a background completion (Host.Alert) toasts and
// notifies in the ui.notify style while the window is away, even where a
// turn's end would not — the event style no status record carries.
func TestAlertWhenAway(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, style string
		focus       tea.Msg
		ping        func(*App)
		want        string // the raw bytes sent, "" for no notification
	}{
		{"away", "osc", tea.BlurMsg{}, func(a *App) { a.Alert(slog.LevelInfo, "Signed in to github", "") }, "\x1b]9;zemi: signed in to github\x07"},
		{"away, an error with a body", "osc", tea.BlurMsg{}, func(a *App) {
			a.Alert(slog.LevelError, "Turn crashed", "zemi kept running; the cause is in /errors")
		}, "\x1b]9;zemi: turn crashed: zemi kept running; the cause is in /errors\x07"},
		{"focused", "osc", nil, func(a *App) { a.Alert(slog.LevelInfo, "Signed in to github", "") }, ""},
		{"away with a bell", "bell", tea.BlurMsg{}, func(a *App) { a.Alert(slog.LevelInfo, "Signed in to github", "") }, "\a"},
	} {
		app, host := notifyApp(t)
		if _, err := host.Settings.SetQuiet("ui.notify", c.style); err != nil {
			t.Fatal(err)
		}
		if c.focus != nil {
			app.Update(c.focus)
		}
		app.testQueue = make(chan tea.Msg, 2) // the toast and the alert post apart
		c.ping(app)
		var sent string
		for range 2 {
			if _, cmd := app.Update(<-app.testQueue); cmd != nil {
				for _, m := range runCmd(cmd) {
					if raw, ok := m.(tea.RawMsg); ok {
						sent = fmt.Sprint(raw.Msg)
					}
				}
			}
		}
		if sent != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, sent, c.want)
		}
		if len(app.toasts) == 0 {
			t.Errorf("%s: no toast", c.name)
		}
	}
}

// TestRecapWhenIdle: once the agent went idle from a turn, a recap starts
// after the session.recap delay when the turn ended well with no input
// since, and the next input stops it. The recap delay comes from the config,
// so the test sets a non-default one.
func TestRecapWhenIdle(t *testing.T) {
	t.Parallel()
	const recapDelay = 4 * time.Minute
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, c := range []struct {
		name  string
		done  turnDoneMsg
		input tea.Msg // after the turn ended
		chain bool    // the agent reads active on the first frame
		loop  bool    // a loop run began the chain
		recap bool
	}{
		{"away since a clean end", turnDoneMsg{}, nil, false, false, true},
		{"a key since", turnDoneMsg{}, key, false, false, false},
		{"focus back since", turnDoneMsg{}, tea.FocusMsg{}, false, false, false},
		{"hover since is not input", turnDoneMsg{}, tea.MouseMotionMsg{}, false, false, true},
		{"stopped", turnDoneMsg{stopped: true}, nil, false, false, false},
		{"failed", turnDoneMsg{err: "boom"}, nil, false, false, false},
		{"a chain goes on", turnDoneMsg{}, nil, true, false, false},
		{"a loop's run", turnDoneMsg{}, nil, false, true, false},
	} {
		app, host := notifyApp(t)
		if _, err := host.Settings.SetQuiet("session.recap", "4m"); err != nil {
			t.Fatal(err)
		}
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
	}
}

// TestStopWording keeps its own file; the next test starts here.
// TestWindowTitle: the tab title marks how a turn ended while the window was
// away, until it is focused again.
func TestWindowTitle(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		msgs []tea.Msg
		at   time.Duration
		want string
	}{
		{"idle", nil, 0, "zemi"},
		{"done away", []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}}, 0, "✓ zemi"},
		{"failed away", []tea.Msg{tea.BlurMsg{}, turnDoneMsg{err: "x"}, frameTickMsg{}}, 0, "✗ zemi"},
		{"seen once focused", []tea.Msg{tea.BlurMsg{}, turnDoneMsg{}, frameTickMsg{}, tea.FocusMsg{}}, 0, "zemi"},
		{"done here", []tea.Msg{turnDoneMsg{}, frameTickMsg{}}, 0, "zemi"},
		{"cancelled away", []tea.Msg{tea.BlurMsg{}, turnDoneMsg{stopped: true}, frameTickMsg{}}, 0, "zemi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, _ := notifyApp(t)
			app.now = func() time.Time { return time.UnixMilli(1_000_000).Add(c.at) }
			for _, m := range c.msgs {
				app.Update(m)
			}
			if got := app.windowTitle(); got != c.want {
				t.Errorf("%q, want %q", got, c.want)
			}
		})
	}
}

// TestWindowTitleAsksTrust: so does the trust prompt, which waits before any
// turn.
func TestWindowTitleAsksTrust(t *testing.T) {
	t.Parallel()
	app, _ := notifyApp(t)
	app.trust = &trustPrompt{root: "/proj"}
	if got := app.windowTitle(); got != "? zemi" {
		t.Errorf("%q, want ? zemi", got)
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
		{"a shorter stage drops the older section whole", []push{{slog.LevelInfo, "Copied", "12 bytes"}, signIn("jit")}, 0, 30, 4, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "└──────────────────────────┘"}},
	} {
		app.toasts = nil
		for _, p := range c.pushes {
			app.pushToast(p.level, p.title, p.body)
		}
		app.pruneToasts(time.Now().Add(c.after))
		var got []string
		tray := toastTray(app.toasts, c.w, c.h, time.Now())
		for _, r := range tray {
			got = append(got, hostapi.SegsText(r))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}
