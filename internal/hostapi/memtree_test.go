package hostapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

// errBuildFails is what the always-failing builder stub answers.
var errBuildFails = errors.New("compaction failed")

// treeModel is the builder's stub: every compaction is answered with the next
// scripted reply and the calls' conversations are kept for assertions. A gate
// holds every call from gateAfter on until its channel closes, and fail
// fails them all. rate fails them all with a 429, the builder's cooldown
// trigger; refuseCap fails that many calls from the first with a max_tokens
// refusal, the cap-drop heal's trigger. finish, when set, replaces the reply's
// finish reason. compact, when set, answers every call with a function of its
// own prompt instead of the script - the real fake's compaction shape. think
// answers with the script as reasoning and no text, the reasoning-only reply,
// until the conversation holds an assistant turn - the carryover's own shape.
// wedge parks a gated call deaf to its context: nothing aborts it, and a
// release past a cancellation answers with it - the abort that lands late.
type treeModel struct {
	mu        sync.Mutex
	replies   []string
	convo     [][]fantasy.Message
	times     []time.Time
	calls     int
	gate      chan struct{}
	gateAfter int
	fail      atomic.Bool
	rate      atomic.Bool
	refuseCap atomic.Int32
	burn      atomic.Bool
	think     atomic.Bool
	wedge     atomic.Bool
	finish    fantasy.FinishReason
	compact   func([]fantasy.Message) string
}

func (m *treeModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	i, gate := m.calls, m.gate
	m.calls++
	m.times = append(m.times, time.Now())
	m.convo = append(m.convo, slices.Clone(call.Prompt))
	m.mu.Unlock()
	if gate != nil && i >= m.gateAfter {
		if m.wedge.Load() {
			<-gate
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if m.rate.Load() {
		return nil, &fantasy.RetryError{Errors: []error{&fantasy.ProviderError{
			StatusCode: 429, Message: "Rate limit exceeded",
		}}}
	}
	if m.refuseCap.Add(-1) >= 0 {
		return nil, &fantasy.ProviderError{StatusCode: 400, Message: "max_tokens is above the model maximum"}
	}
	if m.fail.Load() {
		return nil, errBuildFails
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	reply := m.replies[len(m.replies)-1]
	if i < len(m.replies) {
		reply = m.replies[i]
	}
	if m.compact != nil {
		reply = m.compact(m.convo[i])
	}
	if m.burn.Load() {
		reply = ""
	}
	if m.think.Load() && !slices.ContainsFunc(m.convo[i], func(msg fantasy.Message) bool {
		return msg.Role == fantasy.MessageRoleAssistant
	}) {
		return slices.Values([]fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
			{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: reply},
			{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
		}), nil
	}
	parts := textParts(reply)
	if m.finish != "" {
		parts[len(parts)-1].FinishReason = m.finish
	}
	return slices.Values(parts), nil
}

func (m *treeModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *treeModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *treeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *treeModel) Provider() string { return "stub" }
func (m *treeModel) Model() string    { return "stub-1" }

func (m *treeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// newTree boots a chat log whose builder runs on a stub model: the host's
// own, started at NewHost, on modelFn's stub.
func newTree(t *testing.T, replies ...string) (*Host, *chatLog, *treeModel) {
	t.Helper()
	h, _ := newTestHostDB(t)
	m := &treeModel{replies: replies}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	h.ReleaseBuilder()
	return h, h.chat, m
}

// coverage asserts the view covers [0, n) exactly once: contiguous, ordered,
// no gaps and no overlaps.
func coverage(t *testing.T, db *store.DB, n int64) {
	t.Helper()
	lines, err := db.LoadView()
	if err != nil {
		t.Fatal(err)
	}
	at := int64(0)
	for _, ln := range lines {
		if first := ln.I << ln.L; first != at {
			t.Fatalf("view covers %d at %d+%d, want %d", first, ln.L, ln.I, at)
		}
		at += int64(1) << ln.L
	}
	if at != n {
		t.Fatalf("view ends at %d, want %d", at, n)
	}
}

// TestRulerKeepsShortestLine: five tries and every one over the limit keeps
// the shortest line.
func TestRulerKeepsShortestLine(t *testing.T) {
	t.Parallel()
	shortest := strings.Repeat("e", 520)
	h, cl, m := newTree(t,
		strings.Repeat("a", 600), strings.Repeat("b", 580), strings.Repeat("c", 560),
		strings.Repeat("d", 540), shortest)
	if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 0); err != nil {
			return err
		}
		return nil
	})
	if n, _ := h.DB.NodeAt(0, 0); n.Text != shortest {
		t.Fatalf("kept a %d-byte line, want the shortest", len(n.Text))
	}
	if calls := m.callCount(); calls != _buildTryMax {
		t.Fatalf("%d calls, want %d", calls, _buildTryMax)
	}
}

// TestViewConcurrentCoverage: two mutators merging from their own stale reads
// of the view can never double-cover a message.
func TestViewConcurrentCoverage(t *testing.T) {
	t.Parallel()
	db, err := store.Open(filepath.Join(t.TempDir(), "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const total = 60
	for range 40 {
		if _, err := db.AppendLog("user", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// every parent answers built: the race is on the view alone
	always := func(nodeID) (int, bool) { return 0, true }
	plan := func() []store.ViewMerge {
		lines, err := db.LoadView()
		if err != nil {
			t.Error(err)
			return nil
		}
		view := make([]viewLine, len(lines))
		for i, ln := range lines {
			view[i] = viewLine{nodeID{ln.L, ln.I}, 1}
		}
		_, merges := mergeDown(view, total, always, func([]viewLine) bool { return false })
		return viewMerges(merges)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		for seq := int64(40); seq < total; seq++ {
			if _, err := db.AppendLog("user", "", "", "", ""); err != nil {
				t.Error(err)
				return
			}
			if _, err := db.MergeViewLines(plan()); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := db.MergeViewLines(plan()); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
	coverage(t, db, total)
}

// TestBuilderCloseQuiet: Close returns once the builder's goroutines exited,
// a call still hanging on the model included, and a wake afterwards neither
// blocks nor panics.
func TestBuilderCloseQuiet(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := &treeModel{gate: make(chan struct{})} // calls hang until released
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	b := newMemTree(context.Background(), h)
	b.release()
	if _, err := h.chat.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if m.callCount() == 0 {
			return errors.New("no call started")
		}
		return nil
	})
	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: the builder is not quiet")
	}
	b.Wake()
	b.RebuildQueues()
}

// TestSplitText: a long text becomes sibling rows of about the cap, cut at a
// newline when one is near, and a short text is one row.
func TestSplitText(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		want int
	}{
		{"short stays whole", strings.Repeat("a", 100), 1},
		{"long splits", strings.Repeat("a", 9000), 2},
		{"a newline near the cut takes the split", strings.Repeat("a", 7900) + "\n" + strings.Repeat("b", 500), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := splitText(c.text, _chatSplitChars)
			if len(rows) != c.want {
				t.Fatalf("%d rows, want %d", len(rows), c.want)
			}
			for _, row := range rows {
				if n := len([]rune(row)); n > _chatSplitChars {
					t.Fatalf("row of %d runes, cap %d", n, _chatSplitChars)
				}
			}
		})
	}
	t.Run("echo clips head and tail", func(t *testing.T) {
		out := clipMiddle(strings.Repeat("a", 10000)+"MIDDLE"+strings.Repeat("b", 10000), 100)
		if !strings.HasPrefix(out, "aaa") || !strings.HasSuffix(out, "bbb") || !strings.Contains(out, "\n...\n") {
			t.Fatalf("clip %q", out)
		}
	})
}

// TestWorkContinuations: a report longer than one row splits into siblings,
// each labeled, so a continuation reads as the same run's where the split cut.
func TestWorkContinuations(t *testing.T) {
	t.Parallel()
	h, cl, _ := newTree(t, "x")
	body := strings.Repeat("paragraph line\n", 1200)
	if _, err := cl.Work("run1", "Scout", "explore", "finished", "Scout  "+body); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogTail(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 || rows[0].Kind != store.KindWork || rows[1].Kind != store.KindWork {
		t.Fatalf("want split work rows, got %d rows", len(rows))
	}
	label := "Scout  "
	for i, r := range rows {
		if !strings.HasPrefix(r.Text, label) {
			t.Fatalf("row %d unlabeled: %q", i, r.Text[:min(20, len(r.Text))])
		}
		if !strings.Contains(r.MetaJSON, `"run":"run1"`) {
			t.Fatalf("row %d meta %q", i, r.MetaJSON)
		}
	}
}

// TestResetRebuilds: the builder survives a wipe and builds the fresh rows
// dense, never going backwards.
func TestResetRebuilds(t *testing.T) {
	t.Parallel()
	h, cl, m := newTree(t, strings.Repeat("s", 400))
	for range 6 {
		if _, err := cl.User(strings.Repeat("w", 600), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(2, 0); err != nil {
			return err
		}
		return nil
	})
	calls := m.callCount()
	if err := h.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if n, err := h.DB.LogCount(); err != nil || n != 1 {
			return errors.New("the wipe has not settled")
		}
		return nil
	})
	// new appends land dense and build over the wipe
	for range 2 {
		if _, err := cl.User(strings.Repeat("w", 600), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(1, 0); err != nil {
			return err
		}
		return nil
	})
	coverage(t, h.DB, 3) // the reset's note, then the two fresh rows
	if m.callCount() < calls {
		t.Fatal("the builder went backwards")
	}
}

// TestResetRefusesAWedgedBuilder: a build wedged past the abort - a
// provider deaf to its own cancellation - refuses the reset instead of
// wedging it. The armed key's press returns without waiting for the wipe:
// the fence is up and the log whole right after it, and the refusal lands
// on the wipe's own goroutine behind the bound. The build the abort did
// settle - the one held behind the wedge's burst - is carried in the retry
// list, so the kept log's line does not stay an unbuilt placeholder
// behind the wedge.
func TestResetRefusesAWedgedBuilder(t *testing.T) {
	h, cl, m := newTree(t, strings.Repeat("s", 400))
	m.gate = make(chan struct{})
	m.gateAfter = 0     // every call parks on the gate
	m.wedge.Store(true) // and never hears the abort's cancellation
	defer close(m.gate)
	if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.User(strings.Repeat("y", 600), ""); err != nil {
		t.Fatal(err)
	}
	// the first build parks inside the provider call; the second, held
	// behind its burst, is the one the abort settles
	waitFor(t, func() error {
		cl.tree.callMu.Lock()
		flight := cl.tree.flight
		cl.tree.callMu.Unlock()
		if m.callCount() < 1 {
			return errors.New("the wedged build has not started")
		}
		if flight < 2 {
			return errors.New("the second build has not taken its slot")
		}
		return nil
	})
	cl.tree.quiesceWait = 2 * time.Second
	// the armed key's press, through the command the overlay carries: it
	// returns without waiting, so the fence is up and the log still whole -
	// a press that waited for the wipe would return behind a refusal that
	// already opened the gate
	reset := slices.IndexFunc(h.chatCommands(), func(c *Command) bool { return c.Name == "reset" })
	if err := h.chatCommands()[reset].GoFn(h, "", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		cl.tree.callMu.Lock()
		held := cl.tree.held
		cl.tree.callMu.Unlock()
		if !held {
			return errors.New("the fence never went up after the press")
		}
		return nil
	})
	if n, _ := h.DB.LogCount(); n != 2 {
		t.Fatalf("%d log rows under the fence, want the two it held", n)
	}
	// the aborted hearing build is carried, not lost: /health's builder row
	// counts it as a failed build waiting to retry
	waitFor(t, func() error {
		if cl.tree.health.failed.Load() != 1 {
			return errors.New("the aborted build reached no retry list")
		}
		return nil
	})
	// the bound refuses the reset on the wipe's own goroutine: the gate
	// opens, the log is kept whole, and the toast names the refusal
	waitFor(t, func() error {
		cl.tree.callMu.Lock()
		held := cl.tree.held
		cl.tree.callMu.Unlock()
		if held {
			return errors.New("the bound has not refused the reset")
		}
		h.errs.mu.RLock()
		refused := slices.ContainsFunc(h.errs.entries, func(e errEntry) bool {
			return e.kind == "command" && strings.Contains(e.msg, "did not settle")
		})
		h.errs.mu.RUnlock()
		if !refused {
			return errors.New("the refusal reached no toast")
		}
		if n, _ := h.DB.LogCount(); n != 2 {
			return errors.New("the refused reset touched the log")
		}
		return nil
	})
}

// TestBuilderEmptyReplyLadder: a capped compaction reply is the reply budget,
// not the line, so the build doubles the cap and retries; a model that burns
// every cap fails its node, which waits for fresh rows - never a timer - and
// builds on the next message.
func TestBuilderEmptyReplyLadder(t *testing.T) {
	t.Parallel()
	h, cl, m := newTree(t, strings.Repeat("s", 100))
	m.finish = fantasy.FinishReasonLength
	m.burn.Store(true) // every reply burns: a reasoning model over its cap
	for range 2 {
		if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if calls := m.callCount(); calls != 6 {
			return fmt.Errorf("%d calls, want the cap ladder per node", calls)
		}
		if row := h.builderRow(); !strings.Contains(row.Detail, "2 builds failed") {
			return fmt.Errorf("builder row = %q, want both builds failed", row.Detail)
		}
		return nil
	})
	// the failure says why, so the model's config is the next thing to look at
	for _, msg := range h.errorsSince(time.Time{}, "chat builder") {
		if !strings.Contains(msg, "empty compaction reply at the output cap") {
			t.Fatalf("failed error %q, want the output cap named", msg)
		}
	}

	// a failed build rides no timer: the idle window passes callless
	quiet := m.callCount()
	time.Sleep(1500 * time.Millisecond)
	if calls := m.callCount(); calls != quiet {
		t.Fatalf("%d more calls while failed, want none", calls-quiet)
	}

	// fresh rows retry the failed nodes, and the reply builds
	m.burn.Store(false)
	if _, err := cl.User(strings.Repeat("w", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		for _, id := range [][2]int64{{0, 0}, {0, 1}, {0, 2}, {1, 0}} {
			if _, err := h.DB.NodeAt(id[0], id[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if calls := m.callCount(); calls != quiet+3 {
		t.Fatalf("%d calls, want the two retries and the new leaf", calls)
	}
	coverage(t, h.DB, 3)
}

// TestBuilderDropsTheCapTheServerRefuses: a build whose model refuses the cap
// zemi asked for drops it once, memoized, and builds uncapped - the heal a
// turn gets - instead of failing every compaction.
func TestBuilderDropsTheCapTheServerRefuses(t *testing.T) {
	t.Parallel()
	h, cl, m := newTree(t, "line")
	if err := h.Providers.Register(agent.Spec{ID: "c", Kind: agent.KindOpenAICompat, BaseURL: "http://x", Models: []agent.Model{{ID: "leaf"}}}, "", false); err != nil {
		t.Fatal(err)
	}
	h.Settings.SetQuiet("models.compact", "c/leaf") // not models.default: side calls must not resolve a model
	t.Cleanup(func() { _capRejected.Delete("c/leaf") })
	if got := h.maxOutput("c/leaf"); got != 65_536 {
		t.Fatalf("max output %d, want the compat kind default", got)
	}
	m.refuseCap.Store(1) // the first build call refuses the cap; its retry runs uncapped
	for range 2 {
		if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		_, err := h.DB.NodeAt(1, 0) // the pair's join built, so the leaves did too
		return err
	})
	if calls := m.callCount(); calls != 3 {
		t.Fatalf("%d calls, want the refused build, its uncapped retry and the second leaf", calls)
	}
	if got := h.maxOutput("c/leaf"); got != 0 {
		t.Fatalf("max output %d after the heal, want the refusal memoized", got)
	}
	coverage(t, h.DB, 2)
}

// TestBuilderEmptyReplyCarriesThinking: a reasoning model that stops with no
// line has usually written it inside the thinking, so the corrected retry
// hands that thinking back as the assistant turn and asks for the line -
// which lands in one more call.
func TestBuilderEmptyReplyCarriesThinking(t *testing.T) {
	t.Parallel()
	h, cl, m := newTree(t, "thought it through", "the line")
	m.think.Store(true)
	if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		n, err := h.DB.NodeAt(0, 0)
		if err != nil {
			return err
		}
		if n.Text != "the line" {
			return fmt.Errorf("the node drew %q, want the reply after the carryover", n.Text)
		}
		return nil
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls != 2 {
		t.Fatalf("%d calls, want the empty reply and the carryover", m.calls)
	}
	// the prompt rides the system message, so: system, prompt, thinking, note
	if len(m.convo[1]) != 4 {
		t.Fatalf("the retry sent %d messages, want system, prompt, thinking, note", len(m.convo[1]))
	}
	if got := concatText(m.convo[1][2].Content); got != "thought it through" {
		t.Fatalf("the retry's assistant turn reads %q, want the thinking", got)
	}
	if note := concatText(m.convo[1][3].Content); !strings.Contains(note, "carried no line") {
		t.Fatalf("the retry's note reads %q, want the empty-line note", note)
	}
}

// TestBuilderEmptyRetryAsksDifferently: an empty reply with nothing to hand
// back still must not re-request identical bytes - the note names the miss
// on the prompt - and a second empty reply parks the node with the error.
func TestBuilderEmptyRetryAsksDifferently(t *testing.T) {
	t.Parallel()
	h, cl, m := newTree(t, strings.Repeat("s", 100))
	m.burn.Store(true) // every reply empty, no reasoning: the gateway hiccup
	if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if m.callCount() < 2 {
			return errors.New("the corrected retry has not run")
		}
		return nil
	})
	m.mu.Lock()
	first, again := m.convo[0][1].Content, m.convo[1][1].Content
	m.mu.Unlock()
	if concatText(first) == concatText(again) {
		t.Fatal("the retry re-sent identical bytes")
	}
	if note := concatText(again); !strings.Contains(note, "carried no line") {
		t.Fatalf("the retry's prompt reads %q, want the miss named", note)
	}
	waitFor(t, func() error {
		for _, msg := range h.errorsSince(time.Time{}, "chat builder") {
			if strings.Contains(msg, "empty compaction reply") {
				if !strings.Contains(msg, "finish stop, out 0+0 tok, 0 reasoning bytes") {
					return fmt.Errorf("error %q, want the finish and the token spend named", msg)
				}
				return nil
			}
		}
		return errors.New("no empty-reply error recorded")
	})
	// the parked node requeues on the cooldown too, not only on fresh rows
	waitFor(t, func() error {
		if row := h.builderRow(); !strings.Contains(row.Detail, "cooling down") {
			return fmt.Errorf("builder row = %q, want the cooldown armed", row.Detail)
		}
		return nil
	})
	// the empty replies' exact parts land beside the store, one row per call
	dumps, err := os.ReadFile(filepath.Join(filepath.Dir(h.DB.Path()), "compaction-dumps.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(dumps, []byte("\n")); n != 2 {
		t.Fatalf("%d dump rows, want one per empty reply", n)
	}
	if !bytes.Contains(dumps, []byte(`"kind":"text"`)) {
		t.Fatalf("dump rows %s, want the reply's parts named", dumps)
	}
}

// TestBuilderCoolDownOnRateLimit: a rate-limited build arms a cooldown that
// retries it without fresh rows, doubling per consecutive rate-limited
// round; a success clears it, and the rounds leave no error entry - the
// cooldown is a wait, not a failure to act on.
func TestBuilderCoolDownOnRateLimit(t *testing.T) {
	h, cl, m := newTree(t, strings.Repeat("s", 400))
	cl.tree.coolBase, cl.tree.maxRetries = 200*time.Millisecond, 0 // calls are rounds
	m.rate.Store(true)
	if _, err := cl.User(strings.Repeat("z", 600), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if m.callCount() == 0 {
			return errors.New("no build ran")
		}
		return nil
	})
	waitFor(t, func() error {
		if m.callCount() < 4 {
			return fmt.Errorf("%d calls, want the cooldown retrying without fresh rows", m.callCount())
		}
		return nil
	})
	// each consecutive rate-limited round doubles the wait: the gaps floor
	// at the base, its powers in turn
	m.mu.Lock()
	times := slices.Clone(m.times)
	m.mu.Unlock()
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < cl.tree.coolBase<<(i-1) {
			t.Fatalf("call %d followed the last by %v, want the doubled cooldown", i+1, gap)
		}
	}
	waitFor(t, func() error {
		if row := h.builderRow(); !strings.Contains(row.Detail, "cooling down") {
			return fmt.Errorf("builder row = %q, want the cooldown named", row.Detail)
		} else if strings.Contains(row.Detail, "/errors") {
			return fmt.Errorf("builder row = %q, want no errors pointer while cooling", row.Detail)
		}
		return nil
	})
	if msgs := h.errorsSince(time.Time{}, "chat builder"); len(msgs) != 0 {
		t.Fatalf("%d error rows, want a rate limit to leave none", len(msgs))
	}
	m.rate.Store(false)
	waitFor(t, func() error {
		_, err := h.DB.NodeAt(0, 0)
		return err
	})
	if row := h.builderRow(); row.Level != HealthOK {
		t.Fatalf("builder row = %q (%v), want the recovery", row.Detail, row.Level)
	}
}

// TestOverflowRetrySendsAMergedView: a context-too-large refusal merges the
// view synchronously, so the retry sends the merged view instead of the
// bytes that were refused.
func TestOverflowRetrySendsAMergedView(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "done")
	m.overflow.Store(1) // the turn's first request is refused, the retry is not
	h.SetUIHooks(&fakeUI{})
	// two hundred long messages: verbatim leaves, but every pair is over the
	// join's budget, so their merges are model calls and shrink the view.
	// Under the ceiling, so only the overflow guard's batch merges them.
	for range 200 {
		if _, err := h.chat.User(strings.Repeat("w", 400), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitForWithin(t, time.Minute, func() error {
		_, err := h.DB.NodeAt(1, 99) // every level-1 pair built
		return err
	})
	prompts := sendTurns(t, h, m, "do it")
	if len(prompts) < 2 {
		t.Fatalf("%d requests recorded, want the refused one and the retry", len(prompts))
	}
	before, after := viewBytes(t, prompts[0]), viewBytes(t, prompts[1])
	if after >= before {
		t.Fatalf("the retry's view is %d bytes, not smaller than the refused %d", after, before)
	}
	// the retry still carries the task: the merge shrank the view, not the
	// work the turn was asked for - the refused request's message count
	// survives, the view message swapped in place
	if len(prompts[1]) != len(prompts[0]) {
		t.Fatalf("the retry carries %d messages, the refused request %d - the task dropped", len(prompts[1]), len(prompts[0]))
	}
	tail := prompts[1][len(prompts[1])-1]
	tp, ok := fantasy.AsMessagePart[fantasy.TextPart](tail.Content[0])
	if tail.Role != fantasy.MessageRoleUser || !ok || !strings.Contains(tp.Text, "do it") {
		t.Fatalf("the retry ends on %s %q, want the task it was refused for", tail.Role, tp.Text)
	}
}

// TestOverflowDropsTheCapWhenTheMergeDoesNotFit: a context refusal that
// survives the merged retry with a cap zemi set is the cap's fault: the cap
// drops, memoized, and the turn goes on uncapped instead of failing.
func TestOverflowDropsTheCapWhenTheMergeDoesNotFit(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	if err := h.Providers.Register(agent.Spec{ID: "c", Kind: agent.KindOpenAICompat, BaseURL: "http://x", Models: []agent.Model{{ID: "turn"}}}, "", false); err != nil {
		t.Fatal(err)
	}
	h.Settings.SetQuiet("models.default", "c/turn")
	t.Cleanup(func() { _capRejected.Delete("c/turn") })
	if got := h.maxOutput("c/turn"); got != 65_536 {
		t.Fatalf("max output %d, want the compat kind default", got)
	}
	m := replyStub(t, h, "done")
	m.overflow.Store(2) // the refused send and the merged retry; the uncapped one passes
	h.SetUIHooks(&fakeUI{})
	prompts := sendTurns(t, h, m, "do it")
	if len(prompts) != 3 {
		t.Fatalf("%d requests, want the refused one, the merged retry and the uncapped one", len(prompts))
	}
	if got := h.maxOutput("c/turn"); got != 0 {
		t.Fatalf("max output %d after the heal, want the refusal memoized", got)
	}
	for _, row := range h.Session().Transcript {
		if row.Kind == "error" {
			t.Fatalf("error row shown: %+v", row)
		}
	}
}

// viewBytes sizes a request's view message: the bytes its parts carry.
func viewBytes(t *testing.T, prompt []fantasy.Message) int {
	t.Helper()
	msg := viewPart(t, prompt)
	n := 0
	for _, p := range msg.Content {
		tp, ok := p.(fantasy.TextPart)
		if !ok {
			t.Fatal("the view message carries a non-text part")
		}
		n += len(tp.Text)
	}
	return n
}
