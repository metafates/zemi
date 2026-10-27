package hostapi

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// newWindow boots one window on a shared state home and project, the shape
// of two zemi processes on one repository. Its turns run on a reply stub,
// returned for sendTurns.
func newWindow(t *testing.T, stateHome, projectDir string) (*Host, *replyStubModel) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: zemiruntime.FS})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		StateHome:  stateHome,
		DataHome:   t.TempDir(),
		ProjectDir: projectDir,
	})
	h.ReleaseBuilder()
	h.SetUIHooks(&fakeUI{})
	m := replyStub(t, h, "ok")
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), lua.RequireStep("zemi.tui")}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, m
}

// windowText is one window's transcript texts, the order it draws.
func windowText(h *Host) []string {
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	out := make([]string, 0, len(h.Session().Transcript))
	for _, m := range h.Session().Transcript {
		out = append(out, m.Text)
	}
	return out
}

// notifyUI records the toasts the host fires.
type notifyUI struct {
	fakeUI
	mu  sync.Mutex
	got []string
}

func (u *notifyUI) Notify(_ slog.Level, title, _ string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.got = append(u.got, title)
}

func (u *notifyUI) titles() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.got)
}

// TestSyncChatPullsTheOtherWindow: two windows on one repository read as the
// one chat the log keeps - each window's SyncChat splices the other's rows in
// at their place, in the order a restart reads, and a foreign reset reloads
// the window whole.
func TestSyncChatPullsTheOtherWindow(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, ma := newWindow(t, state, proj)
	ui := &notifyUI{}
	a.SetUIHooks(ui)
	b, mb := newWindow(t, state, proj) // the same state home and project, one store
	if a.origin == b.origin {
		t.Fatal("the two windows share a writer id, want distinct ones")
	}
	sendTurns(t, a, ma, "echo one")
	sendTurns(t, a, ma, "echo two")
	b.SyncChat()
	got := windowText(b)
	if len(got) < 4 || !strings.Contains(got[0], "echo one") || !strings.Contains(got[2], "echo two") {
		t.Fatalf("b's window after the sync = %q", got)
	}
	// interleaved writes land in each window at their log order
	sendTurns(t, b, mb, "between")
	sendTurns(t, a, ma, "from a")
	a.SyncChat()
	got = windowText(a)
	if len(got) < 6 || !strings.Contains(got[4], "between") {
		t.Fatalf("a's window spliced the foreign rows wrong = %q", got)
	}
	b.SyncChat()
	waitFor(t, func() error {
		got = windowText(b)
		if len(got) < 8 {
			return errors.New("b's sync has not caught up")
		}
		if !strings.Contains(got[6], "from a") {
			return errors.New("b's window lacks a's turn at its place")
		}
		return nil
	})
	for _, m := range b.Session().Transcript {
		if m.Seq == NoSeq {
			t.Fatalf("b's window holds an unstamped row: %+v", m)
		}
	}
	// a foreign reset wipes under this window: it reloads whole, and the
	// reset names itself
	if err := b.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	if got := windowText(a); len(got) != 0 {
		t.Fatalf("a's window after the foreign reset = %q, want empty", got)
	}
	if got := ui.titles(); len(got) == 0 || got[len(got)-1] != "Chat reset by another window" {
		t.Fatalf("a's toasts after the foreign reset = %q", got)
	}
	sendTurns(t, a, ma, "fresh after the reset")
	b.SyncChat()
	got = windowText(b)
	if len(got) < 2 || !strings.Contains(got[0], "fresh after the reset") {
		t.Fatalf("b's window after the reset's first turn = %q", got)
	}
}

// TestSyncChatLeavesOwnStampingAlone: a window's own rows stamp through the
// pairing exactly as without a second window, foreign rows around them or not.
func TestSyncChatLeavesOwnStampingAlone(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, ma := newWindow(t, state, proj)
	b, mb := newWindow(t, state, proj)
	sendTurns(t, a, ma, "a first")
	sendTurns(t, b, mb, "b between")
	sendTurns(t, a, ma, "a second")
	a.SyncChat()
	got := a.Session().Transcript
	if got[0].Seq != 0 || got[1].Seq != 1 {
		t.Fatalf("a's first turn lost its seqs = %+v", got[:2])
	}
	if !strings.Contains(got[2].Text, "b between") || got[2].Seq != 2 {
		t.Fatalf("the foreign turn did not splice at its place = %+v", got[2])
	}
	if got[3].Seq != 3 {
		t.Fatalf("a's second turn lost its seq = %+v", got[3])
	}
	for i, m := range got {
		// b's user row (seq 2) and its reply (seq 3) are the spliced pair
		if m.Foreign != (m.Seq == 2 || m.Seq == 3) {
			t.Fatalf("a's row %d (seq %d) foreign = %v, want the spliced pair alone", i, m.Seq, m.Foreign)
		}
	}
	// a window booting late reads the same rows unmarked: the mark is for
	// rows that arrived while this window watched, not for the log's
	c, _ := newWindow(t, state, proj)
	for i, m := range c.Session().Transcript {
		if m.Foreign {
			t.Fatalf("c's boot row %d carries the foreign mark: %+v", i, m)
		}
	}
}

// TestSyncChatToastsADraftCollision: a draft in the composer when another
// window's rows arrive gets one toast, and an unchanged draft never a second.
func TestSyncChatToastsADraftCollision(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	ui := &notifyUI{}
	a.SetUIHooks(ui)
	b, mb := newWindow(t, state, proj)
	a.SetComposerSnapshot("a draft", 6)
	sendTurns(t, b, mb, "from b")
	a.SyncChat()
	if got := ui.titles(); len(got) != 1 || got[0] != "Another window sent a message" {
		t.Fatalf("a's toasts = %q, want the one collision toast", got)
	}
	a.SyncChat()
	if got := ui.titles(); len(got) != 1 {
		t.Fatalf("a's toasts after a quiet sync = %q, want no second", got)
	}
	sendTurns(t, b, mb, "from b again")
	a.SyncChat()
	if got := ui.titles(); len(got) != 1 {
		t.Fatalf("a's toasts over the unchanged draft = %q, want no second", got)
	}
	a.SetComposerSnapshot("a draft, edited", 8)
	sendTurns(t, b, mb, "from b once more")
	a.SyncChat()
	if got := ui.titles(); len(got) != 2 {
		t.Fatalf("a's toasts over the edited draft = %q, want a second", got)
	}
}

// TestSyncChatWithoutAStore: a window with no store syncs into nothing, like
// every other chat path.
func TestSyncChatWithoutAStore(t *testing.T) {
	h := newTestHostUI(t)
	h.SyncChat() // must not panic
	if got := windowText(h); len(got) != 0 {
		t.Fatalf("a storeless window drew %q", got)
	}
}
