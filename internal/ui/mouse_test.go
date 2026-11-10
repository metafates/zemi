package ui

import (
	"fmt"
	"image/color"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// findCell returns the screen cell where text first appears in the frame.
func findCell(t *testing.T, app *App, text string) (x, y int) {
	t.Helper()
	for i, l := range strings.Split(ansi.Strip(app.Frame()), "\n") {
		if before, _, ok := strings.Cut(l, text); ok {
			return ansi.StringWidth(before), i
		}
	}
	t.Fatalf("%q not on screen:\n%s", text, ansi.Strip(app.Frame()))
	return 0, 0
}

func clickAt(app *App, x, y int) {
	app.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// dragTo presses at (x0, y0), moves to (x1, y1) and releases there; the cmd
// is the release's, which copies a text selection.
func dragTo(app *App, x0, y0, x1, y1 int) tea.Cmd {
	app.Update(tea.MouseClickMsg{X: x0, Y: y0, Button: tea.MouseLeft})
	app.Update(tea.MouseMotionMsg{X: x1, Y: y1, Button: tea.MouseLeft})
	_, cmd := app.Update(tea.MouseReleaseMsg{X: x1, Y: y1, Button: tea.MouseLeft})
	return cmd
}

func TestMouseDragCopies(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 16)
	host.AppendMessage("user", "hello there, world")
	app.renderSync(t)
	x, y := findCell(t, app, "hello there")
	if dragTo(app, x, y, x+10, y) == nil {
		t.Fatal("release after a drag should copy")
	}
	if got := app.selectionText(); got != "hello there" {
		t.Fatalf("selection = %q", got)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.selectionText() != "" {
		t.Fatal("a key should clear the selection")
	}
}

func TestMouseClicks(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	host.AppendMessage("user", "why?")
	appendReasoning(host, "first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); strings.Contains(frame, "first idea") {
		t.Fatalf("a settled thought shows its text before a click:\n%s", frame)
	}
	x, y := findCell(t, app, "thought 1.0s")
	clickAt(app, x+2, y) // the digest of a lone thought opens its view at once
	var id string
	for _, row := range app.lastRows {
		if strings.Contains(hostapi.SegsText(row), "thought 1.0s") {
			for _, s := range row {
				if s.Click != "" {
					id = s.Click
				}
			}
		}
	}
	if id != "call:2" {
		t.Fatalf("the lone thought's digest click = %q, want the call view's", id)
	}

	ran := make(chan string, 1)
	host.SetReload(func() error { ran <- "reload"; return nil }, nil)
	app.lastRows = [][]hostapi.Segment{{{Text: " stuck /reload ", Click: "reload"}}}
	if _, act, ok := app.target(2, 0); ok {
		act()
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("clicking the stuck pill should reload")
	}

	done := make(chan bool)
	go func() {
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "shell", Subject: "tool.shell", Arg: "make deploy"})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	x, y = findCell(t, app, "No ")
	clickAt(app, x+1, y)
	if <-done {
		t.Fatal("clicking No allowed the command")
	}
}

// TestMouseHover: hovering a choice lights it, moving off clears it, and
// hovering a palette row then clicking it fills the composer.
func TestMouseHover(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan bool)
	go func() {
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "shell", Subject: "tool.shell", Arg: "make deploy"})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	x, y := findCell(t, app, "Yes ") // an unselected choice: the selected one draws the band
	app.Update(tea.MouseMotionMsg{X: x + 1, Y: y})
	app.renderSync(t)
	if !strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: "Yes", HL: "Host:Hover", Fill: hostapi.FillPanel}}, 80)) {
		t.Fatal("hovered choice not lit")
	}
	app.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	app.renderSync(t)
	if strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: "No", HL: "Host:Hover", Fill: hostapi.FillPanel}}, 80)) {
		t.Fatal("hover stayed after the pointer left")
	}
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	<-done
	// the answer swallowed the keys behind it (a paste's tail); a human's
	// next key comes after the swallow window
	app.now = func() time.Time { return time.Now().Add(time.Second) }

	if _, err := app.host.Commands.Add(&hostapi.Command{Name: "help", Desc: "show keys", GoFn: func(*hostapi.Host, string, bool) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	app.renderSync(t)
	x, y = findCell(t, app, "/help")
	clickAt(app, x+1, y) // select
	clickAt(app, x+1, y) // open
	if app.composer.text != "/help " {
		t.Fatalf("composer = %q", app.composer.text)
	}
}

// TestDropPaste: pasted paths in the forms terminals use for drag and drop
// become @ words; ordinary text stays text.
func TestDropPaste(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	dir := t.TempDir()
	shot := filepath.Join(dir, "My Shot.png")
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(shot, []byte("\x89PNGfake"), 0o600)
	os.WriteFile(notes, []byte("hello"), 0o600)
	shotWord, notesWord := app.host.AttachWord(shot), app.host.AttachWord(notes)
	for _, c := range []struct{ paste, want string }{
		{strings.ReplaceAll(shot, " ", `\ `) + " " + notes, shotWord + " " + notesWord + " "},
		{"'" + shot + "'", shotWord + " "},
		{"file://" + strings.ReplaceAll(shot, " ", "%20") + "\n", shotWord + " "},
		{"see " + notes, "see " + notes},
		{"/no/such/file.png", "/no/such/file.png"},
	} {
		app.composer.text, app.composer.cursor = "", 0
		app.Update(tea.PasteMsg{Content: c.paste})
		if app.composer.text != c.want {
			t.Errorf("paste %q: composer %q, want %q", c.paste, app.composer.text, c.want)
		}
	}
	// a wide space is no word start for an @ reference: space the token from it
	proj := t.TempDir()
	app.host.Dirs.ProjectDir = proj
	main := filepath.Join(proj, "main.go")
	os.WriteFile(main, []byte("package main"), 0o600)
	app.composer.text, app.composer.cursor = "see\u00a0", 4
	app.Update(tea.PasteMsg{Content: main})
	if want := "see\u00a0 @main.go "; app.composer.text != want {
		t.Errorf("drop after a wide space: composer %q, want %q", app.composer.text, want)
	}
}

// TestClickScrolledListOpensSameItem: the first click selects, and the list
// stays put so the second click opens the same item.
func TestClickScrolledListOpensSameItem(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 30)
	var items []hostapi.OverlayItem
	for i := range 30 {
		items = append(items, hostapi.OverlayItem{Text: fmt.Sprintf("item %02d", i), Fill: fmt.Sprintf("item %02d", i)})
	}
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: items})
	x, y := findCell(t, app, "item 08")
	clickAt(app, x+1, y)
	app.Frame()
	clickAt(app, x+1, y)
	if app.composer.text != "item 08" {
		t.Fatalf("composer = %q", app.composer.text)
	}
}

// TestListWindowEndsAfterWholeItem: a scrolled list shows no item cut in
// half, starts on an item and shows no header without its first item, and
// keeps its height.
func TestListWindowEndsAfterWholeItem(t *testing.T) {
	more := [][]hostapi.Segment{{{Text: "m1"}}, {{Text: "m2"}}, {{Text: "m3"}}}
	for _, c := range []struct {
		name   string
		items  []listItem
		limit  int
		hidden string
	}{
		{"header", []listItem{{label: "a", section: "One"}, {label: "b", section: "One"}, {label: "c", section: "Two"}}, 5, "Two"},
		{"multi-line item", []listItem{{label: "x"}, {more: more}}, 4, "m1"},
	} {
		rows := listRowsW(c.items, c.items, 0, &viewport{}, c.limit, 0, 0)
		if len(rows) != c.limit || strings.Contains(hostapi.SegsText(slices.Concat(rows...)), c.hidden) {
			t.Errorf("%s: %q", c.name, hostapi.SegsText(slices.Concat(rows...)))
		}
	}
	// scrolling down, every window starts on an item's first row
	items := slices.Repeat([]listItem{{label: "item", more: more[:1]}}, 5)
	for _, limit := range []int{4, 6, 7} {
		v := viewport{}
		for sel := range items {
			if rows := listRowsW(items, items, sel, &v, limit, 0, 0); !strings.Contains(hostapi.SegsText(rows[0]), "item") {
				t.Errorf("limit %d, selection %d: window starts on %q", limit, sel, hostapi.SegsText(rows[0]))
			}
		}
	}
}

func TestHoverStyle(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	base := uv.Style{Fg: rgb(0x9098a0), Bg: rgb(0x243427), Attrs: uv.AttrBold}
	for _, c := range []struct {
		name  string
		group string
		spec  *hostapi.HLSpec // nil: the group is not set
		want  uv.Style
	}{
		{
			"a list row keeps the colors its selection and matches draw", "HoverItem", nil,
			uv.Style{Fg: base.Fg, Bg: base.Bg, Attrs: uv.AttrBold, Underline: uv.UnderlineSingle},
		},
		{
			"unset keys keep the base", "Hover", &hostapi.HLSpec{Underline: "curl", Sp: "#ff0000"},
			uv.Style{Fg: base.Fg, Bg: base.Bg, Attrs: uv.AttrBold, Underline: uv.UnderlineCurly, UnderlineColor: rgb(0xff0000)},
		},
		{"false and NONE clear", "HoverItem", &hostapi.HLSpec{Bold: new(false), Bg: "NONE"}, uv.Style{Fg: base.Fg}},
		{"replace drops the base", "Hover", &hostapi.HLSpec{Replace: true, Fg: "#ffffff"}, uv.Style{Fg: rgb(0xffffff)}},
		{
			"brighten mixes the text's fg toward white", "Hover", &hostapi.HLSpec{Brighten: 0.5},
			uv.Style{Fg: hostapi.Mix(base.Fg, color.White, 0.5), Bg: base.Bg, Attrs: uv.AttrBold},
		},
		{
			"brighten darkens text on a lighter fill", "Hover", &hostapi.HLSpec{Brighten: 0.5, Bg: "#ffffff"},
			uv.Style{Fg: hostapi.Mix(base.Fg, color.Black, 0.5), Bg: rgb(0xffffff), Attrs: uv.AttrBold},
		},
		{
			"a theme without the group still underlines", "HoverNope", nil,
			uv.Style{Fg: base.Fg, Bg: base.Bg, Attrs: uv.AttrBold, Underline: uv.UnderlineSingle},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, ok := hostapi.Group(c.group)
			if c.spec != nil {
				spec, ok = *c.spec, true
			}
			if !ok {
				spec = hostapi.HLSpec{Underline: hostapi.UnderlineSingle}
			}
			if got := app.thm.hoverSpec(base, spec); !got.Equal(&c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSelectionRejoinsWrappedText: a drag over a wrapped user message, a
// wrapped code block and a wrapped notice copies the text the model and the
// user wrote, without the gutter bar, the bullets, the wrap points or the
// notice's hint.
func TestSelectionRejoinsWrappedText(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 44, 24)
	prompt := "please rewrite this helper so it stops allocating a slice on every call"
	code := "func add(a, b int) int { return a + b + 1000000 }"
	recap := "recap: fixing the build; next run make check"
	host.AppendMessage("user", prompt)
	host.AppendMessage("assistant", "here:\n\n```go\n"+code+"\n```\n")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowNotice, Text: recap, Detail: "turn off in /config"})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "(turn off in /config)") {
		t.Fatalf("the notice hint is not drawn:\n%s", frame)
	}
	for _, c := range []struct{ from, to, want string }{
		{"please", "every call", prompt},
		{"func add", "1000000", code},
		{"recap:", "/config)", recap},
	} {
		_, y0 := findCell(t, app, c.from)
		_, y1 := findCell(t, app, c.to)
		if y1 <= y0 {
			t.Fatalf("%q did not wrap", c.from)
		}
		dragTo(app, 0, y0, app.width-1, y1)
		if got := app.selectionText(); got != c.want {
			t.Errorf("drag from %q: %q, want %q", c.from, got, c.want)
		}
	}
}

// TestMouseQuestion: the question overlay answers to the mouse like the
// permission prompt - a click on an option picks it, one on the field
// focuses it, one on a Submit tab row opens its question, and one on the
// button submits.
func TestMouseQuestion(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	opts := func(labels ...string) []tools.QuestionOption {
		var o []tools.QuestionOption
		for _, l := range labels {
			o = append(o, tools.QuestionOption{Label: l})
		}
		return o
	}
	done := askAsync(t, app,
		tools.Question{Type: tools.TypeSingle, Question: "apply the proposals?", Header: "Apply", Options: opts("apply", "skip")},
		tools.Question{Type: tools.TypeSingle, Question: "keep the flag?", Header: "Flag", Options: opts("keep", "drop")},
	)
	click := func(text string) {
		app.renderSync(t)
		x, y := findCell(t, app, text)
		clickAt(app, x+1, y)
	}
	click("skip")
	click("add one")
	pressKeys(app, "why", "enter")
	click("[1] Apply")
	if app.question.at != 0 {
		t.Fatalf("a summary row should open its question, at %d", app.question.at)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	click("Submit answers")
	if r := awaitAnswer(t, app, done); !r.ok || fmt.Sprint(r.answers) != "[{[skip]  } {[keep]  why}]" {
		t.Fatalf("answered %+v ok=%v", r.answers, r.ok)
	}
}

// TestToolRowClickOpens: in auto mode the command a tool ran is only on its
// clipped row, so a click opens a view of that one row: the command whole,
// its output under it, and the transcript around it untouched. A thought's
// row opens its own text the same way.
func TestToolRowClickOpens(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	reloadUI(t, host) // the chat's rows run commands, so the boot has to have them
	appendReasoning(host, "first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	long := "go test ./internal/hostapi -run TestSomethingWithAVeryLongName -count=1 -race"
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "1", Name: "shell", Label: "run the hostapi tests", Input: long, Status: "ok", Output: "ok  github.com/metafates/zemi/internal/hostapi"},
		{ID: "2", Name: "read", Input: "a.go", Status: "ok"},
		{ID: "3", Name: "read", Input: "b.go", Status: "ok"},
	}})
	app.renderSync(t)
	frame := func() string { return ansi.Strip(app.Frame()) }
	if strings.Contains(frame(), "-race") {
		t.Fatal("the collapsed row should not show the command")
	}
	x, y := findCell(t, app, "run the hostapi tests")
	clickAt(app, x+1, y)
	waitPrompt(t, app, func() bool { return strings.Contains(frame(), "┌ Command") })
	if got := frame(); !strings.Contains(got, "-race") || !strings.Contains(got, "ok  github.com/metafates") {
		t.Fatalf("the view should hold the whole command and its output:\n%s", got)
	}
	// the row under the box kept its line: nothing expanded in the rail
	if rows := strings.Join(chatRows(t, app, 78), "\n"); !strings.Contains(rows, "run the hostapi tests") || strings.Contains(rows, "-race") {
		t.Fatalf("the row expanded in the rail:\n%s", rows)
	}
	// esc goes back, and a thought's row opens its own text
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	app.renderSync(t)
	x, y = findCell(t, app, "thought 1.0s")
	clickAt(app, x+1, y)
	waitPrompt(t, app, func() bool { return strings.Contains(frame(), "┌ Thought") })
	if got := frame(); !strings.Contains(got, "first idea") || strings.Contains(got, "-race") {
		t.Fatalf("the thought's view should hold its text alone:\n%s", got)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	app.renderSync(t)
	// an opened step's calls are rows of their own: hovering one lights it alone
	x, y = findCell(t, app, "read 2 files")
	clickAt(app, x+1, y)
	app.renderSync(t)
	_, first := findCell(t, app, "read a.go")
	x, y = findCell(t, app, "read b.go")
	app.Update(tea.MouseMotionMsg{X: x + 1, Y: y})
	app.renderSync(t)
	app.Frame()
	lit := func(row []hostapi.Segment) bool {
		return slices.ContainsFunc(row, func(s hostapi.Segment) bool { return s.Over == "Hover" })
	}
	if !lit(app.lastRows[y]) || lit(app.lastRows[first]) {
		t.Fatalf("hovering the second call lights %v, the first %v", lit(app.lastRows[y]), lit(app.lastRows[first]))
	}
}

// TestScrollbarDrag: a box's bar drags like the chat's, to either end, and
// the chat's own while a popup it does not hold is open.
func TestScrollbarDrag(t *testing.T) {
	for name, c := range map[string]struct {
		open func(*App, *hostapi.Host)
		down bool
		done func(*App) bool
	}{
		"chat": {
			func(_ *App, host *hostapi.Host) { fillChat(host) }, false,
			func(app *App) bool { return app.scrolls[_regionChat].top == 0 },
		},
		"chat beside the palette": {
			func(app *App, host *hostapi.Host) {
				fillChat(host)
				for _, r := range "/he" {
					app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
				}
			}, false,
			func(app *App) bool { return app.scrolls[_regionChat].top == 0 && app.palette.selected == 0 },
		},
		"help list": {
			func(_ *App, host *hostapi.Host) { host.OpenHelp() }, true,
			func(app *App) bool { st := &app.overlays[0]; return st.selected == len(st.visible)-1 },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 80, 24)
			c.open(app, host)
			for app.Pump() {
			}
			app.renderSync(t)
			app.Frame()
			x, top, bottom := -1, -1, -1
			for y, row := range app.lastRows {
				cx := 0
				for _, s := range row {
					if s.Bar {
						x, bottom = cx, y
						if top < 0 {
							top = y
						}
					}
					cx += ansi.StringWidth(s.Text)
				}
			}
			if x < 0 {
				t.Fatal("no scrollbar on screen")
			}
			if c.down {
				dragTo(app, x, top, x, bottom)
			} else {
				dragTo(app, x, bottom, x, top)
			}
			if !c.done(app) {
				t.Fatalf("the drag from row %d to %d did not reach the end", top, bottom)
			}
		})
	}
}

// TestSelectionStaysInItsPane: a drag selects inside the box or pane it
// started in, a drag outside a modal selects nothing, and no drag closes one.
func TestSelectionStaysInItsPane(t *testing.T) {
	for _, c := range []struct {
		name    string
		overlay bool
		// the drag runs from the cell of from, dx columns on, to the cell of
		// to, the screen's last cell for ""
		from, to string
		dx       int
		want     string
	}{
		{"a view over the chat copies its text only", true, "first body", "second body", 0, "first body line\ns"},
		{"a drag past the box's bottom selects to its last row", true, "first body", "", 0, "first body line\nsecond body line\n\ny copy   esc close"},
		{"a press outside a modal selects nothing", true, "chat line 5", "", 0, ""},
		{"a drag out of the box from its last cell keeps it open", true, "close", "", 4, "e"},
		{"a drag from the chat stops at the composer", false, "chat line 11", "typed draft", 0, "chat line 11 behind the box\n\nchat line 12 behind the box"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 100, 24)
			for i := range 12 {
				host.AppendMessage("user", fmt.Sprintf("chat line %d behind the box", i+1))
			}
			app.composer.text = "typed draft"
			app.renderSync(t)
			if c.overlay {
				app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Body: "first body line\nsecond body line"})
			}
			x0, y0 := findCell(t, app, c.from)
			x1, y1 := app.width-1, app.height-1
			if c.to != "" {
				x1, y1 = findCell(t, app, c.to)
			}
			dragTo(app, x0+c.dx, y0, x1, y1)
			if got := app.selectionText(); got != c.want {
				t.Errorf("selection = %q, want %q", got, c.want)
			}
			if open := len(app.overlays) > 0; open != c.overlay {
				t.Errorf("overlay open = %v, a drag closes nothing", open)
			}
			app.Frame()
			for y, row := range app.lastRows {
				x := 0
				for _, s := range row {
					if s.HL == "Host:Selection" && !(uv.Position{X: x, Y: y}).In(app.mouse.clip) {
						t.Fatalf("cell %d,%d outside %v is lit", x, y, app.mouse.clip)
					}
					x += ansi.StringWidth(s.Text)
				}
			}
		})
	}
}

// TestLinkClickOpens: a click on text a reply shows as a link opens it, and
// the row is drawn as a terminal hyperlink so the terminal shows it is one.
func TestLinkClickOpens(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 16)
	url := "https://example.com/docs"
	row := []hostapi.Segment{{Text: "see "}, {Text: "the docs", Link: url}}
	app.lastRows = [][]hostapi.Segment{row}
	var got string
	was := _openURL
	_openURL = func(u string) error { got = u; return nil }
	defer func() { _openURL = was }()
	clickAt(app, 6, 0)
	if got != url {
		t.Fatalf("click opened %q, want %q", got, url)
	}
	out := app.renderSegs(row, 40)
	if !strings.Contains(out, ansi.SetHyperlink(url)+"the docs"+ansi.ResetHyperlink()) {
		t.Fatalf("row is not a hyperlink: %q", out)
	}
	// a row cut to fit closes the sequence it opened
	tight := app.renderSegs([]hostapi.Segment{{Text: "abcdefgh", Link: url}}, 3)
	if !strings.Contains(tight, ansi.SetHyperlink(url)+"abc"+ansi.ResetHyperlink()) {
		t.Fatalf("cut row: %q", tight)
	}
}

// TestLinkClickOpensInOverlay: a link in an overlay's body, a detail view's
// README, opens too; a list row the click lands on still acts first.
func TestLinkClickOpensInOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 16)
	url := "https://example.com/docs"
	app.openOverlay(hostapi.Overlay{
		ID: "d", Title: "Detail", Origin: lua.OriginBuiltin,
		Body: "read https://example.com/docs online",
	})
	app.lastRows = [][]hostapi.Segment{{{Text: "read "}, {Text: "example.com/docs", Link: url}}}
	var got string
	was := _openURL
	_openURL = func(u string) error { got = u; return nil }
	defer func() { _openURL = was }()
	clickAt(app, 6, 0)
	if got != url {
		t.Fatalf("click in an overlay opened %q, want %q", got, url)
	}
}

// TestLinkHoverLights: hover underlines a link's cells on a body row that
// stays quiet otherwise, and leaves the row's text unlit.
func TestLinkHoverLights(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 16)
	url := "https://example.com/docs"
	app.lastRows = [][]hostapi.Segment{{{Text: "see "}, {Text: "the docs", Link: url, NoHover: true}}}
	app.hovering = true
	app.hoverX, app.hoverY = 6, 0
	rows := [][]hostapi.Segment{{{Text: "see "}, {Text: "the docs", Link: url, NoHover: true}}}
	app.highlightHover(rows)
	if rows[0][0].Over != "" {
		t.Errorf("the plain text lit: %q", rows[0][0].Over)
	}
	if rows[0][1].Over != "Hover" {
		t.Errorf("the link did not light: %q", rows[0][1].Over)
	}
}

// TestCopyWarnsWhenEveryLocalToolFails: the Copied toast is optimistic about
// the terminal's own copy, whose outcome cannot be read, so only local tools
// that ran and failed may warn.
func TestCopyWarnsWhenEveryLocalToolFails(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		tools [][]string
		want  bool
	}{
		{"no local tool ran stays optimistic", nil, false},
		{"a tool that worked stays quiet", [][]string{{"true"}}, false},
		{"every local tool failed warns", [][]string{{"false"}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 80, 16)
			app.copyTools = c.tools
			batch, ok := app.copyCmd("x")().(tea.BatchMsg)
			if !ok || len(batch) != 2 {
				t.Fatalf("copyCmd = %v, want a batch of two", ok)
			}
			batch[1]()       // the local tools run off the UI goroutine
			for app.Pump() { // Notify lands on the queue without a program
			}
			if warn := slices.ContainsFunc(app.toasts, func(to toast) bool { return to.level == slog.LevelWarn }); warn != c.want {
				t.Fatalf("warned = %v, want %v (toasts %+v)", warn, c.want, app.toasts)
			}
		})
	}
}
