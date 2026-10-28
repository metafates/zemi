package ui

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// newTestApp builds an App over an unbooted runtime: enough for composer,
// completion, and cmdline tests (no widgets, no Lua calls).
func newTestApp(t *testing.T, projectDir string) *App {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	host := hostapi.NewHost(rt, log, config.NewStore(), config.Dirs{ProjectDir: projectDir})
	app := NewApp(host, log)
	host.SetUIHooks(app)
	return app
}

func TestComposerEdit(t *testing.T) {
	const accent = "\u0301" // a combining acute, one grapheme with its base
	run := func(name string, mutate func(c *composerState), wantText string, wantCur int) {
		t.Run(name, func(t *testing.T) {
			c := &composerState{}
			mutate(c)
			if c.text != wantText || c.cursor != wantCur {
				t.Fatalf("text=%q cursor=%d, want %q %d", c.text, c.cursor, wantText, wantCur)
			}
		})
	}
	run("insert", func(c *composerState) { c.insert("hello"); c.insert(" world") }, "hello world", 11)
	run("backspace", func(c *composerState) { c.insert("ab"); c.backspace() }, "a", 1)
	run("backspace-empty", func(c *composerState) { c.backspace() }, "", 0)
	run("backspace-keeps-mark", func(c *composerState) { c.insert("cafe" + accent); c.backspace() }, "caf", 3)
	run("backspace-mark-run", func(c *composerState) { c.insert("e" + accent + accent); c.backspace() }, "", 0)
	run("backspace-lone-mark", func(c *composerState) { c.insert(accent); c.backspace() }, "", 0)
	run("delete", func(c *composerState) { c.insert("abc"); c.cursor = 1; c.delete() }, "ac", 1)
	run("delete-eof", func(c *composerState) { c.insert("ab"); c.delete() }, "ab", 2)
	run("delete-keeps-mark", func(c *composerState) { c.insert("a" + accent + "b"); c.cursor = 0; c.delete() }, "b", 0)
	run("delete-mark-run", func(c *composerState) { c.insert("a" + accent + accent + "b"); c.cursor = 0; c.delete() }, "b", 0)
	run("left-right", func(c *composerState) { c.insert("ab"); c.left(); c.left(); c.right() }, "ab", 1)
	run("left-home", func(c *composerState) { c.insert("ab"); c.left(); c.left(); c.left() }, "ab", 0)
	run("newline", func(c *composerState) { c.insert("a"); c.insert("\n"); c.insert("b") }, "a\nb", 3)
	run("backspace-joins", func(c *composerState) { c.insert("a\nb"); c.cursor = 2; c.backspace() }, "ab", 1)
	run("word", func(c *composerState) { c.insert("one two"); c.delWord() }, "one ", 4)
	run("word-punct", func(c *composerState) { c.insert("x.ts add"); c.delWord() }, "x.ts ", 5)
	run("word-to-newline", func(c *composerState) { c.insert("a\nb"); c.delWord() }, "a\n", 2)
	run("word-left", func(c *composerState) { c.insert("one two  "); c.wordLeft() }, "one two  ", 4)
	run("word-right", func(c *composerState) { c.insert("one two"); c.cursor = 0; c.wordRight(); c.wordRight() }, "one two", 7)
}

func TestComposerUndo(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	for _, r := range "hi there" {
		app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
	}
	app.composerKey("ctrl+w", tea.KeyPressMsg{})
	var got []string
	for range 4 {
		app.composerKey("ctrl+z", tea.KeyPressMsg{})
		got = append(got, app.composer.text)
	}
	if want := "hi there|hi |hi|"; strings.Join(got, "|") != want {
		t.Fatalf("undo steps %q, want %q", got, want)
	}
	// ctrl+c clearing a draft is one step too
	for _, r := range "alpha beta" {
		app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
	}
	app.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	app.composerKey("ctrl+z", tea.KeyPressMsg{})
	if app.composer.text != "alpha beta" {
		t.Fatalf("ctrl+z after ctrl+c: %q", app.composer.text)
	}
}

// TestDraftCap: typing or pasting past maxDraftChars refuses with a toast
// and leaves the draft at the cap, while a large paste still folds to a
// token, which expand unfolds at submit.
func TestDraftCap(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	chunk := strings.Repeat("a", 996)
	for range 30 {
		app.composerKey("x", tea.KeyPressMsg{Text: chunk})
	}
	if got := utf8.RuneCountInString(app.composer.text); got > maxDraftChars || got <= maxDraftChars-len(chunk) {
		t.Fatalf("the draft holds %d runes, want the cap %d minus one refused chunk", got, maxDraftChars)
	}
	if toast := lastToast(app); !strings.Contains(toast, "20k") {
		t.Fatalf("the cap toasted %q", toast)
	}
	// the refused keys change nothing; the draft stays editable
	full := utf8.RuneCountInString(app.composer.text)
	app.composerKey("backspace", tea.KeyPressMsg{})
	if got := utf8.RuneCountInString(app.composer.text); got != full-1 {
		t.Fatalf("backspace left %d runes, want %d", got, full-1)
	}
	// a paste over the token threshold still folds: its text waits in the
	// token, the draft stays short
	app.composer.text, app.composer.cursor = "", 0
	big := strings.Repeat("b", 5000)
	app.Update(tea.PasteMsg{Content: big})
	if app.composer.text == "" || strings.Contains(app.composer.text, "bbbb") {
		t.Fatalf("the big paste did not fold to a token: %d chars", len(app.composer.text))
	}
	if got := app.composer.expand(app.composer.text); got != big {
		t.Fatalf("the token expanded to %d chars, want the paste whole", len(got))
	}
}

// TestOverCapDraft: an over-cap draft (a recalled message, a restored queue)
// refuses every edit whole - a backspace trims it one cluster, never a
// clamp; enter refuses the send and keeps the draft; ctrl+c and esc clear it
// and the next ctrl+c arms quit; and a refused keystroke marks no frame, so
// a flood of them recomposes nothing.
func TestOverCapDraft(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	over := strings.Repeat("x", maxDraftChars+1000)
	app.composer.text, app.composer.cursor = over, utf8.RuneCountInString(over)

	// one backspace trims one cluster and keeps the rest, still over cap
	app.composerKey("backspace", tea.KeyPressMsg{})
	if got := utf8.RuneCountInString(app.composer.text); got != maxDraftChars+999 {
		t.Fatalf("backspace left %d runes, want %d intact", got, maxDraftChars+999)
	}
	// enter refuses whole: the draft stands and the cap toasted
	app.submitComposer()
	if got := utf8.RuneCountInString(app.composer.text); got != maxDraftChars+999 {
		t.Fatalf("enter left %d runes, want the draft kept", got)
	}
	if toast := lastToast(app); !strings.Contains(toast, "20k") {
		t.Fatalf("the send boundary toasted %q", toast)
	}
	// ctrl+c clears, and the next one arms quit
	app.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !app.composer.empty() {
		t.Fatalf("ctrl+c left %d runes", utf8.RuneCountInString(app.composer.text))
	}
	app.composer.text, app.composer.cursor = over, utf8.RuneCountInString(over)
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !app.composer.empty() {
		t.Fatalf("esc left %d runes", utf8.RuneCountInString(app.composer.text))
	}
	if !app.isArmed("ctrl+c") && app.armed.key != "" {
		t.Fatalf("after the clears quit is not one ctrl+c away: %+v", app.armed)
	}

	// a refused keystroke changes nothing and marks no frame: the flood pays
	// no compose a key
	app.composer.text, app.composer.cursor = over, utf8.RuneCountInString(over)
	app.draftCapToast() // the toast showing: the flood's first refusal
	app.frameDirty = false
	for range 100 {
		app.handleKey(tea.KeyPressMsg{Text: "x"})
		if !app.frameDirty {
			continue
		}
		t.Fatal("a refused keystroke marked the frame")
	}
	if got := utf8.RuneCountInString(app.composer.text); got != maxDraftChars+1000 {
		t.Fatalf("the flood left %d runes, want the draft untouched", got)
	}
}

// TestCapNeverClamps: the cap refuses an edit, it never rewrites the text:
// a keystroke past the cap is refused, a paste past it rides its token
// whole, and the send boundary refuses the expanded text while a draft at
// the cap sends whole.
func TestCapNeverClamps(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 12)
	runLua(t, app, `zemi.event.on("composer.submit", function(ev)
		if #ev.text == 20000 then return { decision = "rewrite", text = "reached whole" } end
	end)`)
	sent := func() string {
		n := len(app.composer.history)
		if n == 0 {
			return ""
		}
		return app.composer.history[n-1]
	}
	// a 20000-char draft sends whole: the submit event saw every byte
	app.composer.text = strings.Repeat("x", maxDraftChars)
	app.composer.cursor = maxDraftChars
	app.submitComposer()
	if got := sent(); got != "reached whole" {
		t.Fatalf("the send boundary sent %q, want the draft at the cap whole", got)
	}
	sentN := len(app.composer.history)
	// one keystroke past the cap is refused: the draft holds the cap, no
	// character of the user's text was dropped
	app.clearComposer()
	app.composer.text = strings.Repeat("x", maxDraftChars)
	app.composer.cursor = maxDraftChars
	app.composerKey("y", tea.KeyPressMsg{Text: "y"})
	if got := utf8.RuneCountInString(app.composer.text); got != maxDraftChars || strings.ContainsRune(app.composer.text, 'y') {
		t.Fatalf("the past-cap keystroke left %d runes, want the cap kept whole", got)
	}
	// a 20001-char paste rides its token whole and enter refuses it whole
	big := strings.Repeat("p", maxDraftChars+1)
	app.clearComposer()
	app.Update(tea.PasteMsg{Content: big})
	if got := app.composer.expand(app.composer.text); got != big {
		t.Fatalf("the paste holds %d chars, want all %d", len(got), len(big))
	}
	app.submitComposer()
	if len(app.composer.history) != sentN {
		t.Fatalf("the send boundary sent %q", sent())
	}
	if got := app.composer.expand(app.composer.text); got != big {
		t.Fatalf("the refused draft holds %d chars, want the paste kept whole", len(got))
	}
	if toast := lastToast(app); !strings.Contains(toast, "20k") {
		t.Fatalf("the send boundary toasted %q", toast)
	}
}

// TestComposerRowMoves: up and down move by the visual row the composer
// draws, so a wrapped line walks its own rows and history only recalls off
// the drawn first or last row.
func TestComposerRowMoves(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.pushHistory("recalled")
	const nine = "aaaaaaaaa"
	wrapped := nine + nine + "aaaaaa" // draws three rows at rune offsets 0, 9, 18
	for _, c := range []struct {
		name, text, key, want string
		cursor, wantCur       int
	}{
		{"up within a wrapped line", wrapped, "up", wrapped, 12, 3},
		{"down within a wrapped line", wrapped, "down", wrapped, 10, 19},
		{"up from the first row recalls", wrapped, "up", "recalled", 2, 8},
		{"down from the last row stays", wrapped, "down", wrapped, 20, 20},
		{"up over a hard break", "ab\ncd", "up", "ab\ncd", 4, 1},
		{"down over a hard break", "ab\ncd", "down", "ab\ncd", 1, 4},
		{"down from a row's end", "ab\ncd", "down", "ab\ncd", 2, 5},
		{"down clamps to the row below", "abcdefghij\nx", "down", "abcdefghij\nx", 5, 10},
		{"up clamps to the row above", "abcdefghij\nx", "up", "abcdefghij\nx", 10, 1},
		{"a shell draft wraps two cells short", "!" + nine + nine, "up", "!" + nine + nine, 11, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			app.composerRows(10, "")
			app.composer.text, app.composer.cursor = c.text, c.cursor
			app.composer.histIdx = len(app.composer.history) // each case starts at the draft
			app.composerKey(c.key, tea.KeyPressMsg{})
			if app.composer.text != c.want || app.composer.cursor != c.wantCur {
				t.Fatalf("text=%q cursor=%d, want %q %d", app.composer.text, app.composer.cursor, c.want, c.wantCur)
			}
		})
	}
}

func TestComposerHistory(t *testing.T) {
	c := &composerState{text: "draft"}
	c.histPrev()
	if c.text != "draft" {
		t.Fatalf("an empty history recalled %q", c.text)
	}
	for _, s := range []string{"one", "two"} {
		c.pushHistory(s)
	}
	// past either end a recall stays put
	for i, step := range []struct {
		recall func()
		want   string
	}{{c.histPrev, "two"}, {c.histPrev, "one"}, {c.histPrev, "one"}, {c.histNext, "two"}, {c.histNext, "draft"}, {c.histNext, "draft"}} {
		if step.recall(); c.text != step.want {
			t.Fatalf("step %d: %q, want %q", i, c.text, step.want)
		}
	}
	// A repeat moves the entry to the end, wherever it was; a recall
	// resubmitted never walks itself twice.
	c2 := &composerState{}
	c2.pushHistory("one")
	c2.pushHistory("two")
	c2.pushHistory("one")
	if len(c2.history) != 2 || c2.history[0] != "two" {
		t.Fatalf("duplicate: %#v", c2.history)
	}
	for i := range historyCap {
		c2.pushHistory(strconv.Itoa(i))
	}
	if len(c2.history) != historyCap || c2.history[0] != "0" {
		t.Fatalf("cap: %d entries from %q", len(c2.history), c2.history[0])
	}
}

func TestWrapComposer(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	for _, c := range []struct {
		name, text string
		w, cursor  int
		rows       string
		row, col   int
		soft       string // one mark per row: ~ wraps onto the next, . ends
	}{
		{"empty", "", 8, 0, "", 0, 0, "."},
		{"breaks after space", "hello world", 8, 6, "hello |world", 1, 0, "~."},
		{"splits long word", "abcdefghij", 4, 4, "abcd|efgh|ij", 1, 0, "~~."},
		{"cursor before newline", "a\nb", 8, 1, "a|b", 0, 1, ".."},
		{"wide runes by cell", "你好世界", 5, 4, "你好|世界", 1, 2, "~."},
		{"tab takes a cell", "a\tbc", 3, 3, "a |bc", 1, 1, "~."},
	} {
		app.composer.text, app.composer.cursor = c.text, c.cursor
		row, col, rows, soft := app.wrapComposer(c.w)
		var got []string
		marks := ""
		for i, r := range rows {
			got = append(got, string(r))
			marks += map[bool]string{true: "~", false: "."}[soft[i]]
		}
		if strings.Join(got, "|") != c.rows || row != c.row || col != c.col || marks != c.soft {
			t.Errorf("%s: rows %q cursor %d,%d soft %q", c.name, got, row, col, marks)
		}
	}
}

// TestComposerHiddenRows: a draft taller than the window shows
// composerMaxRows rows, fading the ones past an edge while content lies past
// it, deepest at the edge, and keeping the cursor row off the ramp with a row
// of headroom under it.
func TestComposerHiddenRows(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = strings.Repeat("line\n", 11) + "end" // 12 rows
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	for _, c := range []struct {
		name   string
		cursor int
		want   []float64 // the fade of the rows shown, top down
	}{
		{
			"at the head only the rows below fade", 0,
			[]float64{0, 0, 0, 0, 0, 0, 1.0 / 3, 2.0 / 3},
		},
		{
			"at the tail only the rows above fade", 58,
			[]float64{2.0 / 3, 1.0 / 3, 0, 0, 0, 0, 0, 0},
		},
		{
			"a cursor with a row below it is off the ramp", 40,
			[]float64{2.0 / 3, 1.0 / 3, 0, 0, 0, 0, 0, 2.0 / 3},
		},
	} {
		app.composer.cursor = c.cursor
		rows := app.composerRows(40, "")
		got := make([]float64, len(rows))
		for i, r := range rows {
			got[i] = r[0].Fade
		}
		if len(rows) != composerMaxRows || !slices.EqualFunc(got, c.want, near) {
			t.Errorf("%s: %d rows %v, want %d %v", c.name, len(rows), got, composerMaxRows, c.want)
		}
	}
}

// TestComposerPlaceholder: an empty draft shows the placeholder dim under
// the cursor and left out of a copy; the first typed rune takes the cell the
// cursor was on.
func TestComposerPlaceholder(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.focusables = []string{"side"}
	for _, c := range []struct {
		name, text, focus, want string
		cursor                  int  // the cursor cell's column, -1 for none
		modal                   bool // a box over the composer has the keys
	}{
		{"empty", "", "", "Ask anything", 0, false},
		{"typed", "h", "", "h", 1, false},
		{"command draft", "/", "", "/", 2, false},
		{"a node has the keys", "", "side", "Ask anything", -1, false},
		{"a modal has the keys", "", "", "Ask anything", -1, true},
	} {
		app.composer.text, app.composer.cursor = c.text, len(c.text)
		app.host.SetFocus(c.focus)
		if c.modal {
			app.openOverlay(hostapi.Overlay{ID: "box", Title: "box"})
		}
		segs := app.composerRows(40, "Ask anything")[0]
		cursor, col := -1, 0
		for _, s := range segs {
			if s.HL == hostapi.ChromeCursor {
				cursor = col
			}
			if c.text == "" && c.want != "" && (!s.NoCopy || s.HL != "Dim" && s.HL != hostapi.ChromeCursor) {
				t.Errorf("%s: placeholder segment %+v copies or reads as text", c.name, s)
			}
			col += ansi.StringWidth(s.Text)
		}
		if got := strings.TrimRight(hostapi.SegsText(segs), " "); got != c.want || cursor != c.cursor {
			t.Errorf("%s: row %q, cursor at %d; want %q at %d", c.name, got, cursor, c.want, c.cursor)
		}
		if c.modal {
			app.cancelOverlay(app.overlays[len(app.overlays)-1].o)
		}
	}
}

// TestComposerGutter: a plain draft starts at the frame's inner edge with no
// gutter down its rows; a shell draft says so with its own glyph on the first
// row and holds the cells it opens on the ones after it.
func TestComposerGutter(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = "one\ntwo"
	rows := app.composerRows(40, "")
	if len(rows) != 2 {
		t.Fatalf("a two-line draft drew %d rows", len(rows))
	}
	for i, want := range []string{"one", "two"} {
		if got := hostapi.SegsText(rows[i]); got != want || hostapi.SegsWidth(rows[i]) != 3 {
			t.Fatalf("row %d is %q over %d cells, want %q in 3: no gutter", i, got, hostapi.SegsWidth(rows[i]), want)
		}
	}
	app.composer.text = "!ls\ntwo"
	rows = app.composerRows(40, "")
	if got := rows[0][0]; got.Text != "! " || got.HL != "PromptShell" || hostapi.SegsText(rows[0]) != "! ls" {
		t.Fatalf("shell prompt %+v", rows[0])
	}
	if got := rows[1][0]; got.Text != "  " || got.HL != "" || hostapi.SegsText(rows[1]) != "  two" {
		t.Fatalf("shell gutter %+v", rows[1])
	}
	// a full row ends at the cell the cursor mark keeps free, and a shell row
	// a cell short of that, its glyph standing in for the draft's own first one
	for _, c := range []struct {
		name, text string
		want       int
	}{
		{"plain", strings.Repeat("a", 60), 39},
		{"shell", "!" + strings.Repeat("a", 60), 38},
	} {
		app.composer.text = c.text
		if got := hostapi.SegsWidth(app.composerRows(40, "")[0]); got != c.want {
			t.Fatalf("the %s row is %d cells wide, want %d", c.name, got, c.want)
		}
	}
}

func TestCompletionFlow(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.completion.setFiles([]string{"cmd/root.go", "internal/ui/app.go", "main.go"})

	type k struct {
		key  string
		text string
	}
	send := func(ks ...k) {
		for _, x := range ks {
			app.composerKey(x.key, tea.KeyPressMsg{Text: x.text})
		}
	}

	send(k{"s", "s"}, k{"e", "e"}, k{"e", "e"}, k{" ", " "}, k{"@", "@"})
	if !app.completion.open || app.completion.token != "" {
		t.Fatalf("popup after @: open=%v token=%q", app.completion.open, app.completion.token)
	}
	if len(app.completion.matches) != 3 {
		t.Fatalf("empty token matches: %v", app.completion.matches)
	}
	send(k{"u", "u"}, k{"i", "i"})
	if got := app.completion.matches; len(got) != 1 || got[0].Index != 1 {
		t.Fatalf("token 'ui' matches: %v", got)
	}
	if app.completion.start != 4 { // after "see "
		t.Fatalf("start=%d, want 4", app.completion.start)
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	if app.completion.open {
		t.Fatal("popup open after accept")
	}
	if got := app.composer.text; got != "see @internal/ui/app.go " {
		t.Fatalf("accepted text: %q", got)
	}
	// the reference stays editable: back inside its word, the popup returns
	app.composerKey("left", tea.KeyPressMsg{})
	if !app.completion.open || app.completion.token != "internal/ui/app.go" {
		t.Fatalf("popup on the accepted word: open=%v token=%q", app.completion.open, app.completion.token)
	}
	app.composerKey("right", tea.KeyPressMsg{}) // back past the word, where the popup closes
	if app.completion.open {
		t.Fatal("popup open at the end of the accepted word")
	}
	// A trigger mid-word does not open; esc closes an open palette.
	send(k{"a", "a"}, k{"@", "@"})
	if app.completion.open {
		t.Fatal("popup opened mid-word")
	}
	app.composer.text, app.composer.cursor = "/qui", 4
	app.updatePalette()
	if !app.palette.open {
		t.Fatal("palette not open after trigger")
	}
	app.composerKey("esc", tea.KeyPressMsg{})
	if app.palette.open || app.completion.open {
		t.Fatal("popups open after esc")
	}
}

func TestSkillCompletion(t *testing.T) {
	app, host := bootAgentApp(t, 60, 12)
	dir := filepath.Join(host.Dirs.ConfigHome, "skills", "lint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: lint\ndescription: lint the tree\n---\n\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadUI(t, host, host.SkillsUserHooksStep())

	send := func(ks ...string) {
		for _, k := range ks {
			app.composerKey(k, tea.KeyPressMsg{Text: k})
		}
	}
	send("@", "l", "i")
	waitPrompt(t, app, func() bool { return app.completion.shown() })
	if app.completion.trigger != '@' || app.completion.title() != "Attach" || app.completion.count() != 1 {
		t.Fatalf("popup after @li: trigger=%q title=%q rows=%d", app.completion.trigger, app.completion.title(), app.completion.count())
	}
	// the one row sits under the Skills header
	if rows := app.completionRows(60, 10, 0); len(rows) < 2 || strings.TrimSpace(hostapi.SegsText(rows[0])) != "Skills" {
		t.Fatalf("rows: %q", hostapi.SegsText(rows[0]))
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != "@lint " {
		t.Fatalf("accepted text: %q", got)
	}
	if app.completion.open {
		t.Fatal("popup open after accept")
	}
}

// TestCompletionScan: the popup's own walk lists the project's files,
// ignored ones included, and skips .git and the build directories - a
// hidden venv with the visible one, dotfiles listed.
// pathToken pins the split every listing consumer joins with: the
// directory always ends in its separator, so `@~` labels entries as
// `~/name`, not `~name`.
func TestPathToken(t *testing.T) {
	for _, c := range []struct {
		token, dir, base string
		ok               bool
	}{
		{"~", "~/", "", true},
		{"~/", "~/", "", true},
		{"~Downloads", "~/", "Downloads", true},
		{`"~Downloads`, "~/", "Downloads", true},
		{"..", "../", "", true},
		{"..Downloads", "../", "Downloads", true},
		{"...", "../", ".", true},
		{"/", "/", "", true},
		{"/Users", "/", "Users", true},
		{"./x", "./", "x", true},
		{"~/Sub/a", "~/Sub/", "a", true},
		{`"~/`, `~/`, "", true},
		{"name", "", "", false},
	} {
		dir, base, ok := pathToken(c.token)
		if dir != c.dir || base != c.base || ok != c.ok {
			t.Errorf("pathToken(%q) = %q, %q, %v; want %q, %q, %v", c.token, dir, base, ok, c.dir, c.base, c.ok)
		}
	}
}

// TestCompletionNarrowsHome: a head typed without its separator - `@~`,
// `@..` - keeps narrowing its directory's listing as the letters land,
// and a word that stops being a path leaves no listing row behind.
func TestCompletionNarrowsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir reads it; no t.Parallel beside it
	for _, d := range []string{"Downloads", "Documents", filepath.Join("proj", "sub")} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "decoy.txt"), []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t, filepath.Join(home, "proj"))
	open := func(token string) {
		app.composer.text, app.composer.cursor = token, len([]rune(token))
		app.updateCompletion()
	}
	labels := func() []string {
		out := make([]string, len(app.completion.paths))
		for i, p := range app.completion.paths {
			out[i] = p.label
		}
		return out
	}
	open("@~")
	if got := labels(); !slices.Contains(got, "~/Downloads/") || !slices.Contains(got, "~/decoy.txt") {
		t.Fatalf("home listing: %q", got)
	}
	open("@~Downloads")
	if got := app.completion.paths; len(got) != 1 || got[0].label != "~/Downloads/" ||
		!slices.Equal(got[0].match, []int{2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("home narrowing: %+v", got)
	}
	open(`@"~Down`)
	if got := app.completion.paths; len(got) != 1 || got[0].label != `"~/Downloads/` {
		t.Fatalf("quoted home narrowing: %+v", got)
	}
	open("@..")
	if got := labels(); !slices.Contains(got, "../Downloads/") {
		t.Fatalf("parent listing: %q", got)
	}
	open("@..Downloads")
	if got := app.completion.paths; len(got) != 1 || got[0].label != "../Downloads/" {
		t.Fatalf("parent narrowing: %+v", got)
	}
	open("@decoy")
	if got := app.completion.paths; len(got) != 0 {
		t.Fatalf("stale listing rows: %+v", got)
	}
}

func TestCompletionScan(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{".git", "node_modules", "internal", filepath.Join(".venv", "lib"), "lib"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"main.go", "internal/app.go", ".git/config", "node_modules/x.js", ".venv/lib/x.py", ".gitignore", "lib/.hidden.go", "My Notes.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := newTestApp(t, dir)
	cmd := app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	if cmd == nil {
		t.Fatal("no scan cmd launched")
	}
	msg, ok := cmd().(filesScannedMsg)
	if !ok {
		t.Fatal("cmd did not return filesScannedMsg")
	}
	app.onFilesScanned(msg)
	for _, want := range []string{"main.go", "internal/app.go", ".gitignore", "lib/.hidden.go"} {
		if !slices.Contains(app.completion.files, want) {
			t.Fatalf("missing %s in %v", want, app.completion.files)
		}
	}
	for _, gone := range []string{".git/config", "node_modules/x.js", ".venv/lib/x.py"} {
		if slices.Contains(app.completion.files, gone) {
			t.Fatalf("skipped dir leaked: %s", gone)
		}
	}
	// the empty token's head is the top level, shallowest first
	if app.completion.files[0] != ".gitignore" {
		t.Fatalf("head of the list: %v", app.completion.files[:3])
	}

	// Reopening rescans, and a result from an older scan is dropped.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	app.composerKey("esc", tea.KeyPressMsg{})
	cmd = app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	if cmd == nil {
		t.Fatal("no rescan on reopen")
	}
	app.onFilesScanned(cmd().(filesScannedMsg))
	app.onFilesScanned(msg)
	if !slices.Contains(app.completion.files, "new.go") {
		t.Fatalf("rescan missed new.go: %v", app.completion.files)
	}

	// a quoted token narrows the corpus by the path inside the quotes
	app.composer.text, app.composer.cursor = `@"My`, 4
	app.updateCompletion()
	if len(app.completion.matches) != 1 || app.completion.files[app.completion.matches[0].Index] != "My Notes.md" {
		t.Fatalf("quoted token matches: %+v", app.completion.matches)
	}
}

// TestCompletionSections: both sections show under their headers while
// both have rows, the Files one capped so the Skills header stays in the
// window, and a skill query alone shows Skills at the top.
func TestCompletionSections(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	dir := filepath.Join(host.Dirs.ConfigHome, "skills", "lint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: lint\ndescription: lint the tree\n---\n\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadUI(t, host, host.SkillsUserHooksStep())

	var files []string
	for i := range 9 {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	app.completion.skills = app.host.SkillMentions()
	app.completion.reskill()
	app.onFilesScanned(filesScannedMsg{gen: app.completion.walkGen, files: files})
	texts := func() (out []string) {
		for _, r := range app.completionRows(76, 10, 0) {
			out = append(out, strings.TrimSpace(hostapi.SegsText(r)))
		}
		return out
	}
	rows := texts()
	// 1 Files header + 6 files + 1 blank separator + 1 Skills header + 1 skill row
	if len(rows) != 10 || !strings.HasPrefix(rows[0], "Files") || !strings.HasPrefix(rows[8], "Skills") || rows[9] == "" {
		t.Fatalf("rows = %q", rows)
	}
	if !slices.ContainsFunc(app.completion.skillRows, func(r listItem) bool { return r.label == "lint" }) {
		t.Fatalf("lint is not a skill row: %v", app.completion.skillRows)
	}
	// a skill query alone: the Files section drops, header included
	send := func(ks ...string) {
		for _, k := range ks {
			app.composerKey(k, tea.KeyPressMsg{Text: k})
		}
	}
	send("l")
	rows = texts()
	if len(rows) != 2 || rows[0] != "Skills" || !strings.Contains(rows[1], "lint") {
		t.Fatalf("skill query rows: %q", rows)
	}
	// the windows walk the frame's order: past the files, a row accepts as
	// the skill it shows
	base := len(app.completion.extra) + len(app.completion.matches) + len(app.completion.paths)
	idx := slices.IndexFunc(app.completion.skillRows, func(r listItem) bool { return r.label == "lint" })
	app.completion.selected = base + idx
	app.acceptCompletion()
	if got := app.composer.text; got != "@lint " {
		t.Fatalf("the lint row accepted %q", got)
	}
}

// TestCompletionWalksIn: accepting a directory candidate keeps the popup
// open on the directory, in the frame the token typed, and a file accept
// closes it with its space.
func TestCompletionWalksIn(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "Sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "Sub", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := func(token string) {
		app.composer.text, app.composer.cursor = token, len([]rune(token))
		app.updateCompletion()
	}
	open("@" + out + "/Su")
	// a candidate word replaces what follows the @, the @ stays, and what
	// the token matched rides highlighted - a prefix by its head, a fuzzy
	// hit by the runes it took, offset past the directory the token typed
	if got := app.completion.paths; len(got) != 1 || got[0].label != out+"/Sub/" || !slices.Equal(got[0].match, []int{len(out) + 1, len(out) + 2}) {
		t.Fatalf("candidates: %+v", got)
	}
	open("@" + out + "/ub")
	if got := app.completion.paths; len(got) != 1 || got[0].label != out+"/Sub/" || !slices.Equal(got[0].match, []int{len(out) + 2, len(out) + 3}) {
		t.Fatalf("fuzzy candidates: %+v", got)
	}
	open("@" + out + "/")
	// a directory accept walks in: no trailing space, the popup open
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != "@"+out+"/Sub/" {
		t.Fatalf("walked to %q", got)
	}
	if !app.completion.open || len(app.completion.paths) != 1 || app.completion.paths[0].label != out+"/Sub/a.txt" {
		t.Fatalf("candidates after the walk: %+v open=%v", app.completion.paths, app.completion.open)
	}
	// a file accept closes the popup
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != "@"+out+"/Sub/a.txt " || app.completion.open {
		t.Fatalf("accepted %q, open=%v", got, app.completion.open)
	}
	// a quoted token keeps its opening quote through the walk and closes
	// it on a file accept
	spacey := filepath.Join(t.TempDir(), "My Docs")
	if err := os.MkdirAll(filepath.Join(spacey, "Deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spacey, "Deep", "n.txt"), []byte("n"), 0o600); err != nil {
		t.Fatal(err)
	}
	open(`@"` + spacey + `/`)
	if got := app.completion.paths; len(got) != 1 || got[0].label != `"`+spacey+"/Deep/" {
		t.Fatalf("quoted candidates: %+v", got)
	}
	// the quote and the walked dir shift the match offsets with them
	open(`@"` + spacey + `/De`)
	if got := app.completion.paths[0]; got.label != `"`+spacey+"/Deep/" ||
		!slices.Equal(got.match, []int{len(spacey) + 2, len(spacey) + 3}) {
		t.Fatalf("quoted match offsets: %+v", app.completion.paths)
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != `@"`+spacey+"/Deep/" {
		t.Fatalf("quoted walk landed on %q", got)
	}
	if !app.completion.open {
		t.Fatal("popup closed on a quoted walk-in")
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != `@"`+spacey+"/Deep/n.txt\" " {
		t.Fatalf("quoted file accept: %q", got)
	}
	// a candidate a bare token cannot carry - a space in the name - rides
	// quoted from an unquoted walk too
	open("@" + filepath.Dir(spacey) + "/")
	cand := filepath.Dir(spacey) + "/My Docs/"
	idx := slices.IndexFunc(app.completion.paths, func(r listItem) bool { return r.label == cand })
	if idx < 0 {
		t.Fatalf("candidates: %+v", app.completion.paths)
	}
	app.completion.selected = idx
	app.acceptCompletion()
	if got := app.composer.text; got != `@"`+spacey+"/\"" || !app.completion.open {
		t.Fatalf("a spacey directory walked in as %q", got)
	}
}

// TestCompletionListingCaps: a bare listing keeps half its rows for each
// kind, so a directory dense in subdirectories cannot push its files off
// the list, and the selection's far end is a row the popup drew.
func TestCompletionListingCaps(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	out := t.TempDir()
	for i := range 150 {
		if err := os.MkdirAll(filepath.Join(out, fmt.Sprintf("d%03d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, fmt.Sprintf("f%03d.txt", i)), []byte("f"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app.composer.text, app.composer.cursor = "@"+out+"/", len("@"+out+"/")
	app.updateCompletion()
	if got := len(app.completion.paths); got != listMaxFiles {
		t.Fatalf("candidates: %d, want %d", got, listMaxFiles)
	}
	var dirs int
	for _, r := range app.completion.paths {
		if strings.HasSuffix(r.label, "/") {
			dirs++
		}
	}
	if dirs != listMaxFiles/2 {
		t.Fatalf("dirs: %d, want %d", dirs, listMaxFiles/2)
	}
	app.completion.selected = app.completion.count() - 1
	app.acceptCompletion()
	if got := app.composer.text; got != "@"+out+fmt.Sprintf("/f%03d.txt ", listMaxFiles/2-1) {
		t.Fatalf("the last row accepted as %q", got)
	}
}

// TestCompletionListWidth: the popup keeps its width while the selection
// scrolls away from its widest path, which a narrow one cuts at the head,
// and lists at most listMaxFiles.
func TestCompletionListWidth(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	files := []string{"a/deeply/nested/a_really_long_file_name_here.txt"}
	for i := range 2 * listMaxFiles {
		files = append(files, fmt.Sprintf("f%03d.go", i))
	}
	app.onFilesScanned(filesScannedMsg{gen: app.completion.walkGen, files: files})
	// row 0 is the section header, row 1 the first file
	first := hostapi.SegsWidth(app.completionRows(76, 10, 0)[1])
	if row := strings.TrimSpace(hostapi.SegsText(app.completionRows(30, 10, 0)[1])); !strings.Contains(row, "_here.txt") {
		t.Fatalf("a cut path lost its name: %q", row)
	}
	app.completion.selected = app.completion.count() - 1
	rows := app.completionRows(76, 10, 0)
	if w := hostapi.SegsWidth(rows[1]); w != first || app.completion.count() != listMaxFiles {
		t.Fatalf("width %d then %d, %d items", first, w, app.completion.count())
	}
}

// TestCompletionPopupRefits: the @ popup's box fits the list it shows now,
// so a token that widens it narrows back when erased; only a frame with no
// matches keeps the size it had.
func TestCompletionPopupRefits(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	files := make([]string, 0, listMaxFiles+1)
	for i := range listMaxFiles {
		files = append(files, fmt.Sprintf("f%03d.go", i))
	}
	files = append(files, "a/really/long/project/path/that/stretches/the/popup/wide.go")
	app.completion.setFiles(files)

	type k struct{ key, text string }
	send := func(ks ...k) {
		for _, x := range ks {
			app.composerKey(x.key, tea.KeyPressMsg{Text: x.text})
		}
		app.renderSync(t)
	}
	width := func() int {
		f := ansi.Strip(app.Frame())
		for line := range strings.SplitSeq(f, "\n") {
			i := strings.Index(line, "┌ Attach")
			if i < 0 {
				continue
			}
			j := strings.Index(line[i:], "┐")
			if j < 0 {
				t.Fatalf("popup cut:\n%s", f)
			}
			return ansi.StringWidth(line[i:i+j]) + 1
		}
		t.Fatalf("popup not drawn:\n%s", f)
		return 0
	}

	send(k{"@", "@"})
	open := width()
	send(k{"w", "w"}, k{"i", "i"}, k{"d", "d"}, k{"e", "e"})
	if len(app.completion.matches) != 1 {
		t.Fatalf("token 'wide' matches: %v", app.completion.matches)
	}
	wide := width()
	send(k{"backspace", ""}, k{"backspace", ""}, k{"backspace", ""}, k{"backspace", ""})
	if app.completion.token != "" {
		t.Fatalf("token after erasing: %q", app.completion.token)
	}
	if got := width(); got != open {
		t.Fatalf("popup %d cells after erasing, want the %d it opened at (it widened to %d)", got, open, wide)
	}
}

func TestFileRanking(t *testing.T) {
	files := []string{"README.md", "agent/turn_final_response_app.py", "apps/x.go", "happy.go", "internal/ui/app.go", "lib/app_app.go", "ã©.go"}
	for _, c := range []struct{ token, want string }{
		{"", "README.md[] agent/turn_final_response_app.py[] apps/x.go[] happy.go[] internal/ui/app.go[] lib/app_app.go[] ã©.go[]"},
		{"app", "lib/app_app.go[4 5 6] internal/ui/app.go[12 13 14] apps/x.go[0 1 2] happy.go[1 2 3] agent/turn_final_response_app.py[14 20 30]"},
		{"uiapp", "internal/ui/app.go[9 10 12 13 14] agent/turn_final_response_app.py[7 12 14 20 30]"},
		{"READ", "README.md[0 1 2 3]"},
		{"é", ""},
	} {
		f := wordCompleter{token: c.token}
		f.setFiles(files)
		f.rematch()
		var got []string
		for _, m := range f.matches {
			got = append(got, fmt.Sprintf("%s%v", files[m.Index], m.Pos))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%q: got %q, want %q", c.token, strings.Join(got, " "), c.want)
		}
	}
}

func TestPaletteComplete(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	ran := "" // the command line that ran
	for _, name := range []string{"q", "qa", "echo"} {
		if _, err := app.host.Commands.Add(&hostapi.Command{
			Name: name, NArgs: "*", Desc: "test", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
			GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = name + " " + args; return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}

	type k struct {
		key  string
		text string
	}
	send := func(ks ...k) {
		for _, x := range ks {
			app.composerKey(x.key, tea.KeyPressMsg{Text: x.text})
		}
	}

	send(k{"/", "/"})
	if !app.palette.open || len(app.palette.matches) != 3 {
		t.Fatalf("palette after /: open=%v matches=%d", app.palette.open, len(app.palette.matches))
	}
	// a narrower query selects its first match, whatever was highlighted
	send(k{"down", ""}, k{"down", ""}, k{"q", "q"})
	if len(app.palette.matches) != 2 || app.palette.selected != 0 {
		t.Fatalf("after /, down, down, q: matches=%d selected=%d", len(app.palette.matches), app.palette.selected)
	}
	// Down, then up returns to the first match; tab accepts it.
	send(k{"down", ""}, k{"up", ""}, k{"tab", ""})
	if app.palette.open {
		t.Fatal("palette open after accept")
	}
	if got := app.composer.text; got != "/q " {
		t.Fatalf("accepted text: %q", got)
	}
	// Past "/q " the palette switches to argument candidates; q offers
	// none, so it closes instead of listing commands again.
	send(k{"e", "e"})
	if app.palette.open {
		t.Fatalf("palette open for a command without completions: args=%v", app.palette.args)
	}
	// Esc closes the palette and keeps the typed line.
	send(k{"backspace", ""}, k{"backspace", ""})
	app.composerKey("esc", tea.KeyPressMsg{})
	if app.palette.open || app.composer.text != "/q" {
		t.Fatalf("esc: open=%v text=%q", app.palette.open, app.composer.text)
	}
	// ctrl+space opens it again, with no edit
	app.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	if !app.palette.open || len(app.palette.matches) != 2 || app.composer.text != "/q" {
		t.Fatalf("ctrl+space: open=%v matches=%d text=%q", app.palette.open, len(app.palette.matches), app.composer.text)
	}

	// The bare "/" that opened the palette is not a draft: esc takes it back.
	app.composer.text, app.composer.cursor = "/", 1
	app.updatePalette()
	if !app.palette.open {
		t.Fatal("palette not open on /")
	}
	app.composerKey("esc", tea.KeyPressMsg{})
	if app.palette.open || app.composer.text != "" {
		t.Fatalf("esc on /: open=%v text=%q", app.palette.open, app.composer.text)
	}

	// Submitting a unique command runs it with the argument string.
	app.composer.text, app.composer.cursor = "/echo hi", 8
	app.updatePalette()
	app.composerKey("enter", tea.KeyPressMsg{})
	if ran != "echo hi" || app.composer.text != "" {
		t.Fatalf("submit: ran=%q text=%q", ran, app.composer.text)
	}

	// An unknown slash line is not sent and keeps the text for fixing.
	app.composer.text, app.composer.cursor = "/nope", 5
	app.updatePalette()
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != "/nope" {
		t.Fatalf("unknown submit cleared text: %q", got)
	}
	if len(app.toasts) == 0 {
		t.Fatal("no toast for unknown command")
	}

	// The reserved /reload never reaches the registry.
	app.composer.text, app.composer.cursor = "/reload", 7
	app.updatePalette()
	if len(app.palette.matches) != 0 {
		t.Fatalf("/reload listed: %d", len(app.palette.matches))
	}
}

func TestPaletteDispatch(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	ran := "" // the command line that ran
	for _, name := range []string{"zq", "zqa", "echo", "reloadall"} {
		if _, err := app.host.Commands.Add(&hostapi.Command{
			Name: name, NArgs: "*", Desc: "test", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
			GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = name + " " + args; return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	// What is highlighted beside "enter run" is what runs: any hit while the
	// name is being typed, only a name the typed one begins once arguments
	// follow.
	for _, c := range []struct {
		name, text string
		cursor     int
		keys       []string
		row, ran   string // the highlighted row, "" for none, and the line that ran
		left       string // the draft after enter
	}{
		{"a fuzzy hit runs", "/eho", 4, nil, "/echo", "echo ", ""},
		{"a prefix hit runs with the arguments", "/ec hi", 3, nil, "/echo", "echo hi", ""},
		{"a fuzzy hit is not listed before arguments", "/eho hi", 4, nil, "", "", "/eho hi"},
		{"the moved-to row runs over a typed name", "/zq hi", 3, []string{"down"}, "/zqa", "zqa hi", ""},
		{"the reserved reload lists no row to claim it", "/reload", 7, nil, "", "", ""},
	} {
		ran = ""
		app.composer.text, app.composer.cursor = c.text, c.cursor
		app.updatePalette()
		for _, key := range c.keys {
			app.composerKey(key, tea.KeyPressMsg{})
		}
		frame, row := ansi.Strip(app.Frame()), ""
		if m := regexp.MustCompile(`› (/\S+)`).FindStringSubmatch(frame); m != nil {
			row = m[1]
		}
		if row != c.row || !strings.Contains(frame, "enter run") {
			t.Errorf("%s: highlighted %q, want %q beside the hint:\n%s", c.name, row, c.row, frame)
		}
		app.composerKey("enter", tea.KeyPressMsg{})
		if ran != c.ran || app.composer.text != c.left {
			t.Errorf("%s: ran %q, want %q; text %q", c.name, ran, c.ran, app.composer.text)
		}
	}
}

// TestPasteToken: a large paste stands in the composer as one token that
// backspace removes whole and submit expands; small pastes stay inline.
// TestSubmitFailed: the test app has no model, so every send fails, and
// the same failure twice is one toast.
func TestSubmitFailed(t *testing.T) {
	for _, text := range []string{"send with no model keeps the text", "an empty popup still sends @nope.txt"} {
		app := newTestApp(t, t.TempDir())
		app.completion.setFiles([]string{"main.go"})
		for _, r := range text {
			app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
		}
		app.composerKey("enter", tea.KeyPressMsg{})
		app.composerKey("enter", tea.KeyPressMsg{})
		c := app.composer
		if c.text != text || c.cursor != len(c.runes()) || len(c.history) != 1 || app.completion.open {
			t.Errorf("%s: text %q cursor %d history %v popup %v", text, c.text, c.cursor, c.history, app.completion.open)
		}
		if len(app.toasts) != 1 || !strings.HasPrefix(app.toasts[0].body, "no model yet") {
			t.Errorf("%s: toasts %+v", text, app.toasts)
		}
	}
}

// TestSubmitEmptyShellDraft: a "!" with no command is an empty draft, not a
// message for the model.
// TestRestoreQueue: an interrupt hands the queue back as one draft that
// history keeps, with the popups a slash draft held closed.
func TestRestoreQueue(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 60, 12)
	host.SeedStream(hostapi.StreamSnapshot{Active: true})
	if err := host.AgentSendUser("queued"); err != nil {
		t.Fatal(err)
	}
	app.composer.text, app.composer.cursor = "/he", 3
	app.updatePalette()
	if !app.palette.open {
		t.Fatal("the palette did not open")
	}
	app.composerKey("esc", tea.KeyPressMsg{}) // closes the palette, then arms
	app.composerKey("esc", tea.KeyPressMsg{})
	app.composerKey("esc", tea.KeyPressMsg{}) // and interrupts
	if app.composer.text != "queued\n\n/he" || app.palette.open || app.completion.shown() {
		t.Fatalf("restore left text %q, palette %v, completion %v", app.composer.text, app.palette.open, app.completion.shown())
	}
	app.composerKey("ctrl+z", tea.KeyPressMsg{})
	if app.composer.text != "/he" {
		t.Fatalf("undo after the restore: %q", app.composer.text)
	}
	// the queue text is not lost with the draft: it recalls from history
	app.clearComposer()
	app.composerKey("ctrl+p", tea.KeyPressMsg{})
	if app.composer.text != "queued\n\n/he" {
		t.Fatalf("history does not hold the restored text: %q", app.composer.text)
	}
}

func TestSubmitEmptyShellDraft(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, t.TempDir())
	app.composer.text = "!"
	app.submitComposer()
	if app.composer.text != "!" || len(app.composer.history) != 0 {
		t.Fatalf("a bare ! submitted: draft %q, history %v", app.composer.text, app.composer.history)
	}
}

// TestComposerEditRecomputes: every composer write from Lua recomputes the
// popups, so a set or insert never leaves one keyed at stale rune offsets.
func TestComposerEditRecomputes(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 12)
	app.composer.text, app.composer.cursor = "/he", 3
	app.updateCompletion()
	if !app.palette.open {
		t.Fatal("the palette did not open")
	}
	runLua(t, app, `zemi.composer.set("hello")`)
	if app.palette.open {
		t.Fatal("set left the palette open over the new text")
	}
	runLua(t, app, `zemi.composer.insert(" see @no")`)
	if !app.completion.shown() {
		t.Fatalf("insert did not open the completion: %q", app.composer.text)
	}
}

// TestImagePasteUndo: an image paste is one undo step, like a text paste.
func TestImagePasteUndo(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, t.TempDir())
	app.composer.text, app.composer.cursor = "hi", 2
	app.Update(clipboardMsg{token: "[Image pasted.png]"})
	if app.composer.text != "hi [Image pasted.png] " {
		t.Fatalf("the token did not insert: %q", app.composer.text)
	}
	app.composerKey("ctrl+z", tea.KeyPressMsg{})
	if app.composer.text != "hi" {
		t.Fatalf("ctrl+z did not undo the image paste: %q", app.composer.text)
	}
}

func TestPasteToken(t *testing.T) {
	big := strings.Repeat("line\n", 12) // the trailing newline is no line of its own
	var c composerState
	c.insert("see ")
	c.paste(big)
	c.paste("small")
	if c.text != "see [Pasted text #1 +12 lines]small" {
		t.Fatalf("composer text %q", c.text)
	}
	if got := c.expand(c.text); got != "see "+big+"small" {
		t.Fatalf("expanded %q", got)
	}
	for range len("small") + 1 {
		c.backspace()
	}
	if c.text != "see " {
		t.Fatalf("after backspace %q", c.text)
	}
}

// TestPasteLineEndings: terminals paste line breaks as CR, and a paste's
// escape sequences never reach the composer.
func TestPasteLineEndings(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.Update(tea.PasteMsg{Content: "one\rtwo\r\nthree\x1b]0;owned\x07\tend"})
	if app.composer.text != "one\ntwo\nthree\tend" {
		t.Fatalf("composer text %q", app.composer.text)
	}
}

// TestPasteSurvivesSubmit: ctrl+z after a submit brings the draft back, so
// the tokens in it must still expand to what was pasted.
func TestPasteSurvivesSubmit(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	big := strings.Repeat("line\n", 40) + "end"
	app.Update(tea.PasteMsg{Content: big})
	// what a sent message does; the test app has no model to send to
	prev := composerSnap{app.composer.text, app.composer.cursor}
	app.clearComposer()
	app.composer.snapshot(prev, false)
	app.composerKey("ctrl+z", tea.KeyPressMsg{})
	if got := app.composer.expand(app.composer.text); got != big {
		t.Fatalf("restored draft expands to %q", got)
	}
}
