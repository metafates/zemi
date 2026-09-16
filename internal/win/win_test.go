package win

import (
	"context"
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

// TestRepliesAfterExit: emulator replies written after the program is gone
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
