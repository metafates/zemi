package ui

import (
	"fmt"
	"os"
	"regexp"
	"slices"
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
// it and runs the command.
func TestGoldenHelpOverlay(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	ran := false
	host.Commands.Add(&hostapi.Command{Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(*hostapi.Host, string) error { ran = true; return nil }})
	host.OpenHelp()
	for app.Pump() {
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	st := &app.overlays[0]
	st.selected = len(st.texts) - 1
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(app.overlays) != 0 || !ran {
		t.Fatalf("enter on a command row: overlays=%d ran=%v", len(app.overlays), ran)
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
	appendReply(host, "# Heading\n\nsome **bold** and `code` with a verylongunbreakableidentifierthatneverfits\n\n```go\nfunc main() { println(\"a long line of code that overflows\") }\n```\n\n- item one\n- item two\n", hostapi.TurnStat{Out: 10})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Tools: []hostapi.StreamTool{
		{Name: "bash", Input: strings.Repeat("ёж ", 60), Status: "run"},
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

// TestModeSwitchQuiet: a mode switch only changes the pill, and a
// picker opens by the statusline instead of mid-chat, its border in line
// with the composer frame on the side of the pill that opened it.
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
	for _, anchor := range []string{"bottom-left", "bottom-right"} {
		a.openOverlay(hostapi.Overlay{ID: "mode", Title: "Permission mode", Items: []any{"manual", "auto"}, Anchor: anchor})
		rows := strings.Split(ansi.Strip(a.Frame()), "\r\n")
		edge := func(row string) bool {
			if anchor == "bottom-left" {
				return strings.HasPrefix(row, "┌ Permission mode") || strings.HasPrefix(row, "└─")
			}
			return strings.HasSuffix(row, "┐") || strings.HasSuffix(row, "─┘")
		}
		top := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, "Permission mode") })
		if last := a.stage.Max.Y - 1; top < 0 || !edge(rows[top]) || !edge(rows[last]) {
			t.Fatalf("%s: picker rows %d and %d not in line with the composer frame:\n%s", anchor, top, last, strings.Join(rows, "\n"))
		}
		a.closeOverlayByID("mode")
	}
}

// TestProfileSegment: the statusline names a profile other than default.
func TestProfileSegment(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	runLua(t, app, `zemi.agent.profile.add("terse", {})`)
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

// TestToastCap: a long error keeps a few rows, drops its stack trace, says
// how much it cut and sends the reader to /errors when its caller recorded
// it there; a toast records nothing itself.
func TestToastCap(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	home, _ := os.UserHomeDir()
	body := "@user:" + home + "/.config/zemi/init.lua:10: " + strings.Repeat("something went wrong ", 12) + "\nstack traceback:\n\t[G]: in function 'error'"
	host.RecordError("keymap ctrl+t", body)
	app.pushToast("error", "Could not run ctrl+t", body)
	var rows []string
	for _, r := range toastBox(app.toasts[0], 80) {
		rows = append(rows, strings.TrimSpace(plainSegs(r)))
	}
	if len(rows) != 2+toastRows || !strings.HasPrefix(rows[0], "┌ Could not run ctrl+t ─") || !strings.HasPrefix(rows[1], "│ ~/.config/zemi/init.lua:10: something") || !strings.HasPrefix(rows[toastRows], "│ … 4 more lines /errors ") || strings.Contains(strings.Join(rows, "\n"), "traceback") {
		t.Fatalf("toast rows: %q", rows)
	}
	app.pushToast("warn", "", "line one\nline two\nline three\nline four\nline five")
	if r := plainSegs(toastBox(app.toasts[1], 80)[toastRows]); !strings.HasPrefix(strings.TrimSpace(r), "│ … 3 more lines ") || strings.Contains(r, "/errors") {
		t.Fatalf("multi-line cut: %q", r)
	}
	// an untitled failure still says it is one
	app.pushToast("error", "", "x")
	if top := plainSegs(toastBox(app.toasts[2], 80)[0]); !strings.HasPrefix(top, "┌ Error ─") {
		t.Fatalf("untitled error toast: %q", top)
	}
}

// TestFitClusters: a short statusline drops the highest priority widget
// first, whichever side it is on, and the lasting ones stay outermost.
func TestFitClusters(t *testing.T) {
	item := func(text string, pri int) statusItem { return statusItem{[]hostapi.Segment{{Text: text}}, pri} }
	left := []statusItem{item("auto", 5), item("proj", 10), item("viewing explore · esc back", 120)}
	right := []statusItem{item("fake/m", 8), item("96%", 40), item("1.1k/128k ctx", 41), item("$0.12", 42)}
	for _, c := range []struct {
		w    int
		want string
	}{
		{80, "auto · proj · viewing explore · esc back|$0.12 · 1.1k/128k ctx · 96% · fake/m"},
		{45, "auto · proj|1.1k/128k ctx · 96% · fake/m"},
		{20, "auto · proj|fake/m"},
		{15, "auto|fake/m"},
		{8, "auto|"},
		{3, "au…|"},
	} {
		l, r := fitClusters(left, right, c.w)
		if got := plainSegs(l) + "|" + plainSegs(r); got != c.want {
			t.Errorf("w %d: %q, want %q", c.w, got, c.want)
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
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, 100, 12)
			host.SeedStream(c.s)
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			if !strings.Contains(frame, c.want) || strings.Contains(frame, c.skip) {
				t.Fatalf("want %q and no %q:\n%s", c.want, c.skip, frame)
			}
		})
	}
}
