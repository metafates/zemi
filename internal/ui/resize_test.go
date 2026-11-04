package ui

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
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
	host.Commands.Add(&hostapi.Command{
		Name: "model", Desc: "choose the model", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(*hostapi.Host, string, bool) error { ran = true; return nil },
	})
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

// TestResizeStartsNoTick: Init starts the one frame loop; a resize that
// started another would leave both running for good.
func TestResizeStartsNoTick(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	_, cmd := app.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, msg := range runCmd(cmd) {
		if _, tick := msg.(frameTickMsg); tick {
			t.Fatal("resize started a second frame loop")
		}
	}
}

// TestStaleRenderKeepsNewerLines: a render dispatched earlier that lands
// after a newer one, of the same generation at another size and failing
// alike, leaves the newer lines and the pending mark alone.
func TestStaleRenderKeepsNewerLines(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.pending = map[string]renderKey{"w": {gen: 8}}
	app.Update(widgetsRenderedMsg{{id: "w", lines: [][]hostapi.Segment{{{Text: "new"}}}, height: 19, gen: 7, seq: 2}})
	app.Update(widgetsRenderedMsg{{id: "w", lines: [][]hostapi.Segment{{{Text: "old"}}}, height: 20, gen: 7, seq: 1}})
	app.Update(widgetsRenderedMsg{{id: "w", height: 20, gen: 7, seq: 1, err: errors.New("old")}})
	if c := app.caches["w"]; hostapi.SegsText(c.lines[0]) != "new" || c.height != 19 || c.err != "" || app.pending["w"].gen != 8 {
		t.Fatalf("cache %+v, pending %v", c, app.pending)
	}
}

// TestAnchoredPickerComposerLine: a picker opens by the statusline instead
// of mid-chat, its border in line with the composer frame on the side of the
// pill that opened it, standing on the frame's top rule.
func TestAnchoredPickerComposerLine(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	_ = a.Frame()
	for _, anchor := range []hostapi.OverlayAnchor{hostapi.OverlayBottomLeft, hostapi.OverlayBottomRight} {
		a.openOverlay(hostapi.Overlay{ID: "pick", Title: "Picker", Items: []hostapi.OverlayItem{{Text: "manual"}, {Text: "allow"}}, Anchor: anchor})
		rows := strings.Split(ansi.Strip(a.Frame()), "\r\n")
		edge := func(row string) bool {
			if anchor == hostapi.OverlayBottomLeft {
				return strings.HasPrefix(row, "┌ Picker") || strings.HasPrefix(row, "├─")
			}
			return strings.HasSuffix(row, "┐") || strings.HasSuffix(row, "┤")
		}
		top := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, "Picker") })
		c := a.layout(100, 16).composer
		if top < 0 || !edge(rows[top]) || !edge(rows[c.Min.Y]) {
			t.Fatalf("%s: picker rows %d and %d not in line with the composer frame:\n%s", anchor, top, c.Min.Y, strings.Join(rows, "\n"))
		}
		// the corner it stands on is its own border on its panel, not the dimmed backdrop
		x := c.Min.X
		if anchor == hostapi.OverlayBottomRight {
			x += c.Dx() - 1
		}
		if s := dropCells(a.lastRows[c.Min.Y], x)[0]; s.HL != hostapi.ChromeRule || s.Fill != hostapi.FillPanel {
			t.Errorf("%s: its corner is %q %s on fill %d", anchor, s.Text, s.HL, s.Fill)
		}
		a.closeOverlayByID("pick")
	}
}

// TestAnchoredBoxBottomBorder: the rule an anchored box stands on carries the
// working rule's key and clock, and the cells the box covers are its own
// bottom border: no backdrop text cuts it.
func TestAnchoredBoxBottomBorder(t *testing.T) {
	t.Parallel()
	for _, anchor := range []hostapi.OverlayAnchor{hostapi.OverlayBottomLeft, hostapi.OverlayBottomRight} {
		app, host := bootAgentApp(t, 100, 20)
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
		app.renderSync(t)
		if label := ansi.Strip(app.Frame()); !strings.Contains(label, "esc interrupt  0m 20s") {
			t.Fatalf("%s: no working key and clock on the rule to cut:\n%s", anchor, label)
		}
		app.openOverlay(hostapi.Overlay{ID: "usage", Title: "Usage", Items: []hostapi.OverlayItem{{Text: "row one"}}, Anchor: anchor})
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		r := app.chromeRect
		mid := truncateSegs(dropCells(app.lastRows[r.Max.Y-1], r.Min.X), r.Dx())
		if hostapi.SegsWidth(mid) != r.Dx() {
			t.Fatalf("%s: the box's bottom row is %d cells, want %d:\n%s", anchor, hostapi.SegsWidth(mid), r.Dx(), frame)
		}
		for _, s := range mid {
			if !s.Line || s.HL != hostapi.ChromeRule || s.Fill != hostapi.FillPanel || s.Fade != 0 {
				t.Fatalf("%s: the box's bottom border is %+v:\n%s", anchor, s, frame)
			}
			for _, ch := range s.Text {
				if armsOf(ch) == 0 {
					t.Fatalf("%s: %q cuts the box's bottom border:\n%s", anchor, ch, frame)
				}
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
	for _, r := range toastTray(app.toasts, 80, 24) {
		rows = append(rows, strings.TrimSpace(hostapi.SegsText(r)))
	}
	if len(rows) != 2+_toastRows || !strings.HasPrefix(rows[0], "┌ Could not run ctrl+t ─") || !strings.HasPrefix(rows[1], "│ ~/.config/zemi/init.lua:10: something") || !strings.HasPrefix(rows[_toastRows], "│ … 5 more lines /errors ") || strings.Contains(strings.Join(rows, "\n"), "traceback") {
		t.Fatalf("toast rows: %q", rows)
	}
	app.pushToast(slog.LevelWarn, "", "line one\nline two\nline three\nline four\nline five")
	if r := hostapi.SegsText(toastTray(app.toasts[1:], 80, 24)[_toastRows]); !strings.HasPrefix(strings.TrimSpace(r), "│ … 3 more lines ") || strings.Contains(r, "/errors") {
		t.Fatalf("multi-line cut: %q", r)
	}
	// an untitled failure still says it is one
	app.pushToast(slog.LevelError, "", "x")
	if top := hostapi.SegsText(toastTray(app.toasts[2:], 80, 24)[0]); !strings.HasPrefix(top, "┌ Error ─") {
		t.Fatalf("untitled error toast: %q", top)
	}
	for _, c := range []struct {
		title, body string
		rows        int
	}{{"Config reloaded", "", 3}, {"", "saved", 3}, {"Session exported", "saved to x", 3}} {
		app.pushToast(slog.LevelInfo, c.title, c.body)
		box := toastTray(app.toasts[len(app.toasts)-1:], 80, 24)
		lone := c.title == "" || c.body == ""
		if inner := hostapi.SegsText(box[1]); len(box) != c.rows || !strings.Contains(inner, cmp.Or(c.body, c.title)) || lone != strings.HasPrefix(hostapi.SegsText(box[0]), "┌─") {
			t.Errorf("toast %q %q: %d rows, %q; want %d rows with the lone text inside", c.title, c.body, len(box), inner, c.rows)
		}
	}
}

// TestFitClusters: a short statusline drops the highest priority widget
// first, whichever side it is on, and the lasting ones stay outermost.
func TestFitClusters(t *testing.T) {
	item := func(text string, pri float64) statusItem { return statusItem{[]hostapi.Segment{{Text: text}}, pri} }
	left := []statusItem{item("auto", 5), item("proj", 10), item("ctrl+c again to quit", 110)}
	right := []statusItem{item("fake/m", 8), item("96%", 40), item("1.1k/128k ctx", 41), item("$0.12", 42)}
	for _, c := range []struct {
		w    int
		want string
	}{
		{80, "auto  proj  ctrl+c again to quit|$0.12  1.1k/128k ctx  96%  fake/m"},
		{45, "auto  proj|$0.12  1.1k/128k ctx  96%  fake/m"},
		{20, "auto  proj|fake/m"},
		{15, "auto|fake/m"},
		{8, "auto|"},
		{3, "au…|"},
	} {
		l, r := fitClusters(left, right, c.w)
		if got := hostapi.SegsText(l) + "|" + hostapi.SegsText(r); got != c.want {
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
		{"window and use in the billions", hostapi.StreamSnapshot{SessionIn: 90000, LastIn: 1200000000, CtxWindow: 1800000000}, "1.2B/1.8B ctx", " tok"},
		{"total in the billions", hostapi.StreamSnapshot{SessionIn: 2000000000}, "2B tok", "ctx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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
