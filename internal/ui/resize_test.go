package ui

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestGoldenOverlayDetail pins a detail view: the wrapped body, then the
// actions as a list.
func TestGoldenOverlayDetail(t *testing.T) {
	app, host := bootAgentAppPinned(t, 80, 24)
	host.RecordError("mcp", "gitlab", `mcp: initialize: Post "http://127.0.0.1:1/mcp": dial tcp 127.0.0.1:1: connect: connection refused`)
	if err := host.OpenErrors(""); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.renderSync(t)
	boxRow := func() string { return regexp.MustCompile(`┌.*┐`).FindString(ansi.Strip(app.Frame())) }
	list := boxRow()
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	app.renderSync(t)
	if detail := boxRow(); utf8.RuneCountInString(detail) != utf8.RuneCountInString(list) {
		t.Errorf("the detail view jumps in width:\n%s\n%s", list, detail)
	}
	golden.RequireEqual(t, regexp.MustCompile(`\d\d:\d\d:\d\d`).ReplaceAllString(app.Frame(), "12:00:00"))

	// a detail view shorter than its list replaces it rather than sitting on it
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if err := host.OpenHealth(); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 })
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	app.renderSync(t)
	// the composer frame's own rule starts a row; every overlay box is indented
	frame, _, _ := strings.Cut(ansi.Strip(app.Frame()), "\r\n┌")
	if strings.Count(frame, "┌") != 1 {
		t.Fatalf("the list shows around its detail view:\n%s", frame)
	}
}

// TestOverlayWidthStableWhileScrolling: a list longer than its window
// keeps one box width as the selection scrolls past rows of other widths.
func TestOverlayWidthStableWhileScrolling(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	for i := range 14 {
		host.Commands.Add(&hostapi.Command{
			Name: fmt.Sprintf("cmd%02d", i), Desc: strings.Repeat("d", 3+i*3),
			Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		})
	}
	host.OpenHelp()
	for app.Pump() {
	}
	boxWidth := func() int {
		app.renderSync(t)
		for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
			if strings.Contains(row, "┌") {
				return ansi.StringWidth(strings.TrimSpace(row))
			}
		}
		t.Fatal("no box on screen")
		return 0
	}
	want := boxWidth()
	for range len(app.overlays[0].texts) {
		app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		if got := boxWidth(); got != want {
			t.Fatalf("box width changed while scrolling: %d -> %d", want, got)
		}
	}
}

// TestResizeSweep resizes one busy frame (markdown reply, multi-line toast,
// permission box) through degenerate and normal sizes; no row may ever be
// wider than the terminal.
func TestResizeSweep(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "show me")
	appendReply(host, "# Heading\n\nsome **bold** and `code` with a verylongunbreakableidentifierthatneverfits\n\n```go\nfunc main() { println(\"a long line of code that overflows\") }\n```\n\n- item one\n- item two\n", hostapi.TurnStat{Out: 10})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Tools: []hostapi.StreamTool{
		{Name: "shell", Input: strings.Repeat("ёж ", 60), Status: "run"},
	}})

	// Before any popup covers it: the long tool input stays on one row, cut
	// by character with "…".
	app.renderSync(t)
	var toolRows []string
	for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
		if strings.Contains(row, "ёж") && !strings.HasPrefix(row, "┌") { // the rule names the call too
			toolRows = append(toolRows, row)
		}
	}
	if len(toolRows) != 1 || !strings.Contains(toolRows[0], " run ёж") || !strings.HasSuffix(strings.TrimRight(toolRows[0], " "), "…") {
		t.Fatalf("tool row must be one truncated row, got %q", toolRows)
	}
	app.pushToast(slog.LevelWarn, "a rather long toast title that must truncate", "line one of the body\nline two which is long enough to need wrapping at small widths")
	host.OpenHelp()
	for app.Pump() {
	}
	if len(app.overlays) != 1 {
		t.Fatal("help overlay did not open")
	}
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.shell", Arg: "rm -rf build && go test ./..."}}

	sizes := []struct{ w, h int }{
		{1, 1}, {2, 3}, {5, 2}, {12, 6}, {20, 10}, {40, 8}, {80, 24}, {33, 50}, {200, 60}, {7, 40},
	}
	for _, sz := range sizes {
		app.Update(tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		app.renderSync(t)
		frame := app.Frame()
		if !utf8.ValidString(frame) {
			t.Fatalf("%dx%d: frame is not valid UTF-8", sz.w, sz.h)
		}
		for i, row := range strings.Split(frame, "\r\n") {
			if n := ansi.StringWidth(row); n > sz.w {
				t.Fatalf("%dx%d: row %d is %d cells wide: %q", sz.w, sz.h, i, n, ansi.Strip(row))
			}
		}
	}
}

// TestToastCap: a long error keeps a few rows, drops its stack trace, says
// how much it cut and sends the reader to /errors when its caller recorded
// it there; a toast records nothing itself. A lone title or body sits inside the box.
func TestToastCap(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	home, _ := os.UserHomeDir()
	body := "@user:" + home + "/.config/zemi/init.lua:10: " + strings.Repeat("something went wrong ", 12) + "\nstack traceback:\n\t[G]: in function 'error'"
	host.RecordError("keymap", "ctrl+t", body)
	app.pushToast(slog.LevelError, "Could not run ctrl+t", body)
	var rows []string
	tray := toastTray(app.toasts, 80, 24, time.Now())
	for _, r := range tray {
		rows = append(rows, strings.TrimSpace(hostapi.SegsText(r)))
	}
	if len(rows) != 2+_toastRows || !strings.HasPrefix(rows[0], "┌ Could not run ctrl+t ─") || !strings.HasPrefix(rows[1], "│ ~/.config/zemi/init.lua:10: something") || !strings.HasPrefix(rows[_toastRows], "│ … 5 more lines /errors ") || strings.Contains(strings.Join(rows, "\n"), "traceback") {
		t.Fatalf("toast rows: %q", rows)
	}
	app.pushToast(slog.LevelWarn, "", "line one\nline two\nline three\nline four\nline five")
	if r := hostapi.SegsText(func() []hostapi.Segment {
		tt := toastTray(app.toasts[1:], 80, 24, time.Now())
		return tt[_toastRows]
	}()); !strings.HasPrefix(strings.TrimSpace(r), "│ … 3 more lines ") || strings.Contains(r, "/errors") {
		t.Fatalf("multi-line cut: %q", r)
	}
	// an untitled failure still says it is one
	app.pushToast(slog.LevelError, "", "x")
	if top := hostapi.SegsText(func() []hostapi.Segment { tt := toastTray(app.toasts[2:], 80, 24, time.Now()); return tt[0] }()); !strings.HasPrefix(top, "┌ Error ─") {
		t.Fatalf("untitled error toast: %q", top)
	}
	for _, c := range []struct {
		title, body string
		rows        int
	}{{"Config reloaded", "", 3}, {"", "saved", 3}, {"Session exported", "saved to x", 3}} {
		app.pushToast(slog.LevelInfo, c.title, c.body)
		box := toastTray(app.toasts[len(app.toasts)-1:], 80, 24, time.Now())
		lone := c.title == "" || c.body == ""
		if inner := hostapi.SegsText(box[1]); len(box) != c.rows || !strings.Contains(inner, cmp.Or(c.body, c.title)) || lone != strings.HasPrefix(hostapi.SegsText(box[0]), "┌─") {
			t.Errorf("toast %q %q: %d rows, %q; want %d rows with the lone text inside", c.title, c.body, len(box), inner, c.rows)
		}
	}
}

// TestContextMeter: the meter reads the window in use, and a total of the
// session's tokens stands in only when the model's window is unknown.
func TestContextMeter(t *testing.T) {
	for _, c := range []struct {
		name       string
		s          hostapi.StreamSnapshot
		want, skip string
	}{
		{"window and use known", hostapi.StreamSnapshot{SessionIn: 90000, LastIn: 12300, CtxWindow: 64000}, "12.3k/64k ctx", " tok"},
		{"use not known yet", hostapi.StreamSnapshot{SessionIn: 90000, CtxWindow: 64000}, "", " tok"},
		{"window unknown", hostapi.StreamSnapshot{SessionIn: 90000, LastIn: 12300}, "90k tok", "ctx"},
		{"window and use in the billions", hostapi.StreamSnapshot{SessionIn: 90000, LastIn: 1200000000, CtxWindow: 1800000000}, "1.2B/1.8B ctx", " tok"},
		{"total in the billions", hostapi.StreamSnapshot{SessionIn: 2000000000}, "2B tok", "ctx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 100, 12)
			app.SignalBoot(nil) // the meter waits out the boot
			// a known windowless model: the tok fallback is its readout, and a
			// spec-less boot (no provider yet) shows nothing at all
			runLua(t, app, `zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })`)
			host.SeedStream(c.s)
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			if !strings.Contains(frame, c.want) || strings.Contains(frame, c.skip) {
				t.Fatalf("want %q and no %q:\n%s", c.want, c.skip, frame)
			}
		})
	}
}
