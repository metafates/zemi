package ui

import (
	"fmt"
	"log/slog"
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
	t.Cleanup(host.Close)
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
	run("kill-line", func(c *composerState) { c.insert("3m"); c.editKey("ctrl+u") }, "", 0)
	run("kill-line-keeps-tail", func(c *composerState) { c.insert("one two"); c.cursor = 4; c.editKey("ctrl+u") }, "two", 0)
	run("kill-line-keeps-other-lines", func(c *composerState) { c.insert("a\nbc"); c.editKey("ctrl+u") }, "a\n", 2)
}

// TestOverCapDraft: an over-cap draft (a recalled message, a restored queue)
// refuses every edit whole - a backspace trims it one cluster, never a
// clamp; enter refuses the send and keeps the draft; ctrl+c and esc clear it
// and the next ctrl+c arms quit; and a refused keystroke marks no frame, so
// a flood of them recomposes nothing.
func TestOverCapDraft(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	over := strings.Repeat("x", _maxDraftChars+1000)
	app.composer.text, app.composer.cursor = over, utf8.RuneCountInString(over)

	// one backspace trims one cluster and keeps the rest, still over cap
	app.composerKey("backspace", tea.KeyPressMsg{})
	if got := utf8.RuneCountInString(app.composer.text); got != _maxDraftChars+999 {
		t.Fatalf("backspace left %d runes, want %d intact", got, _maxDraftChars+999)
	}
	// enter refuses whole: the draft stands and the cap toasted
	app.submitComposer()
	if got := utf8.RuneCountInString(app.composer.text); got != _maxDraftChars+999 {
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
	if got := utf8.RuneCountInString(app.composer.text); got != _maxDraftChars+1000 {
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
	app.composer.text = strings.Repeat("x", _maxDraftChars)
	app.composer.cursor = _maxDraftChars
	app.submitComposer()
	if got := sent(); got != "reached whole" {
		t.Fatalf("the send boundary sent %q, want the draft at the cap whole", got)
	}
	sentN := len(app.composer.history)
	// one keystroke past the cap is refused: the draft holds the cap, no
	// character of the user's text was dropped
	app.clearComposer()
	app.composer.text = strings.Repeat("x", _maxDraftChars)
	app.composer.cursor = _maxDraftChars
	app.composerKey("y", tea.KeyPressMsg{Text: "y"})
	if got := utf8.RuneCountInString(app.composer.text); got != _maxDraftChars || strings.ContainsRune(app.composer.text, 'y') {
		t.Fatalf("the past-cap keystroke left %d runes, want the cap kept whole", got)
	}
	// a 20001-char paste rides its token whole and enter refuses it whole
	big := strings.Repeat("p", _maxDraftChars+1)
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
		{"a slash draft wraps like any line", "/" + nine + nine, "up", "/" + nine + nine, 11, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, _ = app.composerRows(10, "")
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
	for i := range _historyCap {
		c2.pushHistory(strconv.Itoa(i))
	}
	if len(c2.history) != _historyCap || c2.history[0] != "0" {
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
			if soft[i] {
				marks += "~"
			} else {
				marks += "."
			}
		}
		if strings.Join(got, "|") != c.rows || row != c.row || col != c.col || marks != c.soft {
			t.Errorf("%s: rows %q cursor %d,%d soft %q", c.name, got, row, col, marks)
		}
	}
}

// TestComposerGutter: a draft starts at the frame's inner edge with no
// gutter down its rows, a slash draft among them.
func TestComposerGutter(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = "one\ntwo"
	rows, _, _ := app.composerRows(40, "")
	if len(rows) != 2 {
		t.Fatalf("a two-line draft drew %d rows", len(rows))
	}
	for i, want := range []string{"one", "two"} {
		if got := hostapi.SegsText(rows[i]); got != want || hostapi.SegsWidth(rows[i]) != 3 {
			t.Fatalf("row %d is %q over %d cells, want %q in 3: no gutter", i, got, hostapi.SegsWidth(rows[i]), want)
		}
	}
	app.composer.text = "/ls\ntwo"
	rows, _, _ = app.composerRows(40, "")
	if got := hostapi.SegsText(rows[0]); got != "/ls" {
		t.Fatalf("command row %+v", rows[0])
	}
	// a full row ends at the cell the cursor mark keeps free
	for _, c := range []struct {
		name, text string
		want       int
	}{
		{"plain", strings.Repeat("a", 60), 39},
		{"slash", "/" + strings.Repeat("a", 60), 39},
	} {
		app.composer.text = c.text
		row, _, _ := app.composerRows(40, "")
		if got := hostapi.SegsWidth(row[0]); got != c.want {
			t.Fatalf("the %s row is %d cells wide, want %d", c.name, got, c.want)
		}
	}
}

// TestCompletionAfterProseQuotes: a quote in prose opens nothing, so an
// unclosed apostrophe (`don't`) or quotation mark never swallows the rest
// of the line the way a shell's would, and the @ after it still triggers.
// The frame the attach grammar reads - the quote right after @ - keeps
// holding its spaces.
func TestCompletionAfterProseQuotes(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.completion.setFiles([]string{"my notes.txt", "main.go"})

	type k struct {
		key  string
		text string
	}
	send := func(ks ...k) {
		for _, x := range ks {
			app.composerKey(x.key, tea.KeyPressMsg{Text: x.text})
		}
	}

	send(k{"d", "d"}, k{"o", "o"}, k{"n", "n"}, k{"'", "'"}, k{"t", "t"}, k{" ", " "}, k{"@", "@"}, k{"m", "m"})
	if !app.completion.open || app.completion.token != "m" {
		t.Fatalf("popup after a prose apostrophe: open=%v token=%q", app.completion.open, app.completion.token)
	}
	if got := app.completion.matches; len(got) != 2 {
		t.Fatalf("token 'm' matches: %v", got)
	}
	app.composer.text, app.composer.cursor = `said "see @m`, 12
	app.updateCompletion()
	if !app.completion.open || app.completion.token != "m" {
		t.Fatalf("popup after a prose quote: open=%v token=%q", app.completion.open, app.completion.token)
	}
	app.composer.text, app.composer.cursor = `@"my no`, 7
	app.updateCompletion()
	if !app.completion.open || app.completion.token != `"my no` || len(app.completion.matches) != 1 {
		t.Fatalf("a quoted word holds its space: open=%v token=%q matches=%v", app.completion.open, app.completion.token, app.completion.matches)
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

// TestCompletionCycles: the @ popup's menu wraps at its ends - up from the
// first row lands on the last, down from the last back on the first.
func TestCompletionCycles(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	// zz matches nothing the host offers, so only the seeded files answer
	app.completion.setFiles([]string{"zz1.go", "zz2.go", "zz3.go"})
	app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	app.composerKey("z", tea.KeyPressMsg{Text: "z"})
	app.composerKey("z", tea.KeyPressMsg{Text: "z"})
	if !app.completion.shown() || app.completion.count() != 3 {
		t.Fatalf("popup: shown=%v rows=%d", app.completion.shown(), app.completion.count())
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if app.completion.selected != 2 {
		t.Fatalf("up from the first row selected %d, want the last", app.completion.selected)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if app.completion.selected != 0 {
		t.Fatalf("down from the last row selected %d, want the first", app.completion.selected)
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
		{"internal/", "internal/", "", true},
		{"internal/u", "", "", false},
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
	// tab hands the row's canonical spelling back: the bare head joins its
	// separator only through the completion
	app.completion.selected = 0
	app.composerKey("tab", tea.KeyPressMsg{})
	if got := app.composer.text; got != "@~/Downloads/" || !app.completion.shown() {
		t.Fatalf("bare-head accept: %q, shown %v", got, app.completion.shown())
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
	// the narrowing searches the directory whole: a file sorting past the
	// display caps still answers by name
	for i := range 250 {
		if err := os.WriteFile(filepath.Join(home, "Downloads", fmt.Sprintf("aa%03d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "Downloads", "zeta file.md"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	open("@~/Downloads/zeta")
	if got := labels(); len(got) != 1 || got[0] != "~/Downloads/zeta file.md" {
		t.Fatalf("narrowing past the caps: %q", got)
	}
	open("@decoy")
	if got := app.completion.paths; len(got) != 0 {
		t.Fatalf("stale listing rows: %+v", got)
	}
	// a bare-head spelling is not a path: the space keeps the quote open,
	// the same rule a word mid-typing rides
	app.composer.text, app.composer.cursor = `@"~Downloads`, 12
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	if got := app.composer.text; got != `@"~Downloads ` {
		t.Fatalf("bare tilde head stayed open as %q", got)
	}
	// esc closes the popup and ctrl+space reopens what the cursor's word
	// takes, with no edit between
	app.composer.text, app.composer.cursor = "@~/Down", 7
	app.updateCompletion()
	app.composerKey("esc", tea.KeyPressMsg{})
	if app.completion.shown() {
		t.Fatal("esc left the popup open")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	if !app.completion.shown() {
		t.Fatal("ctrl+space did not reopen the attach popup")
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
	scanned, ok := cmd().(filesScannedMsg)
	if !ok {
		t.Fatal("rescan did not return filesScannedMsg")
	}
	app.onFilesScanned(scanned)
	app.onFilesScanned(msg)
	if !slices.Contains(app.completion.files, "new.go") {
		t.Fatalf("rescan missed new.go: %v", app.completion.files)
	}

	// a directory rides the corpus, separator-suffixed, reachable by name
	app.composer.text, app.composer.cursor = "@inte", 5
	app.updateCompletion()
	if !slices.ContainsFunc(app.completion.matches, func(h hostapi.Hit) bool {
		return app.completion.files[h.Index] == "internal/"
	}) {
		t.Fatalf("dir rides the corpus: %v", app.completion.files)
	}

	// a bare relative dir prefix lists its entries, as `./` does
	app.composer.text, app.composer.cursor = "@internal/", 10
	app.updateCompletion()
	if !slices.ContainsFunc(app.completion.paths, func(p listItem) bool { return p.label == "internal/app.go" }) {
		t.Fatalf("bare dir prefix lists: %+v", app.completion.paths)
	}

	// Enter on a corpus directory row walks in: the word keeps its
	// separator and the popup stays open on the listing
	app.composer.text, app.composer.cursor = "@inte", 5
	app.updateCompletion()
	sel := -1
	for i, m := range app.completion.matches {
		if app.completion.files[m.Index] == "internal/" {
			sel = len(app.completion.extra) + i
			break
		}
	}
	if sel < 0 {
		t.Fatalf("dir row missing from the corpus: %+v", app.completion.matches)
	}
	app.completion.selected = sel
	app.acceptCompletion()
	if got := app.composer.text; got != "@internal/" || !app.completion.open {
		t.Fatalf("corpus dir accept: %q open=%v", got, app.completion.open)
	}
	if !slices.ContainsFunc(app.completion.paths, func(p listItem) bool { return p.label == "internal/app.go" }) {
		t.Fatalf("corpus dir walk-in lists: %+v", app.completion.paths)
	}

	// a quoted token narrows the corpus by the path inside the quotes
	app.composer.text, app.composer.cursor = `@"My`, 4
	app.updateCompletion()
	if len(app.completion.matches) != 1 || app.completion.files[app.completion.matches[0].Index] != "My Notes.md" {
		t.Fatalf("quoted token matches: %+v", app.completion.matches)
	}
}

// TestKindCompletion: a full kind prefix narrows the @ popup to its kind -
// @file: the corpus, @skill: the skills, @workflow: and @agent: their
// sections - where a partial prefix still suggests everything; an accept
// spells the kind out only for a name another kind answers.
func TestKindCompletion(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	dir := filepath.Join(host.Dirs.ConfigHome, "skills", "lint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: lint\ndescription: lint the tree\n---\n\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadUI(t, host, host.SkillsUserHooksStep())
	// hunt: a file and the built-in workflow; lint: a file and the
	// skill; solo.md: the files' alone
	app.completion.setFiles([]string{"hunt", "lint", "solo.md"})
	reopen := func(word string) {
		r := []rune(word)
		app.composer.text, app.composer.cursor = word, len(r)
		app.updateCompletion()
		for app.Pump() { // the fetched rows land through the queue
		}
	}
	// a partial prefix suggests every kind
	reopen("@l")
	if len(app.completion.matches) == 0 || len(app.completion.skillRows) == 0 || len(app.completion.entityRows) == 0 {
		t.Fatalf("a partial prefix narrowed: matches=%d skills=%d entities=%d",
			len(app.completion.matches), len(app.completion.skillRows), len(app.completion.entityRows))
	}
	// a full prefix filters to its kind
	reopen("@file:")
	if len(app.completion.matches) != 3 || len(app.completion.skillRows) != 0 || len(app.completion.entityRows) != 0 || app.completion.paths != nil {
		t.Fatalf("@file: matches=%d skills=%d entities=%d paths=%d",
			len(app.completion.matches), len(app.completion.skillRows), len(app.completion.entityRows), len(app.completion.paths))
	}
	reopen("@skill:")
	if len(app.completion.skillRows) == 0 || len(app.completion.matches) != 0 || len(app.completion.entityRows) != 0 {
		t.Fatalf("@skill: skills=%d matches=%d entities=%d",
			len(app.completion.skillRows), len(app.completion.matches), len(app.completion.entityRows))
	}
	reopen("@agent:")
	if len(app.completion.entityRows) == 0 || slices.ContainsFunc(app.completion.entityRows, func(r listItem) bool { return r.section != "Agents" }) {
		t.Fatalf("@agent: rows: %+v", app.completion.entityRows)
	}
	reopen("@workflow:")
	if len(app.completion.entityRows) == 0 || slices.ContainsFunc(app.completion.entityRows, func(r listItem) bool { return r.section != "Workflows" }) {
		t.Fatalf("@workflow: rows: %+v", app.completion.entityRows)
	}
	// an accept spells the kind out only where the name is shared
	reopen("@hunt")
	base := len(app.completion.extra) + len(app.completion.matches) + len(app.completion.paths) + len(app.completion.skillRows)
	app.completion.selected = base // the workflow row under the file's name
	app.acceptCompletion()
	if got := app.composer.text; got != "@workflow:hunt " {
		t.Fatalf("the shared workflow accepted %q", got)
	}
	reopen("@lint")
	app.completion.selected = len(app.completion.extra) // the file row under the skill's name
	app.acceptCompletion()
	if got := app.composer.text; got != "@file:lint " {
		t.Fatalf("the shared file accepted %q", got)
	}
	reopen("@lint")
	base = len(app.completion.extra) + len(app.completion.matches) + len(app.completion.paths)
	app.completion.selected = base // the skill row under the file's name
	app.acceptCompletion()
	if got := app.composer.text; got != "@skill:lint " {
		t.Fatalf("the shared skill accepted %q", got)
	}
	// a name one kind holds rides alone
	reopen("@solo.md")
	app.completion.selected = len(app.completion.extra)
	app.acceptCompletion()
	if got := app.composer.text; got != "@solo.md " {
		t.Fatalf("the unshared file accepted %q", got)
	}
}

// TestCompletionWalksIn: accepting a directory candidate keeps the popup
// open on the directory, in the frame the token typed, and a file accept
// closes it with its space.
func TestCompletionWalksIn(t *testing.T) {
	proj := t.TempDir()
	app := newTestApp(t, proj)
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
	if got := app.composer.text; got != `@"`+spacey+"/" || !app.completion.open {
		t.Fatalf("a spacey directory walked in as %q", got)
	}
	// the wrap leaves the quote open, so the letters after it keep
	// narrowing the listing; a file accept closes the pair
	app.composer.text += "De"
	app.composer.cursor += 2
	app.updateCompletion()
	if len(app.completion.paths) != 1 || app.completion.paths[0].label != `"`+spacey+"/Deep/" {
		t.Fatalf("letters after the open quote narrow: %+v", app.completion.paths)
	}
	// accepting the narrowed row walks in; a space then ends the word -
	// the quote closes, so prose after it rides outside the path
	app.composerKey("enter", tea.KeyPressMsg{})
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	for _, r := range "attach this" {
		app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
	}
	if got := app.composer.text; got != `@"`+spacey+`/Deep/" attach this` {
		t.Fatalf("space closed the quoted word as %q", got)
	}
	// a space inside a word still being typed is part of its name: the
	// quote stays open and the letters after it keep narrowing
	app.composerKey("esc", tea.KeyPressMsg{})
	app.composerKey("esc", tea.KeyPressMsg{})
	app.composer.text, app.composer.cursor = `@"My`, 4
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	for _, r := range "Docs/Deep" {
		app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
	}
	if got := app.composer.text; got != `@"My Docs/Deep` {
		t.Fatalf("mid-word space kept the quote open as %q", got)
	}
	// a hand-typed bare-relative word the popup never listed closes too -
	// the rule is the word's own, not the listing's
	if err := os.WriteFile(filepath.Join(proj, "notes a.txt"), []byte("n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.composer.text, app.composer.cursor = `@"notes a.txt`, 13
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	if got := app.composer.text; got != `@"notes a.txt" ` {
		t.Fatalf("bare-relative word closed as %q", got)
	}
	// a cursor inside the word must not split it
	app.composer.text, app.composer.cursor = `@"notes a.txt`, 8
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	if got := app.composer.text; got != `@"notes  a.txt` {
		t.Fatalf("mid-word cursor split the word as %q", got)
	}
	// the space rule resolves the word the way submit does - component-wise,
	// through a symlink - so a join-cleaned lookalike never closes the quote
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), filepath.Join(proj, "sym")); err != nil {
		t.Fatal(err)
	}
	app.composer.text, app.composer.cursor = `@"sym/../notes a.txt`, 20
	app.composerKey(" ", tea.KeyPressMsg{Text: " "})
	if got := app.composer.text; got != `@"sym/../notes a.txt ` {
		t.Fatalf("a join-cleaned lookalike closed the quote as %q", got)
	}
	// a name holding " rides the ' frame: the " frame the row offered would
	// end at the name's own quote and submit would attach nothing
	if err := os.MkdirAll(filepath.Join(proj, "My Docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "My Docs", `say "hi".txt`), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	open(`@"./My`)
	idx = slices.IndexFunc(app.completion.paths, func(r listItem) bool { return r.label == `"./My Docs/` })
	if idx < 0 {
		t.Fatalf("candidates: %+v", app.completion.paths)
	}
	app.completion.selected = idx
	app.acceptCompletion()
	if got := app.composer.text; got != `@"./My Docs/` || !app.completion.open {
		t.Fatalf("walked in as %q", got)
	}
	app.composer.text += "sa"
	app.composer.cursor += 2
	app.updateCompletion()
	if len(app.completion.paths) != 1 || app.completion.paths[0].label != `"./My Docs/say "hi".txt` {
		t.Fatalf("candidates: %+v", app.completion.paths)
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	if got := app.composer.text; got != `@'./My Docs/say "hi".txt' ` || app.completion.open {
		t.Fatalf("a quoted name accepted as %q", got)
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
	if got := len(app.completion.paths); got != _listMaxFiles {
		t.Fatalf("candidates: %d, want %d", got, _listMaxFiles)
	}
	var dirs int
	for _, r := range app.completion.paths {
		if strings.HasSuffix(r.label, "/") {
			dirs++
		}
	}
	if dirs != _listMaxFiles/2 {
		t.Fatalf("dirs: %d, want %d", dirs, _listMaxFiles/2)
	}
	app.completion.selected = app.completion.count() - 1
	app.acceptCompletion()
	if got := app.composer.text; got != "@"+out+fmt.Sprintf("/f%03d.txt ", _listMaxFiles/2-1) {
		t.Fatalf("the last row accepted as %q", got)
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
	// follow. A line no row stands for goes to the model, and with no model
	// the draft stays.
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
		app.updateCompletion()
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

// TestUnknownCommandIsAMessage: a "/" line that names no command goes to the
// model like any message - a pasted path among them - the popup it opened
// gone once the send is away.
func TestUnknownCommandIsAMessage(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	for _, r := range "/tmp/notes.txt" {
		app.composerKey(string(r), tea.KeyPressMsg{Text: string(r)})
	}
	if !app.completion.shown() {
		t.Fatal("the / popup did not open")
	}
	app.composerKey("enter", tea.KeyPressMsg{})
	c := app.composer
	if c.text != "/tmp/notes.txt" || c.cursor != len(c.runes()) || len(c.history) != 1 {
		t.Errorf("text %q cursor %d history %v", c.text, c.cursor, c.history)
	}
	if len(app.toasts) != 1 || !strings.HasPrefix(app.toasts[0].body, "no model yet") {
		t.Errorf("toasts %+v", app.toasts)
	}
}

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
	app.updateCompletion()
	if !app.completion.shown() {
		t.Fatal("the popup did not open")
	}
	app.composerKey("esc", tea.KeyPressMsg{}) // the esc closes the popup, the line stays
	app.composerKey("esc", tea.KeyPressMsg{}) // arms
	app.composerKey("esc", tea.KeyPressMsg{}) // and interrupts
	if !strings.HasPrefix(app.composer.text, "queued") || app.completion.shown() {
		t.Fatalf("restore left text %q, completion %v", app.composer.text, app.completion.shown())
	}
	// the restored draft (the queue and the line) recalls from history
	app.clearComposer()
	app.composerKey("ctrl+p", tea.KeyPressMsg{})
	if !strings.HasPrefix(app.composer.text, "queued") {
		t.Fatalf("history does not hold the restored text: %q", app.composer.text)
	}
}

// TestComposerEditRecomputes: every composer write from Lua recomputes the
// popups, so a set or insert never leaves one keyed at stale rune offsets.
func TestComposerEditRecomputes(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 12)
	app.composer.text, app.composer.cursor = "/he", 3
	app.updateCompletion()
	if !app.completion.shown() {
		t.Fatal("the popup did not open")
	}
	runLua(t, app, `zemi.composer.set("hello")`)
	if app.completion.shown() {
		t.Fatal("set left the popup open over the new text")
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
