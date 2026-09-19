package win

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestWindowRunsReadsAndTypes(t *testing.T) {
	m := &Manager{}
	exited := make(chan *Window, 1)
	m.OnExit = func(w *Window) { exited <- w }
	w, err := m.Open(Spec{Cmd: "printf 'hello \\033[1mbold\\033[0m \\033[38;5;202mx\\033[48;2;1;2;3my\\033[0m\\n'; read line; echo got:$line; exit 3", Kind: KindFloat, Title: "t", Env: os.Environ(), Cols: 20, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(w.Text(), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("screen never showed %q:\n%s", want, w.Text())
	}
	waitFor("hello bold xy")
	scr := w.Screen()
	got := map[string]string{}
	for _, sp := range scr.Rows[0] {
		got[sp.Text] = sp.SGR
	}
	if got["bold"] != "1" || got["x"] != "38;5;202" || got["y"] != "38;5;202;48;2;1;2;3" {
		t.Fatalf("styles: %+v", scr.Rows[0])
	}
	if !w.Exited() && w.ExitCode() != -1 {
		t.Fatal("running window must report -1")
	}
	w.SendKey(uv.KeyPressEvent{Code: 'x', Text: "x"})
	w.SendKey(uv.KeyPressEvent{Code: uv.KeyEnter})
	waitFor("got:x")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil || w.ExitCode() != 3 {
		t.Fatalf("wait: %v code %d", err, w.ExitCode())
	}
	select {
	case got := <-exited:
		if got != w {
			t.Fatal("OnExit got another window")
		}
	case <-time.After(time.Second):
		t.Fatal("OnExit never ran")
	}
	if len(m.List()) != 1 || !m.Close(w.ID) || len(m.List()) != 0 || m.Close(w.ID) {
		t.Fatal("close bookkeeping")
	}
}

// TestColorQueries: a program asking for the default colors gets the host's,
// both those known when it opened and those reported later.
func TestColorQueries(t *testing.T) {
	m := &Manager{}
	m.SetHostColor(11, color.RGBA{0x12, 0x34, 0x56, 0xff})
	query := `printf '\033]%d;?\007'; head -c 24 | tr -d '\033\007'; printf '\r\n'`
	w, err := m.Open(Spec{Cmd: "stty raw -echo; " + fmt.Sprintf(query, 11) + "; head -c 1 >/dev/null; " + fmt.Sprintf(query, 10) + "; sleep 5", Env: os.Environ(), Cols: 40, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(w.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if text, ok := w.WaitText(ctx, "]11;rgb:1212/3434/5656"); !ok {
		t.Fatalf("background at open:\n%s", text)
	}
	m.SetHostColor(10, color.RGBA{0xab, 0xcd, 0xef, 0xff})
	w.SendKey(uv.KeyPressEvent{Code: 'x', Text: "x"})
	if text, ok := w.WaitText(ctx, "]10;rgb:abab/cdcd/efef"); !ok {
		t.Fatalf("foreground reported later:\n%s", text)
	}
}

// TestKittyKeyboardFlag: the program queries, pushes and pops flag 1, and
// esc reaches it as CSI 27 u only while the flag is on.
func TestKittyKeyboardFlag(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: `stty raw -echo; printf '\033[?u\033[>1u\033[?u'; head -c 10 | tr '\033' E; printf ' A\r\n'; head -c 5 | tr '\033' E; printf '\033[<u B\r\n'; head -c 1 | tr '\033' E; printf ' C'; sleep 5`, Env: os.Environ(), Cols: 40, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(w.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for _, want := range []string{"E[?0uE[?1u A", "E[27u B", "E C"} {
		if text, ok := w.WaitText(ctx, want); !ok {
			t.Fatalf("want %q:\n%s", want, text)
		}
		w.SendKey(uv.KeyPressEvent{Code: uv.KeyEscape})
	}
}

// TestClipboard: a window allowed the clipboard sets it with OSC 52 and lists
// 52 in DA1; a read gets no answer, and any other window sets nothing.
func TestClipboard(t *testing.T) {
	for _, c := range []struct {
		allowed bool
		da1     string
		clip    string
	}{
		{true, "E[?62;1;6;22;52c", "hello"},
		{false, "E[?62;1;6;22c", ""},
	} {
		m := &Manager{}
		cmd := fmt.Sprintf(`stty raw -echo; printf '\033]52;c;?\007\033[c'; head -c %d | tr '\033' E; printf '\033]52;c;aGVsbG8=\007 END'; sleep 5`, len(c.da1))
		w, err := m.Open(Spec{Cmd: cmd, Clipboard: c.allowed, Env: os.Environ(), Cols: 40, Rows: 5})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		if text, ok := w.WaitText(ctx, c.da1+" END"); !ok {
			t.Fatalf("allowed %v:\n%s", c.allowed, text)
		}
		cancel()
		if got := m.TakeClipboard(); got != c.clip {
			t.Errorf("allowed %v: clipboard %q", c.allowed, got)
		}
		m.Close(w.ID)
	}
}

// TestSynchronizedOutput: mode 2026 reports as known, and a window shows the
// screen from before an update until it ends or a second passes.
func TestSynchronizedOutput(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: `stty raw -echo; printf '\033[?2026$p'; head -c 11 | tr '\033' E; head -c 1 >/dev/null; printf '\033[?2026h X'; sleep 5`, Env: os.Environ(), Cols: 40, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(w.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if text, ok := w.WaitText(ctx, "E[?2026;2$y"); !ok {
		t.Fatalf("mode report:\n%s", text)
	}
	w.SendKey(uv.KeyPressEvent{Code: 'x', Text: "x"})
	// the test moves the update's start instead of waiting out the second, so
	// neither check depends on when this goroutine gets to run
	drawn := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		if c := w.term.CellAt(12, 0); c == nil || c.Content != "X" || w.syncSince.IsZero() {
			return false
		}
		w.syncSince = time.Now().Add(time.Hour)
		return true
	}
	for !drawn() {
		select {
		case <-ctx.Done():
			t.Fatalf("the update never arrived:\n%s", w.Text())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if text := w.Text(); strings.Contains(text, "X") {
		t.Fatalf("drew inside the update:\n%s", text)
	}
	w.mu.Lock()
	w.syncSince = time.Now().Add(-time.Second)
	w.mu.Unlock()
	if text := w.Text(); !strings.Contains(text, "X") {
		t.Fatalf("an update never ended must show after a second:\n%s", text)
	}
}

func TestWindowKillsOnClose(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: "sleep 30", Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	w.Resize(30, 10)
	if c, r := w.Size(); c != 30 || r != 10 {
		t.Fatalf("size %dx%d", c, r)
	}
	m.Close(w.ID)
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("closed window still running")
	}
	if _, err := m.Open(Spec{Kind: "tab", Cmd: "true"}); err == nil {
		t.Fatal("unknown kind must refuse")
	}
}

func TestSignalledWindowExits(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: "kill -9 $$", Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !w.Exited() || w.ExitCode() != 128+9 {
		t.Fatalf("exited %v code %d", w.Exited(), w.ExitCode())
	}
}

func TestWindowEnvHasNoHostTerminal(t *testing.T) {
	m := &Manager{}
	env := []string{"TERM_PROGRAM=ghostty", "GHOSTTY_BIN_DIR=/x", "TMUX=/t,1,0", "TMUX_PANE=%1", "LC_TERMINAL=iTerm2", "HOME=/h", "PATH=" + os.Getenv("PATH")}
	w, err := m.Open(Spec{Cmd: "env; echo END", Env: env, Cols: 60, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(w.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	text, ok := w.WaitText(ctx, "END")
	if !ok || !strings.Contains(text, "HOME=/h") {
		t.Fatalf("env never printed:\n%s", text)
	}
	for _, bad := range []string{"TERM_PROGRAM", "GHOSTTY", "TMUX", "LC_TERMINAL"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%s leaked into the window:\n%s", bad, text)
		}
	}
}

// TestRepliesAfterExit:emulator replies written after the program is gone
// (a late cursor report, say) must not block, or mu stays held for good.
func TestRepliesAfterExit(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: "true", Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	<-w.Done()
	done := make(chan struct{})
	go func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for range 3 {
			w.term.SendText("\x1b[1;1R")
		}
		close(done)
	}()
	select {
	case <-done:
		m.Close(w.ID)
	case <-time.After(3 * time.Second):
		t.Fatal("reply write blocked after exit")
	}
}

// TestPasteToIdleProgram: a program that never reads stdin fills the pty,
// and neither the paste nor the next frame may wait on it. Raw mode, since
// a canonical tty may discard input past its line limit instead of blocking.
func TestPasteToIdleProgram(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: "stty raw -echo; sleep 30", Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(w.ID)
	done := make(chan struct{})
	go func() {
		w.Paste(strings.Repeat("x", 1<<20))
		w.Screen()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("paste or Screen blocked on a program that never reads")
	}
}

// TestSendTextPastesLines: one line types as it is, several lines go in as
// one paste, bracketed when the program asked for that.
func TestSendTextPastesLines(t *testing.T) {
	for _, c := range []struct {
		name    string
		mode    string
		send    string
		bytes   int
		bracket bool
	}{
		{"several lines, program wants brackets", `\033[?2004h`, "one\ntwo\n", 20, true},
		{"several lines, program wants none", "", "one\ntwo\n", 8, false},
		{"one line types raw", `\033[?2004h`, "one\n", 4, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &Manager{}
			defer m.CloseAll()
			cmd := fmt.Sprintf(`stty raw -echo; printf '%sRDY\r\n'; head -c %d | tr '\033' E; printf ' END'; sleep 5`, c.mode, c.bytes)
			w, err := m.Open(Spec{Cmd: cmd, Env: os.Environ(), Cols: 40, Rows: 6})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if text, ok := w.WaitText(ctx, "RDY"); !ok {
				t.Fatalf("program never started:\n%s", text)
			}
			if err := w.SendText(c.send); err != nil {
				t.Fatal(err)
			}
			text, ok := w.WaitText(ctx, "END")
			if !ok {
				t.Fatalf("program never read it all:\n%s", text)
			}
			if !strings.Contains(text, "one") || strings.Contains(text, "E[200~") != c.bracket {
				t.Fatalf("bracketed=%v:\n%s", c.bracket, text)
			}
		})
	}
}

// TestTextWindow: a text window holds text instead of a program, takes a
// replacement and an append in place, keeps its scroll inside what is
// drawn, and refuses what only a program can take.
func TestTextWindow(t *testing.T) {
	m := &Manager{}
	w, err := m.OpenText(Spec{Text: "hello", Markdown: true, Kind: KindFloat})
	if err != nil {
		t.Fatal(err)
	}
	if !w.IsText() || w.Exited() || w.Title() != "text" {
		t.Fatalf("text window: exited=%v title=%q", w.Exited(), w.Title())
	}
	if text, md := w.Content(); text != "hello" || !md {
		t.Fatalf("content = %q, markdown %v", text, md)
	}
	if err := w.SetText("one", false); err != nil {
		t.Fatal(err)
	}
	// an escape sequence in the body must not reach the frame
	if err := w.SetText("\n\x1b[31mtwo", true); err != nil {
		t.Fatal(err)
	}
	if w.Text() != "one\ntwo" {
		t.Fatalf("body = %q", w.Text())
	}
	if err := w.SendText("x"); err == nil {
		t.Fatal("a text window took program input")
	}
	// a terminal reporting its colors must not reach the emulator a text window has none of
	m.SetHostColor(11, color.RGBA{0x12, 0x34, 0x56, 0xff})
	w.Scroll(5)
	if top := w.Top(2); top != 2 {
		t.Fatalf("scroll clamped to %d, want 2", top)
	}
	// the view sits at the end, so it follows text that grew
	if top := w.Top(9); top != 9 {
		t.Fatalf("top %d, want the end at 9", top)
	}
	w.Scroll(-3)
	if top, stuck := w.Top(4), w.Top(9); top != 4 || stuck != 4 {
		t.Fatalf("clamping must stick: top %d then %d, want 4", top, stuck)
	}
	w.SendKey(uv.KeyPressEvent{Code: uv.KeyHome})
	if top := w.Top(9); top != 0 {
		t.Fatalf("home scrolled to %d, want 0", top)
	}
	m.Close(w.ID)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("a closed text window must stop its waiters: %v", err)
	}
}

// TestTextTailFollows: an appended panel stays on its newest lines, a
// replacement goes back to the top, and a view the user scrolled up stays
// put until they scroll back to the end.
func TestTextTailFollows(t *testing.T) {
	var m Manager
	w, err := m.OpenText(Spec{Text: "l1\nl2\nl3\n"})
	if err != nil {
		t.Fatal(err)
	}
	// the UI draws three lines and Top reports the first one it shows
	draw := func(lines int) int { return w.Top(max(lines-3, 0)) }
	draw(3)
	for _, c := range []struct {
		name string
		do   func() int
		want int
	}{
		{"append follows", func() int { _ = w.SetText("l4\nl5\n", true); return draw(5) }, 2},
		{"scrolled up stays", func() int { w.Scroll(-2); _ = w.SetText("l6\n", true); return draw(6) }, 0},
		{"scrolled back follows", func() int { w.Scroll(99); draw(6); _ = w.SetText("l7\n", true); return draw(7) }, 4},
		{"replacement starts at the top", func() int { _ = w.SetText("a\nb\nc\nd\ne\n", false); return draw(5) }, 0},
	} {
		if got := c.do(); got != c.want {
			t.Fatalf("%s: view starts at line %d, want %d", c.name, got, c.want)
		}
	}
	// a trail keeps only its newest lines, whole
	for range 4 {
		_ = w.SetText(strings.Repeat("a line of a trail\n", 5000), true)
	}
	text, _ := w.Content()
	if len(text) > maxBody || !strings.HasPrefix(text, "a line of a trail\n") {
		t.Fatalf("body is %d bytes, starts %q", len(text), text[:min(len(text), 30)])
	}
	// one unbroken line has no line to drop, and the cap still holds
	for range 4 {
		_ = w.SetText(strings.Repeat("é", 100_000), true)
	}
	if text, _ = w.Content(); len(text) > maxBody || !utf8.ValidString(text) {
		t.Fatalf("body is %d bytes, valid=%v", len(text), utf8.ValidString(text))
	}
}

// TestCloseDoesNotWait: a program that ignores SIGHUP is killed on a
// goroutine, so the UI thread that closed the window keeps drawing.
func TestCloseDoesNotWait(t *testing.T) {
	m := &Manager{}
	w, err := m.Open(Spec{Cmd: "trap '' HUP; sleep 30", Env: os.Environ(), Cols: 20, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if !m.Close(w.ID) {
		t.Fatal("close")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("close blocked for %s", took)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("the program outlived its window: %v", err)
	}
}
