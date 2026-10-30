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
	"github.com/metafates/zemi/internal/tools"
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
				if s := fmt.Sprint(raw.Msg); !strings.Contains(s, "\x1b]7501;") { // the status report rides the same batch
					return s
				}
			}
		}
		return ""
	}
	for _, c := range []struct {
		name, style string
		focus       tea.Msg
		chain       bool                // the agent still reads active on the first frame
		loop        bool                // a loop run began the chain
		looped      bool                // a loop was on, and is no more
		support     bool                // the terminal answered the OSC 7501 feature query
		done        turnDoneMsg         // how the turn ended
		seed        func(*hostapi.Host) // chat rows the turn left behind
		want        string              // the raw bytes sent, "" for no notification
	}{
		{"focused", "osc", nil, false, false, false, false, turnDoneMsg{}, nil, ""},
		{"away", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{}, nil, "\x1b]9;zemi finished\x07"},
		{"away with support", "osc", tea.BlurMsg{}, false, false, false, true, turnDoneMsg{}, nil, ""},
		{"away as a chain goes on", "osc", nil, true, false, false, false, turnDoneMsg{}, nil, "\x1b]9;zemi finished\x07"},
		{"away after a loop's run", "osc", nil, false, true, false, false, turnDoneMsg{}, nil, ""},
		{"away as the loop ends", "osc", nil, false, true, true, false, turnDoneMsg{}, nil, "\x1b]9;zemi: the loop ended\x07"},
		{"away as the loop ends with support", "osc", nil, false, true, true, true, turnDoneMsg{}, nil, ""},
		{"away with a failing turn", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{err: "provider refused\n\tand again"}, nil, "\x1b]9;zemi failed: provider refused and again\x07"},
		{"away with a failing turn and support", "osc", tea.BlurMsg{}, false, false, false, true, turnDoneMsg{err: "provider refused"}, nil, ""},
		{"away with a long failure", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{err: strings.Repeat("x", 300)}, nil, "\x1b]9;zemi failed: " + strings.Repeat("x", 79) + "…\x07"},
		{"away with a reply", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{}, func(h *hostapi.Host) {
			h.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: "all 9 tests pass"})
		}, "\x1b]9;zemi finished: all 9 tests pass\x07"},
		{"away with a long reply", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{}, func(h *hostapi.Host) {
			h.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: strings.Repeat("x", 300)})
		}, "\x1b]9;zemi finished: " + strings.Repeat("x", 199) + "…\x07"},
		{"away with a headed reply", "osc", tea.BlurMsg{}, false, false, false, false, turnDoneMsg{}, func(h *hostapi.Host) {
			h.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: "# Deploy notes\nall green"})
		}, "\x1b]9;zemi finished: Deploy notes all green\x07"},
		{"away with a bell", "bell", nil, false, false, false, false, turnDoneMsg{}, nil, "\a"},
		{"back", "osc", tea.FocusMsg{}, false, false, false, false, turnDoneMsg{}, nil, ""},
	} {
		if _, err := host.Settings.SetQuiet("ui.notify", c.style); err != nil {
			t.Fatal(err)
		}
		app.statusSupport = c.support
		if c.focus != nil {
			app.Update(c.focus)
		}
		app.Update(c.done)
		if c.chain {
			host.SeedStream(hostapi.StreamSnapshot{Active: true})
			if got := frame(); got != "" {
				t.Errorf("%s: sent %q while the agent is active", c.name, got)
			}
		}
		host.SeedStream(hostapi.StreamSnapshot{ByLoop: c.loop})
		app.looping = c.looped
		if c.seed != nil {
			c.seed(host)
		}
		if got := frame(); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAskNotifyRidesTheRecord: an ask alerts an unfocused terminal only
// where the program-status record does not already carry it — a terminal
// that answered the OSC 7501 feature query shows the record itself.
func TestAskNotifyRidesTheRecord(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		support bool
		want    string
	}{
		{"without support", false, "\x1b]9;zemi: a question: which database?\x07"},
		{"with support", true, ""},
	} {
		app, _ := notifyApp(t)
		app.Update(tea.BlurMsg{})
		app.statusSupport = c.support
		app.testQueue = make(chan tea.Msg, 1)
		answered := make(chan struct{}, 1)
		go func() {
			app.AskQuestion([]tools.Question{{Question: "which database?"}})
			answered <- struct{}{}
		}()
		_, cmd := app.Update(<-app.testQueue)
		var got string
		for _, m := range runCmd(cmd) {
			if raw, ok := m.(tea.RawMsg); ok {
				if s := fmt.Sprint(raw.Msg); strings.Contains(s, "\x1b]9;") {
					got = s
				}
			}
		}
		if got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
		app.finishQuestion(false)
		<-answered
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

// TestAskQuestionNotifiesWhenAway: the question ask alerts an unfocused
// terminal with its batch count and its first line.
func TestAskQuestionNotifiesWhenAway(t *testing.T) {
	t.Parallel()
	app, host := notifyApp(t)
	if _, err := host.Settings.SetQuiet("ui.notify", "osc"); err != nil {
		t.Fatal(err)
	}
	app.Update(tea.BlurMsg{})
	app.testQueue = make(chan tea.Msg, 1)
	done := make(chan bool, 1)
	go func() {
		_, ok := app.AskQuestion([]tools.Question{{Question: "which database?"}, {Question: "which driver?"}})
		done <- ok
	}()
	var sent string
	if _, cmd := app.Update(<-app.testQueue); cmd != nil {
		for _, m := range runCmd(cmd) {
			if raw, ok := m.(tea.RawMsg); ok {
				sent = fmt.Sprint(raw.Msg)
			}
		}
	}
	if sent != "\x1b]9;zemi: a question (1 of 2): which database?\x07" {
		t.Fatalf("sent %q", sent)
	}
	app.finishQuestion(false)
	if got := <-done; got {
		t.Fatal("a dismissed batch answered true")
	}
}

// TestAskTrustNotifiesWhenAway: the trust ask alerts an unfocused terminal
// like the other three asks do.
func TestAskTrustNotifiesWhenAway(t *testing.T) {
	t.Parallel()
	app, host := notifyApp(t)
	if _, err := host.Settings.SetQuiet("ui.notify", "osc"); err != nil {
		t.Fatal(err)
	}
	app.Update(tea.BlurMsg{})
	app.testQueue = make(chan tea.Msg, 1)
	done := make(chan string, 1)
	go func() { done <- app.AskTrust("/proj", "/proj", "") }()
	var sent string
	if _, cmd := app.Update(<-app.testQueue); cmd != nil {
		for _, m := range runCmd(cmd) {
			if raw, ok := m.(tea.RawMsg); ok {
				sent = fmt.Sprint(raw.Msg)
			}
		}
	}
	if sent != "\x1b]9;zemi: trust this project? · /proj\x07" {
		t.Fatalf("sent %q", sent)
	}
	app.answerTrust("later")
	if got := <-done; got != "later" {
		t.Fatalf("answered %q", got)
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
// TestWindowTitle: the tab title asks with ? and marks how a turn ended
// while the window was away, until it is focused again; a running turn
// leaves the title alone — the program status reports it.
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
		{"running", running, nil, 0, "zemi"},
		{"running 200 ms on", running, nil, 200 * time.Millisecond, "zemi"},
		{"asking", func(app *App) { running(app); app.permAsk = &permAskPrompt{} }, nil, 0, "? zemi"},
		{"asking trust", func(app *App) { app.trust = &trustPrompt{root: "/proj"} }, nil, 0, "? zemi"},
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
		{"a shorter stage drops the older section whole", []push{{slog.LevelInfo, "Copied", "12 bytes"}, signIn("jit")}, 0, 30, 4, []string{"┌ Sign-in needed ──────────┐", "│ jit; open /mcp           │", "└──────────────────────────┘"}},
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
