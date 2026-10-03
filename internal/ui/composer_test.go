package ui

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

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
	c := &composerState{text: "draft"}
	c.histPrev()
	if c.text != "draft" {
		t.Fatalf("an empty history recalled %q", c.text)
	}
	for _, s := range []string{"one", "two"} {
		c.pushHistory(s, 50)
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

// TestComposerHiddenRows: the row under a tall draft counts the lines on the
// side the cursor left out of view.
func TestComposerHiddenRows(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = strings.Repeat("line\n", 9) + "end"
	for cursor, want := range map[int]string{0: "  … 2 more lines", len(app.composer.text): "  … 2 more lines above"} {
		app.composer.cursor = cursor
		rows := app.composerRows(40, "")
		if len(rows) != composerMaxRows+1 || hostapi.SegsText(rows[len(rows)-1]) != want {
			t.Fatalf("cursor %d: rows %d, last %q", cursor, len(rows), hostapi.SegsText(rows[len(rows)-1]))
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
		cursor                  int // the cursor cell's column, -1 for none
	}{
		{"empty", "", "", "> Ask anything", 2},
		{"typed", "h", "", "> h", 3},
		{"command draft", "/", "", "/", 2},
		{"a node has the keys", "", "side", "> Ask anything", -1},
	} {
		app.composer.text, app.composer.cursor = c.text, len(c.text)
		app.host.SetFocus(c.focus)
		segs := app.composerRows(40, "Ask anything")[0]
		cursor, col := -1, 0
		for i, s := range segs {
			if s.HL == hostapi.ChromeCursor {
				cursor = col
			}
			if i > 0 && c.text == "" && (!s.NoCopy || s.HL != "Dim" && s.HL != hostapi.ChromeCursor) {
				t.Errorf("%s: placeholder segment %+v copies or reads as text", c.name, s)
			}
			col += ansi.StringWidth(s.Text)
		}
		if got := strings.TrimRight(hostapi.SegsText(segs), " "); got != c.want || cursor != c.cursor {
			t.Errorf("%s: row %q, cursor at %d; want %q at %d", c.name, got, cursor, c.want, c.cursor)
		}
	}
}

// TestComposerGutter: rows after the first carry the same prompt, dimmed, so
// a tall draft hangs off one glyph; a shell draft says so with its own.
func TestComposerGutter(t *testing.T) {
	app := newTestApp(t, t.TempDir())
	app.composer.text = "one\ntwo"
	rows := app.composerRows(40, "")
	if len(rows) != 2 || rows[0][0].Text != "> " || rows[1][0].Text != "> " || rows[1][0].HL != "Dim" {
		t.Fatalf("rows %+v", rows)
	}
	app.composer.text = "!ls"
	if got := app.composerRows(40, "")[0][0]; got.Text != "! " || got.HL != "PromptShell" {
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
	first := hostapi.SegsWidth(app.completionRows(76, 10, 0)[0])
	if row := strings.TrimSpace(hostapi.SegsText(app.completionRows(30, 10, 0)[0])); !strings.Contains(row, "_here.txt") {
		t.Fatalf("a cut path lost its name: %q", row)
	}
	app.completion.selected = app.completion.count() - 1
	rows := app.completionRows(76, 10, 0)
	if w := hostapi.SegsWidth(rows[0]); w != first || app.completion.count() != listMaxFiles {
		t.Fatalf("width %d then %d, %d items", first, w, app.completion.count())
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
		f := fileCompleter{token: c.token}
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
			GoFn: func(_ *hostapi.Host, args string) error { ran = name + " " + args; return nil },
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
	// Esc clears the composer line and closes the palette.
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
	app, _ := bootAgentApp(t, 80, 24)
	ran := "" // the command line that ran
	for _, name := range []string{"zq", "zqa", "echo", "reloadall"} {
		if _, err := app.host.Commands.Add(&hostapi.Command{
			Name: name, NArgs: "*", Desc: "test", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
			GoFn: func(_ *hostapi.Host, args string) error { ran = name + " " + args; return nil },
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
