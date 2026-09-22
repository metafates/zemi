package ui

import (
	"context"
	"fmt"
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
	app, host := bootAgentApp(t, 80, 20)
	host.AppendMessage("user", "why?")
	host.AppendReasoning("first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	app.renderSync(t)
	x, y := findCell(t, app, "Thought for")
	clickAt(app, x+1, y)
	app.renderSync(t)
	if !strings.Contains(ansi.Strip(app.Frame()), "│ first idea") {
		t.Fatal("clicking the thought line should expand it")
	}

	ran := make(chan string, 1)
	host.Commands.Add(&hostapi.Command{Name: "mode", GoFn: func(*hostapi.Host, string) error { ran <- "mode"; return nil }, Origin: lua.OriginBuiltin})
	x, y = findCell(t, app, "auto")
	clickAt(app, x+1, y)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("clicking the mode pill should run /mode")
	}

	app.reload = func() { ran <- "reload" }
	app.lastRows = [][]hostapi.Segment{{{Text: " stuck: /reload ", Click: "reload"}}}
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
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"})
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
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan bool)
	go func() {
		ok, _ := app.AskPermission(hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	x, y := findCell(t, app, "No ")
	app.Update(tea.MouseMotionMsg{X: x + 1, Y: y})
	app.renderSync(t)
	if !strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: "No", HL: "Host:Hover"}}, 80)) {
		t.Fatal("hovered choice not lit")
	}
	app.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	app.renderSync(t)
	if strings.Contains(app.Frame(), app.renderSegs([]hostapi.Segment{{Text: "No", HL: "Host:Hover"}}, 80)) {
		t.Fatal("hover stayed after the pointer left")
	}
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	<-done

	if _, err := app.host.Commands.Add(&hostapi.Command{Name: "help", Desc: "show keys", GoFn: func(*hostapi.Host, string) error { return nil }}); err != nil {
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
// become attachments; ordinary text stays text.
func TestDropPaste(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	dir := t.TempDir()
	shot := filepath.Join(dir, "My Shot.png")
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(shot, []byte("\x89PNGfake"), 0o600)
	os.WriteFile(notes, []byte("hello"), 0o600)
	for _, c := range []struct{ paste, want string }{
		{strings.ReplaceAll(shot, " ", `\ `) + " " + notes, "@drop:My-Shot.png @drop:notes.txt "},
		{"'" + shot + "'", "@drop:My-Shot.png "},
		{"file://" + strings.ReplaceAll(shot, " ", "%20") + "\n", "@drop:My-Shot.png "},
		{"see " + notes, "see " + notes},
		{"/no/such/file.png", "/no/such/file.png"},
	} {
		app.composer.text, app.composer.cursor = "", 0
		app.Update(tea.PasteMsg{Content: c.paste})
		if app.composer.text != c.want {
			t.Errorf("paste %q: composer %q, want %q", c.paste, app.composer.text, c.want)
		}
	}
}

func TestComposerCursorColumn(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 12)
	app.composer.text, app.composer.cursor = "hello world", 2
	if segs := app.composerRows(60)[0]; segs[1].Text != "he" || segs[2].Text != "l" || segs[2].HL != "Host:Cursor" {
		t.Fatalf("block cursor not on its column: %+v", segs)
	}
}

func TestFilterKeys(t *testing.T) {
	for _, c := range []struct{ query, key, want string }{
		{"foo bar", "ctrl+w", "foo "},
		{"foo bar", "alt+backspace", "foo "},
		{"foo bar", "ctrl+u", ""},
		{"foo", "backspace", "fo"},
	} {
		f := listFilter{active: true, query: c.query}
		if !f.edit(c.key, "", []string{"a"}) || f.query != c.want {
			t.Errorf("%q %s -> %q, want %q", c.query, c.key, f.query, c.want)
		}
	}
}

// TestHoverTargets: hover and click act on whole items, and only with the
// pointer over the item's own cells.
func TestHoverTargets(t *testing.T) {
	long := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau"
	memories := func(app *App) {
		app.memReview = &memReviewPrompt{items: []hostapi.MemoryRow{
			{ID: "a", Scope: "user", Kind: "fact", Origin: "user", Text: "short one", Queued: true},
			{ID: "b", Scope: "user", Kind: "fact", Origin: "user", Text: long},
		}}
	}
	overlay := func(app *App) {
		app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"beta and a much longer row", "alpha"}})
	}
	keyed := func(app *App) {
		app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Origin: lua.OriginBuiltin, Items: []any{"a much longer row than the other", map[string]any{"text": "alpha", "key": "current"}}})
	}
	perm := func(app *App) {
		app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "make deploy"}, answer: make(chan permAnswer, 1)}
	}
	thought := func(app *App) {
		app.host.AppendMessage("user", "why?")
		app.host.AppendReasoning("first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	}
	widgets := func(app *App) {
		src := `zemi.ui.widget.add({ id = "a", region = "statusline.left", priority = 90, on_click = function() end,
			render = function() return {{{ text = "clickme", click = "go" }}} end })
		zemi.ui.widget.add({ id = "b", region = "statusline.left", priority = 91,
			render = function() return {{{ text = "noclick", click = "go" }}} end })
		zemi.ui.widget.add({ id = "c", region = "statusline.left", priority = 89, on_click = function() end,
			render = function() return {{{ text = "quiet", hl = "Comment", click = "go", hover = false }}} end })`
		if err := app.host.RT.ExecuteChunk(context.Background(), src, "w.lua", lua.OriginUser); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		open func(app *App)
		text string // hovered text
		dx   int    // cell offset from where text starts
		rows int    // rows lit, 0 = not a target
		lit  string // the hover group, or the fixed prompt style
	}{
		{"memory text row lights the whole item", memories, "sigma", 1, 3, "HoverItem"},
		{"right of memory text is not a target", memories, "sigma", 40, 0, ""},
		{"the selected memory is not a target", memories, "short one", 1, 0, ""},
		{"memory section header is not a target", memories, "Remembered", 1, 0, ""},
		{"overlay row text is a target", overlay, "alpha", 1, 1, "HoverItem"},
		{"overlay row padding is not a target", overlay, "alpha", 6, 0, ""},
		{"overlay gutter is not a target", overlay, "beta and", -1, 0, ""},
		{"gap before the key column is not a target", keyed, "current", -3, 0, ""},
		{"the key column lights its item", keyed, "current", 1, 1, "HoverItem"},
		{"a prompt choice keeps the fixed hover", perm, "No ", 0, 1, "Host:Hover"},
		{"a choice's key button answers it too", perm, "No ", -3, 1, "Host:Hover"},
		{"the padding right of a choice is not a target", perm, "No ", 30, 0, ""},
		// a chat body row acts on a click and keeps its own groups
		{"thought line text is a target", thought, "Thought for", 1, 1, ""},
		{"a quiet click id takes no hover style", widgets, "quiet", 1, 1, ""},
		{"right of the thought line is not a target", thought, "Thought for", 60, 0, ""},
		{"a widget click id with a handler is a target", widgets, "clickme", 1, 1, "Hover"},
		{"a widget click id without a handler is not", widgets, "noclick", 1, 0, ""},
		{"the space right of a widget click id is not", widgets, "clickme", 7, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 80, 24)
			c.open(app)
			app.renderSync(t)
			x, y := findCell(t, app, c.text)
			app.Update(tea.MouseMotionMsg{X: x + c.dx, Y: y})
			app.renderSync(t)
			app.Frame()
			spans, _, ok := app.target(x+c.dx, y)
			if !ok {
				spans = nil
			}
			lit := map[int]bool{}
			for _, sp := range spans {
				lit[sp.y] = true
			}
			if len(lit) != c.rows {
				t.Fatalf("lit %d rows, want %d: %+v", len(spans), c.rows, spans)
			}
			for _, sp := range spans {
				seg := dropCells(app.lastRows[sp.y], sp.lo)
				// a prompt choice's fixed style replaces HL, every other hover
				// draws Over, and a body row keeps its own style
				got := ""
				if len(seg) > 0 {
					got = seg[0].Over
					if seg[0].HL == "Host:Hover" {
						got = seg[0].HL
					}
				}
				if got != c.lit {
					t.Fatalf("row %d lit with %q, want %q: %+v", sp.y, got, c.lit, app.lastRows[sp.y])
				}
			}
		})
	}
}

// TestClickScrolledListOpensSameItem: the first click selects, and the list
// stays put so the second click opens the same item.
func TestClickScrolledListOpensSameItem(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 30)
	var items []any
	for i := range 30 {
		items = append(items, map[string]any{"text": fmt.Sprintf("item %02d", i), "fill": fmt.Sprintf("item %02d", i)})
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

// TestBoxEllipsisOnlyOnLongRows: a long row ends in an ellipsis, a short row
// padded to its width does not.
func TestBoxEllipsisOnlyOnLongRows(t *testing.T) {
	box := hostBox("", "", "", listRows([]listItem{{label: "short"}, {label: strings.Repeat("long ", 20)}}, 0, nil, 10), 40)
	if short, long := plainSegs(box[1]), plainSegs(box[2]); strings.Contains(short, "…") || !strings.Contains(long, "…") {
		t.Fatalf("short %q, long %q", short, long)
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
		top := 0
		rows := listRows(c.items, 0, &top, c.limit)
		if len(rows) != c.limit+1 || strings.Contains(plainSegs(slices.Concat(rows...)), c.hidden) {
			t.Errorf("%s: %q", c.name, plainSegs(slices.Concat(rows...)))
		}
	}
	// scrolling down, every window starts on an item's first row
	items := slices.Repeat([]listItem{{label: "item", more: more[:1]}}, 5)
	for _, limit := range []int{4, 6, 7} {
		top := 0
		for sel := range items {
			if rows := listRows(items, sel, &top, limit); !strings.Contains(plainSegs(rows[0]), "item") {
				t.Errorf("limit %d, selection %d: window starts on %q", limit, sel, plainSegs(rows[0]))
			}
		}
	}
}

// TestHoverStyle: a hover group draws over the text's own style.
func TestHoverStyle(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	base := uv.Style{Fg: rgb(0x9098a0), Bg: rgb(0x243427), Attrs: uv.AttrBold}
	for _, c := range []struct {
		name  string
		group string
		spec  *hostapi.HLSpec // nil: the group is not set
		want  uv.Style
	}{
		{"unset keys keep the base", "Hover", &hostapi.HLSpec{Underline: "curl", Sp: "#ff0000"},
			uv.Style{Fg: base.Fg, Bg: base.Bg, Attrs: uv.AttrBold, Underline: uv.UnderlineCurly, UnderlineColor: rgb(0xff0000)}},
		{"false and NONE clear", "HoverItem", &hostapi.HLSpec{Bold: new(false), Bg: "NONE"}, uv.Style{Fg: base.Fg}},
		{"replace drops the base", "Hover", &hostapi.HLSpec{Replace: true, Fg: "#ffffff"}, uv.Style{Fg: rgb(0xffffff)}},
		{"brighten mixes the text's fg toward white", "Hover", &hostapi.HLSpec{Brighten: 0.5},
			uv.Style{Fg: rgb(0xc8ccd0), Bg: base.Bg, Attrs: uv.AttrBold}},
		{"brighten darkens text on a lighter fill", "Hover", &hostapi.HLSpec{Brighten: 0.5, Bg: "#ffffff"},
			uv.Style{Fg: rgb(0x484c50), Bg: rgb(0xffffff), Attrs: uv.AttrBold}},
		{"a theme without the group still underlines", "HoverNope", nil,
			uv.Style{Fg: base.Fg, Bg: base.Bg, Attrs: uv.AttrBold, Underline: uv.UnderlineSingle}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.spec != nil {
				app.host.Themes.SetHL(c.group, *c.spec)
			}
			if got := app.thm.hover(base, c.group); !got.Equal(&c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSelectionRejoinsWrappedText: a drag over a wrapped user message and a
// wrapped code block copies the text the model and the user wrote, without
// the gutter bar, the bullets or the wrap points.
func TestSelectionRejoinsWrappedText(t *testing.T) {
	app, host := bootAgentApp(t, 44, 24)
	prompt := "please rewrite this helper so it stops allocating a slice on every call"
	code := "func add(a, b int) int { return a + b + 1000000 }"
	host.AppendMessage("user", prompt)
	host.AppendMessage("assistant", "here:\n\n```go\n"+code+"\n```\n")
	app.renderSync(t)
	dragRows := func(from, to string) string {
		_, y0 := findCell(t, app, from)
		_, y1 := findCell(t, app, to)
		if y1 <= y0 {
			t.Fatalf("%q did not wrap", from)
		}
		dragTo(app, 0, y0, app.width-1, y1)
		return app.selectionText()
	}
	if got := dragRows("please", "every call"); got != prompt {
		t.Errorf("prompt: %q", got)
	}
	if got := dragRows("func add", "1000000"); got != code {
		t.Errorf("code: %q", got)
	}
}

// TestMouseQuestion: the question overlay answers to the mouse like the
// permission prompt - the first click selects a row, the second picks it.
func TestMouseQuestion(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan []string, 1)
	go func() {
		ans, _ := app.AskQuestion([]tools.Question{{Type: tools.TypeSingle, Question: "apply the proposals?", Options: []tools.QuestionOption{{Label: "apply"}, {Label: "skip"}}}})
		done <- ans
	}()
	waitPrompt(t, app, func() bool { return app.question != nil })
	app.renderSync(t)
	x, y := findCell(t, app, "skip")
	clickAt(app, x+1, y)
	app.renderSync(t)
	clickAt(app, x+1, y)
	select {
	case ans := <-done:
		if len(ans) != 1 || ans[0] != "skip" {
			t.Fatalf("answered %q", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("clicking an option never answered")
	}
}

// TestToolRowClickExpands: in auto mode the command a tool ran is only on
// its clipped row, so a click opens that one row (whole command plus the
// output) without expanding the rest of the transcript.
func TestToolRowClickExpands(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.AppendReasoning("first idea\n", hostapi.TurnStat{ThoughtMS: 1000})
	long := "go test ./internal/hostapi -run TestSomethingWithAVeryLongName -count=1 -race"
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "1", Name: "bash", Label: "run the hostapi tests", Input: long, Status: "ok", Output: "ok  github.com/metafates/zemi/internal/hostapi"},
	}})
	app.renderSync(t)
	if strings.Contains(ansi.Strip(app.Frame()), "-race") {
		t.Fatal("the collapsed row should not show the command")
	}
	x, y := findCell(t, app, "Ran 1 command")
	clickAt(app, x+1, y)
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "-race") || !strings.Contains(frame, "ok  github.com/metafates") {
		t.Fatalf("clicking a tool row should show its command and output:\n%s", frame)
	}
	if strings.Contains(frame, "first idea") {
		t.Fatalf("one row expanded the whole transcript:\n%s", frame)
	}
	x, y = findCell(t, app, "run the hostapi tests")
	clickAt(app, x+1, y)
	app.renderSync(t)
	if strings.Contains(ansi.Strip(app.Frame()), "-race") {
		t.Fatal("a second click should collapse the row again")
	}
}
