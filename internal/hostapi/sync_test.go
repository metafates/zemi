package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
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

// TestSyncChatReplacesAStaleInheritedGoal: a window that adopted a gone
// writer's goal drops the stale copy when the writer returns with a new one,
// and the live row keeps standing - the stale copy never deletes it.
func TestSyncChatReplacesAStaleInheritedGoal(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	peerHeartbeat(t, a)
	if err := a.SetGoal("goal one holds"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a)
	b, _ := newWindow(t, state, proj)
	peerHeartbeat(t, b)
	b.SyncChat()
	if g, ok := b.Goal(); !ok || g.Condition != "goal one holds" {
		t.Fatalf("b loaded %+v, want goal one", g)
	}

	// the writer returns: fresh heartbeat, and the goal replaced
	peerHeartbeat(t, a)
	if err := a.SetGoal("goal two holds"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a)

	// b's next sync sees a row that names another goal: the stale copy drops
	// and the live goal takes its place
	b.SyncChat()
	g, ok := b.Goal()
	if !ok || g.Condition != "goal two holds" {
		t.Fatalf("b holds %+v, want the writer's goal two", g)
	}
	if raw, err := a.DB.GetKV(goalKV); err != nil || !strings.Contains(raw, "goal two holds") {
		t.Fatalf("the live goal's row = %q (%v), want goal two standing", raw, err)
	}
}

// TestSyncChatSplicesAgainstTheTurn: the live append's trim, the settled
// append's trim and the sync's splice contend on one lock order (the agents
// lock leads the transcript lock); the invariants are the window bound and
// the cut accounting.
func TestSyncChatSplicesAgainstTheTurn(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	h, _ := newWindow(t, state, proj)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the live append's trim
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ag := h.agents
			ag.mu.Lock()
			ag.textBuf = "live"
			ag.mu.Unlock()
			h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: fmt.Sprintf("live %d", i)})
		}
	}()
	go func() { // the settled append's trim
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("settled %d", i)})
		}
	}()
	go func() { // the sync tick
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.SyncChat()
		}
	}()
	<-time.After(2 * time.Second)
	close(stop)
	wg.Wait()
	tr := h.Session().Transcript
	if len(tr) > chatWindowRows {
		t.Fatalf("the window holds %d rows, bound %d", len(tr), chatWindowRows)
	}
	if cut := h.trCut.Load(); cut+int64(len(tr)) < chatWindowRows {
		t.Fatalf("cut %d + window %d shed rows", cut, len(tr))
	}
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
// at their place, in the order a restart reads, and the writer's reset
// reloads the window whole.
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
		t.Fatalf("a's window spliced the other window's rows wrong = %q", got)
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
	// the writer's reset wipes under this window: it reloads whole, the goal
	// this window held drops with its kv row, and the reset names itself
	g := &Goal{Condition: "the tests pass", SetAt: 0}
	a.agents.mu.Lock()
	a.agents.goal = g
	a.agents.mu.Unlock()
	a.storeGoal(g)
	if err := b.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	if got := windowText(a); len(got) != 0 {
		t.Fatalf("a's window after the writer's reset = %q, want empty", got)
	}
	if _, ok := a.Goal(); ok {
		t.Fatal("the goal survived the writer's reset")
	}
	if _, err := a.DB.GetKV("goal"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the goal's kv row survived the writer's reset: %v", err)
	}
	if got := ui.titles(); len(got) == 0 || got[len(got)-1] != "Chat reset by the writable window" {
		t.Fatalf("a's toasts after the writer's reset = %q", got)
	}
	sendTurns(t, a, ma, "fresh after the reset")
	b.SyncChat()
	got = windowText(b)
	if len(got) < 2 || !strings.Contains(got[0], "fresh after the reset") {
		t.Fatalf("b's window after the reset's first turn = %q", got)
	}
}

// TestSyncChatLeavesOwnStampingAlone: a window's own rows stamp through the
// pairing exactly as without a second window, spliced rows around them or not.
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
		t.Fatalf("the other window's turn did not splice at its place = %+v", got[2])
	}
	if got[3].Seq != 3 {
		t.Fatalf("a's second turn lost its seq = %+v", got[3])
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
	if got := ui.titles(); len(got) != 1 || got[0] != "The writable window sent a message" {
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

// TestSyncChatReloadsARegrownReset: the writer's reset whose wiped place the
// log already regrown past - a turn or machine-paced sends refilled it
// before the tick - is still caught, by content: the window reloads and
// toasts instead of drawing the wiped chat forever.
func TestSyncChatReloadsARegrownReset(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, ma := newWindow(t, state, proj)
	ui := &notifyUI{}
	a.SetUIHooks(ui)
	b, mb := newWindow(t, state, proj)
	sendTurns(t, a, ma, "one", "two")
	a.SyncChat()
	if n, err := a.DB.LogCount(); err != nil || n != 4 {
		t.Fatalf("a's log holds %d rows, want 4", n)
	}
	if err := b.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	sendTurns(t, b, mb, "r1", "r2", "r3") // regrows past a's cursor before any tick
	a.SyncChat()
	got := windowText(a)
	if len(got) != 6 || !strings.Contains(got[0], "r1") || !strings.Contains(got[4], "r3") {
		t.Fatalf("a's window after the regrown reset = %q, want b's fresh chat", got)
	}
	if got := ui.titles(); !slices.Contains(got, "Chat reset by the writable window") {
		t.Fatalf("a's toasts = %q, want the reset one", got)
	}
	// the window keeps working: a's next turn lands on the fresh chat and stamps
	sendTurns(t, a, ma, "after the reset")
	a.SyncChat()
	got = windowText(a)
	if len(got) != 8 || !strings.Contains(got[6], "after the reset") {
		t.Fatalf("a's window after its own next turn = %q", got)
	}
	for i, m := range a.Session().Transcript {
		if m.Seq == NoSeq {
			t.Fatalf("a's row %d drew unstamped after the reload: %+v", i, m)
		}
	}
}

// TestSyncChatSplicesHistoryUnderTheTurn: rows another window wrote before
// this window's turn started are this window's history: the splice lands them
// at their place while the turn owns the tail, or back-to-back turns hold
// them off the chat for as long as they run.
func TestSyncChatSplicesHistoryUnderTheTurn(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	if _, err := b.DB.AppendLog(store.KindUser, "from b", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DB.AppendLog(store.KindAgent, "b's reply", "", "", ""); err != nil {
		t.Fatal(err)
	}
	a.stampLogSeqs() // the pairing buffers the rows the turn will write above
	hold := make(chan struct{})
	var calls atomic.Int32
	a.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			if calls.Add(1) == 1 {
				// a tool call keeps the turn going: its result comes back for
				// a step that waits on hold
				return []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "nope", ToolCallInput: "{}"},
					{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
				}
			}
			<-hold
			return textParts("step two")
		}}, nil
	}
	if err := a.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if calls.Load() < 2 {
			return errors.New("the held step has not started")
		}
		return nil
	})
	a.SyncChat() // a direct sync mid-turn, the guard the idle tick relies on raced
	if got := windowText(a); !slices.Contains(got, "from b") {
		t.Fatalf("a held the buffered rows off its live turn = %q", got)
	}
	close(hold)
	waitIdle(t, a)
	a.SyncChat()
	// the spliced pair sits at its place, and the live order is the order a
	// restart reads
	if got := windowText(a); !slices.Contains(got, "from b") || got[len(got)-1] != "step two" {
		t.Fatalf("a's window = %q, want the splice under the turn's own rows", got)
	}
	c, _ := newWindow(t, state, proj)
	if got := windowText(c); !slices.Equal(got, windowText(a)) {
		t.Fatalf("a fresh window reads %q, the live window drew %q", got, windowText(a))
	}
}

// TestSyncChatReloadsUnderTheTurn: the writer's reset the log regrew past
// while a turn runs here is caught by the pairing itself - the window
// reloads mid-turn, the turn's next rows land on the fresh log, and the
// wiped chat is gone.
func TestSyncChatReloadsUnderTheTurn(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	ui := &notifyUI{}
	a.SetUIHooks(ui)
	b, mb := newWindow(t, state, proj)
	hold := make(chan struct{})
	var calls atomic.Int32
	a.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			if calls.Add(1) == 1 {
				return []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "nope", ToolCallInput: "{}"},
					{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
				}
			}
			<-hold
			return textParts("held step done")
		}}, nil
	}
	if err := a.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if calls.Load() < 2 {
			return errors.New("the held step has not started")
		}
		return nil
	})
	if err := b.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	sendTurns(t, b, mb, "r1", "r2") // the wiped place regrows before a's next persist
	close(hold)
	waitIdle(t, a)
	a.SyncChat()
	got := windowText(a)
	if !slices.Contains(got, "r1") || got[len(got)-1] != "held step done" {
		t.Fatalf("a's window under the turn = %q, want the fresh chat with the turn's row", got)
	}
	waitFor(t, func() error {
		if !slices.Contains(ui.titles(), "Chat reset by the writable window") {
			return errors.New("the reset toast has not landed")
		}
		return nil
	})
	for i, m := range a.Session().Transcript {
		if m.Seq == NoSeq {
			t.Fatalf("a's row %d drew unstamped after the mid-turn reload: %+v", i, m)
		}
	}
	c, _ := newWindow(t, state, proj)
	if got := windowText(c); !slices.Equal(got, windowText(a)) {
		t.Fatalf("a fresh window reads %q, the live window drew %q", got, windowText(a))
	}
}

// TestSyncChatSplicesFreshMeta: a reply the pairing buffered before
// its readout landed splices with the readout the log holds now, the way a
// restart reads it.
func TestSyncChatSplicesFreshMeta(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	seq, err := b.DB.AppendLog("agent", "hello from b", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.stampLogSeqs() // the pairing buffers the writer's row before the readout lands
	if err := b.DB.SetLogMeta(seq, `{"turn_ms":7000,"out":50}`); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	for _, m := range a.Session().Transcript {
		if m.Text != "hello from b" {
			continue
		}
		if m.Stat == nil || m.Stat.ElapsedMS != 7000 || m.Stat.Out != 50 {
			t.Fatalf("the spliced reply drew stat=%+v, want the readout", m.Stat)
		}
		return
	}
	t.Fatalf("the writer's reply never spliced: %q", windowText(a))
}

// TestSyncChatCarriesALateReadout: a reply's readout lands in the log after
// the row spliced in bare: the watch carries it onto the drawn row, the way
// the writer's own settle does for its window.
func TestSyncChatCarriesALateReadout(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	seq, err := b.DB.AppendLog("agent", "hello from b", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.stampLogSeqs() // the pairing buffers the writer's row before the readout lands
	a.SyncChat()     // the splice draws the row bare: the readout has not landed yet
	bare := false
	for _, m := range a.Session().Transcript {
		if m.Text == "hello from b" {
			bare = m.Stat == nil
		}
	}
	if !bare {
		t.Fatalf("the row spliced with its readout already: %q", windowText(a))
	}
	if err := b.DB.SetLogMeta(seq, `{"turn_ms":7000,"out":50}`); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	for _, m := range a.Session().Transcript {
		if m.Text != "hello from b" {
			continue
		}
		if m.Stat == nil || m.Stat.ElapsedMS != 7000 || m.Stat.Out != 50 {
			t.Fatalf("the spliced reply drew stat=%+v, want the late readout", m.Stat)
		}
		return
	}
	t.Fatalf("the writer's reply never spliced: %q", windowText(a))
}

// TestSyncChatCarriesALateReadoutOntoAFoldedReply: a split reply's row names
// its first chunk while its readout settles on its last - a window that
// spliced the folded row bare still finds the readout when it lands.
func TestSyncChatCarriesALateReadoutOntoAFoldedReply(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	full := strings.Repeat("z", 8100)
	if _, err := b.DB.AppendLog("agent", full[:8000], `{"split":true}`, "", ""); err != nil {
		t.Fatal(err)
	}
	last, err := b.DB.AppendLog("agent", full[8000:], `{"split":true}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.stampLogSeqs() // the pairing buffers the writer's rows before the readout lands
	a.SyncChat()     // the splice folds them into the one bare row
	bare := false
	for _, m := range a.Session().Transcript {
		if m.Text == full {
			bare = m.Stat == nil
		}
	}
	if !bare {
		t.Fatalf("the folded row spliced with its readout already: %q", windowText(a))
	}
	if err := b.DB.SetLogMeta(last, `{"turn_ms":7000,"out":50}`); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	for _, m := range a.Session().Transcript {
		if m.Text != full {
			continue
		}
		if m.Stat == nil || m.Stat.ElapsedMS != 7000 {
			t.Fatalf("the folded reply drew stat=%+v, want the late readout", m.Stat)
		}
		return
	}
	t.Fatalf("the writer's folded reply never spliced: %q", windowText(a))
}

// TestSyncChatSplicesAFailedCall: a call another window failed draws
// failed here too - the splice builds the round's rows together, so the tool
// row pairs with its echo instead of defaulting to a success.
func TestSyncChatSplicesAFailedCall(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	if _, err := b.DB.AppendLog("tool", `{"command":"false"}`, `{"id":"c1","tool":"shell"}`, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DB.AppendLog("echo", "some output", `{"tool":"shell","ok":false}`, "", ""); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	var rail ChatMessage
	for _, m := range a.Session().Transcript {
		if m.Kind == RowTool {
			rail = m
		}
	}
	if rail.Kind != RowTool || rail.Status != CallErr {
		t.Fatalf("the spliced call drew %+v, want a failed rail", rail)
	}
}

// TestSyncChatSplicesAMultiBatchBurstAfterTheTurn: a spliced burst bigger
// than one pairing batch, buffered while a turn holds, splices whole and in
// log order once the turn ends - the splice never lands a row after own rows
// it precedes, and a later plant joins after the burst, never over a skipped
// range.
func TestSyncChatSplicesAMultiBatchBurstAfterTheTurn(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	hold := make(chan struct{})
	var calls atomic.Int32
	a.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			if calls.Add(1) == 1 {
				return []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "nope", ToolCallInput: "{}"},
					{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
				}
			}
			<-hold
			return textParts("held step done")
		}}, nil
	}
	if err := a.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if calls.Load() < 2 {
			return errors.New("the held step has not started")
		}
		return nil
	})
	for i := range 3 * stampBatch {
		if _, err := b.DB.AppendLog(store.KindUser, fmt.Sprintf("burst row %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	close(hold)
	waitIdle(t, a)
	for range 2*stampBatch/128 + 4 { // the pairing claims one batch per tick
		a.SyncChat()
	}
	checkBurstOrder := func(when, tail string) {
		t.Helper()
		tr := a.Session().Transcript
		prev := int64(-1)
		for i, m := range tr {
			if m.Seq == NoSeq {
				continue
			}
			if m.Seq <= prev {
				t.Fatalf("%s: row %d draws seq %d after seq %d (%.30q after %.30q)",
					when, i, m.Seq, prev, m.Text, tr[i-1].Text)
			}
			prev = m.Seq
		}
		if last := tr[len(tr)-1]; last.Text != tail {
			t.Fatalf("%s: the tail is %.40q, want %q", when, last.Text, tail)
		}
	}
	checkBurstOrder("after the burst", "held step done")
	var seqs []int64
	for _, m := range a.Session().Transcript {
		if strings.HasPrefix(m.Text, "burst row ") {
			seqs = append(seqs, m.Seq)
		}
	}
	if len(seqs) != 3*stampBatch {
		t.Fatalf("%d burst rows drew, want %d", len(seqs), 3*stampBatch)
	}
	// a later plant splices after the burst, in its place
	if _, err := b.DB.AppendLog(store.KindUser, "recovery probe", "", "", ""); err != nil {
		t.Fatal(err)
	}
	a.SyncChat()
	checkBurstOrder("after the probe", "recovery probe")
	if got := windowText(a); !slices.Contains(got, "recovery probe") {
		t.Fatalf("the recovery probe never spliced: %q", got[len(got)-3:])
	}
}

// TestLogRowsToolWithoutEcho: a call whose echo never landed (a window
// killed mid-turn) draws failed, not a quiet success; one whose echo may
// still be beyond the read stays quiet.
func TestLogRowsToolWithoutEcho(t *testing.T) {
	tool := func(seq int64) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindTool, Text: `{"command":"ls"}`, MetaJSON: `{"tool":"shell"}`}
	}
	echo := func(seq int64, ok bool) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindEcho, Text: "out", MetaJSON: `{"tool":"shell","ok":` + strconv.FormatBool(ok) + `}`}
	}
	user := func(seq int64) store.LogEntry { return store.LogEntry{Seq: seq, Kind: store.KindUser, Text: "next"} }
	for _, c := range []struct {
		name string
		rows []store.LogEntry
		want CallStatus
	}{
		{"an echoed call reads its echo", []store.LogEntry{tool(0), echo(1, true), user(2)}, CallOK},
		{"a failed echo reads failed", []store.LogEntry{tool(0), echo(1, false), user(2)}, CallErr},
		{"a call killed before its echo reads failed", []store.LogEntry{tool(0), user(1)}, CallErr},
		{"a call the read cut before its echo stays quiet", []store.LogEntry{tool(0)}, CallOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := logRows(c.rows)
			if len(tr) == 0 || tr[0].Kind != RowTool {
				t.Fatalf("logRows drew %+v, want the tool row first", tr)
			}
			if tr[0].Status != c.want {
				t.Fatalf("the call drew %v, want %v", tr[0].Status, c.want)
			}
		})
	}
}

// TestSyncChatLoadsTheWritersGoal: a goal another window set joins a live
// window's state on the tick, so the chain moves here with the write instead
// of leaving the goal unowned; this window holds the row and runs none of it
// while another window writes the chat.
func TestSyncChatLoadsTheWritersGoal(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)
	if _, err := a.DB.AppendLog(store.KindUser, "the goal's message", "", "", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Goal{Condition: "check is clean", SetAt: 0, Writer: a.origin})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DB.SetKV(goalKV, string(raw)); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.Goal(); ok {
		t.Fatal("b holds the goal before any sync")
	}
	b.SyncChat()
	g, ok := b.Goal()
	if !ok || g.Condition != "check is clean" {
		t.Fatalf("b's goal after the sync = %+v, want the writer's one loaded", g)
	}
	if b.writerFlag() {
		t.Fatal("b runs the chain while another window writes the chat")
	}
}

// peerHeartbeat plants the heartbeat file the sync tests read for a live
// window: StartPeers's ticker would keep beating after a Close, so the tests
// name the files by hand.
func peerHeartbeat(t *testing.T, h *Host) {
	t.Helper()
	dir := filepath.Join(h.Dirs.StateHome, "peers", filepath.Base(filepath.Dir(h.DB.Path())))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.peersPath = filepath.Join(dir, h.origin+".json")
	if err := os.WriteFile(h.peersPath, []byte(`{"working":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
}
