package win

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"strings"
	"testing"
	"time"

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
	time.Sleep(300 * time.Millisecond)
	if text := w.Text(); strings.Contains(text, "X") {
		t.Fatalf("drew inside the update:\n%s", text)
	}
	if text, ok := w.WaitText(ctx, "X"); !ok {
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
