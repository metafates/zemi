package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
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

func TestMouseDragCopies(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.AppendMessage("user", "hello there, world")
	app.renderSync(t)
	x, y := findCell(t, app, "hello there")
	app.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseMotionMsg{X: x + 10, Y: y, Button: tea.MouseLeft})
	if _, cmd := app.Update(tea.MouseReleaseMsg{X: x + 10, Y: y, Button: tea.MouseLeft}); cmd == nil {
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

func TestWordSpan(t *testing.T) {
	for _, c := range []struct {
		text     string
		x        int
		from, to int
	}{
		{" manual  plan ", 3, 1, 7},
		{" manual  plan ", 7, 0, 0},
		{"✻ auto", 3, 2, 6},
	} {
		if from, to := wordSpan(c.text, c.x); from != c.from || to != c.to {
			t.Errorf("wordSpan(%q, %d) = %d, %d", c.text, c.x, from, to)
		}
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

// TestPopupClickOverWindow: a completion row drawn over a split window
// takes the click, not the window under it.
func TestPopupClickOverWindow(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	if err := host.RT.ExecuteChunk(context.Background(), `zemi.win.open({ cmd = "sleep 2", kind = "split" })`, "w.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	// the window exits on its own: closing it races inside vt's Emulator
	app.completion.files = []string{"a.go", "b.go", "c.go"}
	app.Update(tea.KeyPressMsg{Code: '@', Text: "@"})
	app.renderSync(t)
	x, y := findCell(t, app, "b.go")
	clickAt(app, x+1, y)
	if app.focusWin != 0 || app.composer.text != "@b.go " {
		t.Fatalf("focusWin=%d composer=%q", app.focusWin, app.composer.text)
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
	for _, c := range []struct {
		name string
		open func(app *App)
		text string // hovered text
		dx   int    // cell offset from where text starts
		rows int    // rows lit, 0 = not a target
	}{
		{"memory text row lights the whole item", memories, "sigma", 1, 3},
		{"right of memory text is not a target", memories, "sigma", 40, 0},
		{"the selected memory is not a target", memories, "short one", 1, 0},
		{"memory section header is not a target", memories, "Remembered", 1, 0},
		{"overlay row text is a target", overlay, "alpha", 1, 1},
		{"overlay row padding is not a target", overlay, "alpha", 6, 0},
		{"overlay gutter is not a target", overlay, "beta and", -1, 0},
		{"gap before the key column is not a target", keyed, "current", -3, 0},
		{"the key column lights its item", keyed, "current", 1, 1},
		{"a choice is a target", perm, "No ", 0, 1},
		{"the gutter left of a choice is not a target", perm, "No ", -2, 0},
		{"thought line text is a target", thought, "Thought for", 1, 1},
		{"right of the thought line is not a target", thought, "Thought for", 60, 0},
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
				if lit := dropCells(app.lastRows[sp.y], sp.lo); len(lit) == 0 || !strings.Contains(lit[0].HL, "Hover") {
					t.Fatalf("row %d not lit: %+v", sp.y, app.lastRows[sp.y])
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
// half and no header without its first item, and keeps its height.
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
}
