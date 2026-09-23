package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// newTestApp builds an App over an unbooted runtime: enough for composer,
// completion, and cmdline tests (no widgets, no Lua calls).
func newTestApp(t *testing.T, projectDir string) *App {
	t.Helper()
	log := logging.New("")
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	host := hostapi.NewHost(rt, log, config.NewStore(), config.Dirs{ProjectDir: projectDir})
	app := NewApp(host, log)
	host.SetUIHooks(app)
	return app
}

func TestComposerEdit(t *testing.T) {
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
	run("delete", func(c *composerState) { c.insert("abc"); c.cursor = 1; c.delete() }, "ac", 1)
	run("delete-eof", func(c *composerState) { c.insert("ab"); c.delete() }, "ab", 2)
	run("left-right", func(c *composerState) { c.insert("ab"); c.left(); c.left(); c.right() }, "ab", 1)
	run("left-home", func(c *composerState) { c.insert("ab"); c.left(); c.left(); c.left() }, "ab", 0)
	run("newline", func(c *composerState) { c.insert("a"); c.insert("\n"); c.insert("b") }, "a\nb", 3)
	run("backspace-joins", func(c *composerState) { c.insert("a\nb"); c.cursor = 2; c.backspace() }, "ab", 1)
	run("word", func(c *composerState) { c.insert("one two"); c.delWord() }, "one ", 4)
	run("word-punct", func(c *composerState) { c.insert("x.ts add"); c.delWord() }, "x.ts ", 5)
	run("word-to-newline", func(c *composerState) { c.insert("a\nb"); c.delWord() }, "a\n", 2)
	run("del-start", func(c *composerState) { c.insert("one\ntwo"); c.cursor = 6; c.delToLineStart() }, "one\no", 4)
	run("del-end", func(c *composerState) { c.insert("one\ntwo"); c.cursor = 1; c.delToLineEnd() }, "o\ntwo", 1)
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
}

func TestComposerLineMoves(t *testing.T) {
	c := &composerState{}
	c.insert("one\ntwoth\nthree")
	c.cursor = 5 // "o" of "twoth"
	if !c.up() || c.cursor != 1 {
		t.Fatalf("up: cursor=%d, want 1", c.cursor)
	}
	if c.up() {
		t.Fatalf("up from first line moved")
	}
	c.cursor = 5
	if !c.down() || c.cursor != 11 { // col 1 lands after 't' of "three"
		t.Fatalf("down: cursor=%d, want 11", c.cursor)
	}
	// A shorter line below keeps the column when possible.
	c2 := &composerState{}
	c2.insert("abcdef\nx\nlonger")
	c2.cursor = 3
	if !c2.down() || c2.cursor != 8 {
		t.Fatalf("down to short line: cursor=%d, want 8", c2.cursor)
	}
}

func TestComposerHistory(t *testing.T) {
	c := &composerState{}
	if c.histPrev() {
		t.Fatal("empty history moved")
	}
	for _, s := range []string{"one", "two"} {
		c.pushHistory(s, 50)
	}
	c.text, c.cursor = "draft", 5
	c.histPrev()
	if c.text != "two" {
		t.Fatalf("histPrev: %q", c.text)
	}
	c.histPrev()
	if c.text != "one" {
		t.Fatalf("histPrev again: %q", c.text)
	}
	if c.histPrev() {
		t.Fatal("histPrev past oldest moved")
	}
	if !c.histNext() || c.text != "two" {
		t.Fatalf("histNext: %q", c.text)
	}
	if !c.histNext() || c.text != "draft" {
		t.Fatalf("histNext to draft: %q", c.text)
	}
	if c.histNext() {
		t.Fatal("histNext past draft moved")
	}
	// Cap and duplicate suppression.
	c2 := &composerState{}
	c2.pushHistory("same", 2)
	c2.pushHistory("same", 2)
	c2.pushHistory("other", 2)
	if len(c2.history) != 2 || c2.history[0] != "same" {
		t.Fatalf("cap: %#v", c2.history)
	}
	// Zero cap disables.
	c2.pushHistory("x", 0)
	if len(c2.history) != 0 {
		t.Fatalf("disabled: %#v", c2.history)
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

func TestComposerHiddenRows(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = strings.Repeat("line\n", 9) + "end"
	app.composer.cursor = len(app.composer.text)
	rows := app.composerRows(40)
	if len(rows) != composerMaxRows+1 || plainSegs(rows[len(rows)-1]) != "  +2 lines" {
		t.Fatalf("rows %d, last %q", len(rows), plainSegs(rows[len(rows)-1]))
	}
}

// TestComposerGutter: rows after the first hang off a gutter, and a shell
// draft says so with its own glyph.
func TestComposerGutter(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = "one\ntwo"
	rows := app.composerRows(40)
	if len(rows) != 2 || rows[0][0].Text != "> " || rows[1][0].Text != "│ " {
		t.Fatalf("rows %+v", rows)
	}
	app.composer.text = "!ls"
	if got := app.composerRows(40)[0][0]; got.Text != "! " || got.HL != "PromptShell" {
		t.Fatalf("shell prompt %+v", got)
	}
}

func TestComposerKeysMultilineOff(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.host.Settings.SetQuiet("ui.composer.multiline", false)
	app.composerKey("h", tea.KeyPressMsg{Code: 'h', Text: "h"})
	app.composerKey("alt+enter", tea.KeyPressMsg{})
	if app.composer.text != "h" {
		t.Fatalf("newline inserted with multiline off: %q", app.composer.text)
	}
}

func TestCompletionFlow(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.completion.files = []string{"cmd/root.go", "internal/ui/app.go", "main.go"}

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
	if got := app.completion.matches; len(got) != 1 || got[0] != "internal/ui/app.go" {
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

func TestCompletionScan(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{".git", "node_modules", "internal"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"main.go", "internal/app.go", ".git/config", "node_modules/x.js", ".gitignore"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("node_modules/\n"), 0o600); err != nil {
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
	for _, want := range []string{"main.go", "internal/app.go"} {
		if !slices.Contains(app.completion.files, want) {
			t.Fatalf("missing %s in %v", want, app.completion.files)
		}
	}
	for _, gone := range []string{".git/config", "node_modules/x.js"} {
		if slices.Contains(app.completion.files, gone) {
			t.Fatalf("skipped dir leaked: %s", gone)
		}
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
}

// TestCompletionPartialNote: a scan that filled the cap says the list is
// partial instead of claiming the repo holds no match.
func TestCompletionPartialNote(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composerKey("@", tea.KeyPressMsg{Text: "@"})
	files := make([]string, scanMaxFiles)
	for i := range files {
		files[i] = "pkg/f" + strconv.Itoa(i) + ".go"
	}
	app.onFilesScanned(filesScannedMsg{gen: app.completion.walkGen, files: files})
	app.composerKey("q", tea.KeyPressMsg{Text: "q"}) // a letter no path holds
	rows := app.completionRows(76, 10)
	if len(rows) == 0 {
		t.Fatal("no popup rows")
	}
	last := rows[len(rows)-1][0].Text
	if !strings.Contains(last, "more files") {
		t.Fatalf("last row = %q, want the partial note", last)
	}
}

func TestMatchScore(t *testing.T) {
	cases := []struct {
		path, q string
		want    int
		ok      bool
	}{
		{"internal/ui/app.go", "app", 0, true},
		{"internal/ui/app.go", "ui/app", 2, true},
		{"internal/ui/app.go", "iap", 2, true},
		{"main.go", "zz", 0, false},
		{"README.md", "readme", 0, true},
	}
	for _, c := range cases {
		got, ok := matchScore(c.path, c.q)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("matchScore(%q,%q) = %d,%v want %d,%v", c.path, c.q, got, ok, c.want, c.ok)
		}
	}
}

func TestPaletteComplete(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	ran := false
	add := func(name string, fn func(h *hostapi.Host, args string) error) {
		if _, err := app.host.Commands.Add(&hostapi.Command{
			Name: name, Desc: "test", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1), GoFn: fn,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("q", nil)
	add("qa", nil)
	add("echo", func(h *hostapi.Host, args string) error { ran = args == "hi"; return nil })

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
	send(k{"q", "q"})
	if len(app.palette.matches) != 2 {
		t.Fatalf("matches after /q: %d", len(app.palette.matches))
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
	// Esc clears the composer line and closes the palette.
	send(k{"backspace", ""}, k{"backspace", ""})
	app.composerKey("esc", tea.KeyPressMsg{})
	if app.palette.open || app.composer.text != "/q" {
		t.Fatalf("esc: open=%v text=%q", app.palette.open, app.composer.text)
	}

	// Submitting a unique command runs it with the argument string.
	app.composer.text, app.composer.cursor = "/echo hi", 8
	app.updatePalette()
	app.composerKey("enter", tea.KeyPressMsg{})
	if !ran || app.composer.text != "" {
		t.Fatalf("submit: ran=%v text=%q", ran, app.composer.text)
	}

	// A partial name runs the highlighted command, arguments kept.
	ran = false
	app.composer.text, app.composer.cursor = "/ec hi", 3
	app.updatePalette()
	app.composerKey("enter", tea.KeyPressMsg{})
	if !ran || app.composer.text != "" {
		t.Fatalf("partial submit: ran=%v text=%q", ran, app.composer.text)
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

// TestPasteToken: a large paste stands in the composer as one token that
// backspace removes whole and submit expands; small pastes stay inline.
// TestSubmitFailed: the test app has no model, so every send fails, and
// the same failure twice is one toast.
func TestSubmitFailed(t *testing.T) {
	for _, text := range []string{"send with no model keeps the text", "an empty popup still sends @nope.txt"} {
		app := newTestApp(t, t.TempDir())
		app.completion.files = []string{"main.go"}
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
