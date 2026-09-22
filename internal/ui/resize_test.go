package ui

import (
	"fmt"
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

// TestGoldenHelpOverlay pins the help box; enter on a command row closes
// it and fills the composer.
func TestGoldenHelpOverlay(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.Commands.Add(&hostapi.Command{Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1)})
	host.OpenHelp()
	for app.Pump() {
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	st := &app.overlays[0]
	st.selected = len(st.texts) - 1
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(app.overlays) != 0 || app.composer.text != "/model " {
		t.Fatalf("enter on a command row: overlays=%d composer=%q", len(app.overlays), app.composer.text)
	}
}

// TestGoldenOverlayDetail pins a detail view: the wrapped body, then the
// actions as a list.
func TestGoldenOverlayDetail(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.RecordError("mcp gitlab", `mcp: initialize: Post "http://127.0.0.1:1/mcp": dial tcp 127.0.0.1:1: connect: connection refused`)
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
	app, host := bootAgentApp(t, 100, 30)
	for i := range 14 {
		host.Commands.Add(&hostapi.Command{Name: fmt.Sprintf("cmd%02d", i), Desc: strings.Repeat("d", 3+i*3),
			Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1)})
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
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "show me")
	host.AppendTurnResult("# Heading\n\nsome **bold** and `code` with a verylongunbreakableidentifierthatneverfits\n\n```go\nfunc main() { println(\"a long line of code that overflows\") }\n```\n\n- item one\n- item two\n", hostapi.TurnStat{Out: 10})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Tools: []hostapi.StreamTool{
		{Name: "bash", Input: strings.Repeat("ёж ", 60), Status: "run"},
	}})

	// Before any popup covers it: the long tool input stays on one row, cut
	// by character with "…".
	app.renderSync(t)
	var toolRows []string
	for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
		if strings.Contains(row, "ёж") {
			toolRows = append(toolRows, row)
		}
	}
	if len(toolRows) != 1 || !strings.Contains(toolRows[0], "Run ёж") || !strings.HasSuffix(strings.TrimRight(toolRows[0], " "), "…") {
		t.Fatalf("tool row must be one truncated row, got %q", toolRows)
	}
	app.pushToast("warn", "a rather long toast title that must truncate", "line one of the body\nline two which is long enough to need wrapping at small widths")
	host.OpenHelp()
	for app.Pump() {
	}
	if len(app.overlays) != 1 {
		t.Fatal("help overlay did not open")
	}
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.bash", Arg: "rm -rf build && go test ./..."}}

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

// TestResizeStartsNoTick: Init starts the one frame loop; a resize that
// started another would leave both running for good.
func TestResizeStartsNoTick(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	if _, cmd := app.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); cmd != nil {
		if _, tick := cmd().(frameTickMsg); tick {
			t.Fatal("resize started a second frame loop")
		}
	}
}

// TestStaleRenderKeepsNewerLines: a render of an older generation that
// lands after a newer one leaves the newer lines and pending mark alone.
func TestStaleRenderKeepsNewerLines(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.pending = map[string]int64{"w": 7}
	app.Update(widgetsRenderedMsg{{id: "w", lines: [][]hostapi.Segment{{{Text: "new"}}}, gen: 6}})
	app.Update(widgetsRenderedMsg{{id: "w", lines: [][]hostapi.Segment{{{Text: "old"}}}, gen: 5}})
	if got := plainSegs(app.widgetLines("w")[0]); got != "new" || app.pending["w"] != 7 {
		t.Fatalf("lines %q, pending %v", got, app.pending)
	}
}

// TestModeSwitchQuiet: tab-style switches only change the pill, and the
// picker opens at the bottom-left by the statusline instead of mid-chat.
func TestModeSwitchQuiet(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	_ = a.Frame()
	if err := a.host.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	if f := a.Frame(); strings.Contains(f, "safe actions run on their own") {
		t.Fatalf("a switch must not explain itself:\n%s", ansi.Strip(f))
	}
	a.openOverlay(hostapi.Overlay{ID: "mode", Title: "Permission mode", Items: []any{"manual", "auto"}, Anchor: "bottom"})
	rows := strings.Split(ansi.Strip(a.Frame()), "\r\n")
	bottom := -1
	for i, row := range rows {
		if strings.Contains(row, "Permission mode") && !strings.HasPrefix(row, " ┌") {
			t.Fatalf("picker not at the left edge: %q", row)
		}
		if strings.HasPrefix(row, " └") {
			bottom = i
		}
	}
	if bottom != a.stage.Max.Y-1 {
		t.Fatalf("picker ends on row %d, want the last chat row %d", bottom, a.stage.Max.Y-1)
	}
}

// TestProfileSegment: the statusline names a profile other than default.
func TestProfileSegment(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	runLua(t, app, `zemi.agent.profile("terse", {})`)
	for _, c := range []struct {
		profile string
		shown   bool
	}{{"terse", true}, {"default", false}} {
		if err := host.SetProfile(c.profile); err != nil {
			t.Fatal(err)
		}
		app.renderSync(t)
		if got := strings.Contains(ansi.Strip(app.Frame()), "terse"); got != c.shown {
			t.Errorf("profile %s: segment shown = %v", c.profile, got)
		}
	}
}

// TestToastCap: a long error keeps a few rows, drops its stack trace and
// sends the reader to /errors, where the whole text was recorded.
func TestToastCap(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	home, _ := os.UserHomeDir()
	body := "@user:" + home + "/.config/zemi/init.lua:10: " + strings.Repeat("something went wrong ", 12) + "\nstack traceback:\n\t[G]: in function 'error'"
	app.pushToast("error", "Keymap ctrl+t failed", body)
	var rows []string
	for _, r := range toastBox(app.toasts[0], 80) {
		rows = append(rows, strings.TrimSpace(plainSegs(r)))
	}
	if len(rows) != 2+toastRows || !strings.HasPrefix(rows[0], "┌ Keymap ctrl+t failed ─") || !strings.HasPrefix(rows[1], "│ ~/.config/zemi/init.lua:10: something") || !strings.HasPrefix(rows[toastRows], "│ ... full error in /errors") || strings.Contains(strings.Join(rows, "\n"), "traceback") {
		t.Fatalf("toast rows: %q", rows)
	}
	if !host.HasUnseenErrors() {
		t.Fatal("the long error was not recorded")
	}
}

func TestFitSegs(t *testing.T) {
	right := []hostapi.Segment{{Text: "fake/fake-1"}, {Text: " · "}, {Text: "15/200.0k ctx"}, {Text: " · "}, {Text: "8%"}}
	for _, c := range []struct {
		w    int
		want string
	}{
		{40, "fake/fake-1 · 15/200.0k ctx · 8%"},
		{30, "fake/fake-1 · 15/200.0k ctx"},
		{20, "fake/fake-1"},
		{8, "fake/fa…"},
	} {
		if got := plainSegs(fitSegs(right, c.w)); got != c.want {
			t.Errorf("w %d: %q, want %q", c.w, got, c.want)
		}
	}
}
