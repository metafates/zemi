// chatflow_test.go pins the turn flip: every main-chat turn is a fresh call
// that opens on the view, the log dual-writes beside the old persist path,
// and zoom/date/note read and mark the eternal chat.

package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// sendTurns runs the turns a test names and returns every non-side prompt
// the stub saw.
func sendTurns(t *testing.T, h *Host, m *replyStubModel, texts ...string) [][]fantasy.Message {
	t.Helper()
	for _, text := range texts {
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.prompts)
}

// viewPart is a request's view message, the user message after the context.
func viewPart(t *testing.T, prompt []fantasy.Message) fantasy.Message {
	t.Helper()
	if len(prompt) < 3 {
		t.Fatalf("prompt has %d messages, want system, context, view, user", len(prompt))
	}
	return prompt[2]
}

// TestViewGrowsOnlyAtItsEnd: the view's parts come from the persisted
// view_lines, oldest first, and only grow at their end - the marked part's
// bytes the next view keeps, so the provider reads it from cache.
func TestViewGrowsOnlyAtItsEnd(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for i := range 10 {
		if _, err := h.DB.AppendLog("user", fmt.Sprintf("row %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.InsertNode(0, int64(i), fmt.Sprintf("line %d text", i)); err != nil {
			t.Fatal(err)
		}
	}
	one, ok := h.viewMessage()
	if !ok {
		t.Fatal("no view message for a ten-line view")
	}
	if len(one.Content) != 3 { // 10 lines: 4+4+2
		t.Fatalf("the first view has %d parts, want 3", len(one.Content))
	}
	if _, marked := one.Content[1].(fantasy.TextPart).ProviderOptions["anthropic"]; !marked {
		t.Fatal("no mark on the newest stable block")
	}
	// two more lines land (a turn's rows), unbuilt: placeholders
	for i := 10; i < 12; i++ {
		if _, err := h.DB.AppendLog("user", fmt.Sprintf("row %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	two, ok := h.viewMessage()
	if !ok {
		t.Fatal("no view message for a twelve-line view")
	}
	if len(two.Content) != 3 { // 12 lines: 4+4+4
		t.Fatalf("the second view has %d parts, want 3", len(two.Content))
	}
	// the parts before the tail are byte-identical, mark included
	for i := range 2 {
		a, b := one.Content[i].(fantasy.TextPart), two.Content[i].(fantasy.TextPart)
		if a.Text != b.Text {
			t.Fatalf("part %d moved between views:\n%q\n%q", i, a.Text, b.Text)
		}
	}
	if _, marked := two.Content[1].(fantasy.TextPart).ProviderOptions["anthropic"]; !marked {
		t.Fatal("the stable block lost its mark")
	}
	// the tail grew, unbuilt message lines as their own heads, the view closed
	if got := two.Content[2].(fantasy.TextPart).Text; !strings.HasSuffix(got, "10+1|user: row 10\n11+1|user: row 11\n</chat>") {
		t.Fatalf("the tail block = %q", got)
	}
	if !strings.HasPrefix(one.Content[0].(fantasy.TextPart).Text, "<chat>\n0+1|line 0 text\n") {
		t.Fatalf("the view does not open with the first line: %q", one.Content[0].(fantasy.TextPart).Text)
	}
	// the empty log skips the view entirely
	h2, _ := newTestHostDB(t)
	if _, ok := h2.viewMessage(); ok {
		t.Fatal("an empty log built a view message")
	}
}

// TestPendingLineNamesAndClips: a line whose node is unbuilt stands in with
// the message's kind (a tool's name too) and its own head, clipped to the
// line budget, so a huge row cannot bloat the view.
func TestPendingLineNamesAndClips(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for _, c := range []struct {
		name string
		kind store.LogKind
		meta string
		text string
		want string
	}{
		{"a reply", store.KindAgent, "", "hello there", "agent: hello there"},
		{"a call", store.KindTool, `{"tool":"shell"}`, `{"command":"ls"}`, `tool shell: {"command":"ls"}`},
		{
			"a long row", store.KindUser, "", strings.Repeat("x", 4*nodeBytes),
			"user: " + strings.Repeat("x", nodeBytes-6) + "...",
		},
	} {
		seq, err := h.DB.AppendLog(c.kind, c.text, c.meta, "", "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := h.chat.pendingLine(seq); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

// gateStub holds the turn's own model calls until the test releases them;
// side calls (titles, the builder) pass through.
type gateStub struct {
	*replyStubModel

	release chan struct{}
	arrived chan struct{} // closed when the turn's first call reached the model
	once    sync.Once
}

func (g *gateStub) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if !sideCall(call) {
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.replyStubModel.Stream(ctx, call)
}

// TestSteeringLogsAtInjection: a message sent mid-turn logs as a user row
// where the step boundary injected it, after the call it interrupted and
// before the step that read it.
func TestSteeringLogsAtInjection(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	base := replyStub(t, h, "mid\n\nfinal")
	base.toolStep.Store(1) // step 1 ends in a tool call
	g := &gateStub{replyStubModel: base, release: make(chan struct{}), arrived: make(chan struct{})}
	h.modelFn = func() (fantasy.LanguageModel, error) { return g, nil }
	h.SetUIHooks(&fakeUI{})
	if err := h.AgentSend("start work"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if !h.AgentActive() {
			return errors.New("the turn has not started")
		}
		select {
		case <-g.arrived:
			return nil
		default:
			return errors.New("the first step has not reached the model")
		}
	})
	if err := h.AgentSendUser("steered in"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	waitIdle(t, h)
	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	steeredAt := -1
	for i, r := range rows {
		kinds = append(kinds, string(r.Kind))
		if r.Kind == store.KindUser && r.Text == "steered in" {
			steeredAt = i
		}
	}
	if steeredAt <= 0 {
		t.Fatalf("the steered row is missing or first: %v", kinds)
	}
	// it landed after the echo of the call it interrupted, before the reply
	// that read it
	if rows[steeredAt-1].Kind != store.KindEcho {
		t.Fatalf("the steered row follows %q, want the interrupted call's echo", rows[steeredAt-1].Kind)
	}
	if !slices.Contains(kinds[steeredAt:], string(store.KindAgent)) {
		t.Fatalf("no reply after the steered row: %v", kinds)
	}
}

// TestNoteCommandAppends: /note writes one note row, mid-turn too, and the
// next turn's view carries its line.
func TestNoteCommandAppends(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	if err := h.Note("chapter one"); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogTail(1)
	if err != nil || len(rows) != 1 || rows[0].Kind != store.KindNote || rows[0].Text != "chapter one" {
		t.Fatalf("note rows = %+v, %v", rows, err)
	}
	waitFor(t, func() error { // a short note is its own node verbatim
		_, err := h.DB.NodeAt(0, rows[0].Seq)
		return err
	})
	prompts := sendTurns(t, h, m, "go on")
	if len(prompts) == 0 {
		t.Fatal("no turn ran")
	}
	if got := concatText(viewPart(t, prompts[len(prompts)-1]).Content); !strings.Contains(got, "chapter one") {
		t.Fatalf("the view does not carry the note: %q", got)
	}
}

// TestNoticesPersist: a recap, a goal verdict and every other status notice
// the chat draws carries a log row, so a resumed window reads the same rows
// back.
func TestNoticesPersist(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	if err := h.Note("chapter zero"); err != nil { // the log is not empty, as any chat a notice lands in
		t.Fatal(err)
	}
	h.notice("Recap: working on the import", "turn off in /config")
	h.notice("Goal met: the tests pass", "")
	rows, err := h.DB.LogTail(3)
	if err != nil || len(rows) != 3 ||
		rows[1].Kind != store.KindNotice || rows[1].Text != "Recap: working on the import" ||
		rows[2].Kind != store.KindNotice || rows[2].Text != "Goal met: the tests pass" {
		t.Fatalf("notice rows = %+v, %v", rows, err)
	}
	// a rebuilt window draws the same rows, detail included
	tr := logRows(rows[1:])
	if len(tr) != 2 || tr[0].Kind != RowNotice || tr[0].Text != "Recap: working on the import" ||
		tr[0].Detail != "turn off in /config" || tr[1].Text != "Goal met: the tests pass" {
		t.Fatalf("rebuilt rows = %+v", tr)
	}
	// the live rows carry their seq from the append
	h.tMu.RLock()
	live := h.Session().Transcript
	h.tMu.RUnlock()
	if n := len(live); n < 3 || live[n-2].Seq != rows[1].Seq || live[n-1].Seq != rows[2].Seq {
		t.Fatalf("live rows unstamped: %+v", live[max(n-2, 0):])
	}
}

// TestBuilderWakesOnRealTurns: a turn's rows reach the builder through the
// dual-write, and every one of them has a built line shortly after.
func TestBuilderWakesOnRealTurns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	line := strings.Repeat("u", 600) // over the leaf budget: a model call per row
	for range 4 {
		if _, err := h.chat.User(line, ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		n, err := h.DB.LogCount()
		if err != nil {
			return err
		}
		for seq := range n {
			if _, err := h.DB.NodeAt(0, seq); err != nil {
				return fmt.Errorf("leaf %d unbuilt: %w", seq, err)
			}
		}
		return nil
	})
	if m.calls.Load() == 0 {
		t.Fatal("no builder call: the wake did not reach the worker")
	}
}

// TestBuilderRebuildsAfterTheWritersReset: the writer's /reset shrank the
// log under this builder's cursor; its next wake must rebuild and build the
// fresh seqs instead of stalling until restart.
func TestBuilderRebuildsAfterTheWritersReset(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	line := strings.Repeat("u", 600)
	for range 3 {
		if _, err := h.chat.User(line, ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 2); err != nil {
			return fmt.Errorf("pre-reset leaf 2 unbuilt: %w", err)
		}
		return nil
	})
	// a reset wipes the log under the builder; the fresh rows are new seqs
	if err := h.DB.ResetLog(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := h.chat.User("after the writer's reset", ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 1); err != nil {
			return fmt.Errorf("post-reset leaf 1 unbuilt: %w", err)
		}
		return nil
	})
	if m.calls.Load() == 0 {
		t.Fatal("no builder call after the writer's reset")
	}
}

// TestPromptCarriesTheViewSection: the main conversation's prompt renders
// the view section and the builder's shares it, word for word; a subagent's
// prompt gains neither.
func TestPromptCarriesTheViewSection(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	sys, _, _ := h.buildSystemParts(h.mainTools())
	for _, want := range []string{"# The chat", "id+n|text", "until you have it whole", "what you learned"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the turn prompt misses %q", want)
		}
	}
	if !strings.Contains(chatSystem, viewPrompt) {
		t.Error("the builder's prompt does not share the view section")
	}
	pipe := h.PipelineFor()
	kept, _ := filterTools(h.allTools(pipe), []string{"shell", "read"}, subagentExcluded)
	sub := h.defaultSystemPrompt(kept)
	if strings.Contains(sub, "# The chat") || strings.Contains(sub, "zoom") {
		t.Error("the subagent prompt gained the view section")
	}
}

// zoomCall runs the zoom tool once, the way a model call lands.
func zoomCall(t *testing.T, h *Host, input string) fantasy.ToolResponse {
	t.Helper()
	res, err := h.zoomTool().Run(t.Context(), fantasy.ToolCall{ID: "z", Name: "zoom", Input: input})
	if err != nil {
		t.Fatalf("zoom(%s): %v", input, err)
	}
	return res
}

// dateCall runs the date tool once.
func dateCall(t *testing.T, h *Host, input string) fantasy.ToolResponse {
	t.Helper()
	res, err := h.dateTool().Run(t.Context(), fantasy.ToolCall{ID: "d", Name: "date", Input: input})
	if err != nil {
		t.Fatalf("date(%s): %v", input, err)
	}
	return res
}

// TestZoomAndDateRoundTrip: zoom replays template leaves, verbatim leaves
// and paged long echoes, opens children, and every bad input is a tool
// error, never a panic.
func TestZoomAndDateRoundTrip(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "ok") // the children's model merge reads it
	if err := h.chat.Echo("shell", true, strings.Repeat("e", 20_000), StreamTool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.chat.User("short one", ""); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{strings.Repeat("u", 450), strings.Repeat("v", 450)} {
		if _, err := h.chat.User(line, ""); err != nil {
			t.Fatal(err)
		}
	}
	// the leaves' nodes land in their appends or by builder call
	waitFor(t, func() error {
		for seq := range int64(4) {
			if _, err := h.DB.NodeAt(0, seq); err != nil {
				return err
			}
		}
		return nil
	})
	t.Run("template echo leaf", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":1}`)
		if !strings.HasPrefix(res.Content, "echo shell: ") {
			t.Fatalf("zoom = %.80q", res.Content)
		}
	})
	t.Run("long echo pages", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":1}`)
		if !strings.Contains(res.Content, "(page 1 of 3") {
			t.Fatalf("page 1 ends %q", res.Content[max(0, len(res.Content)-80):])
		}
		next := zoomCall(t, h, `{"id":0,"n":1,"page":2}`)
		if next.Content == "" || strings.Contains(next.Content, "(page 1 of") {
			t.Fatalf("page 2 = %.80q", next.Content)
		}
		if res := zoomCall(t, h, `{"id":0,"n":1,"page":9}`); !res.IsError {
			t.Fatal("a page past the end is not a tool error")
		}
	})
	t.Run("verbatim leaf and children", func(t *testing.T) {
		if res := zoomCall(t, h, `{"id":1,"n":1}`); res.Content != "user: short one" {
			t.Fatalf("zoom = %q", res.Content)
		}
		// the pair joins into 2+2 once the builder merged them
		waitFor(t, func() error {
			_, err := h.DB.NodeAt(1, 1)
			return err
		})
		res := zoomCall(t, h, `{"id":2,"n":2}`)
		if !strings.Contains(res.Content, "2+1|") || !strings.Contains(res.Content, "3+1|") {
			t.Fatalf("children = %q", res.Content)
		}
	})
	t.Run("date gives the row's time", func(t *testing.T) {
		row, err := h.DB.LogAt(1)
		if err != nil {
			t.Fatal(err)
		}
		if got := dateCall(t, h, `{"id":1}`).Content; got != row.CreatedAt {
			t.Fatalf("date = %q, want %q", got, row.CreatedAt)
		}
	})
	t.Run("n rounds up to a power of two", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":3}`)
		if res.IsError || !strings.Contains(res.Content, "0+2") {
			t.Fatalf("zoom(n 3) = %.60q, want the next power of two's merge", res.Content)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, c := range []struct {
			name, input string
		}{
			{"unknown id", `{"id":99}`},
			{"negative n", `{"id":0,"n":-2}`},
			{"line past the end", `{"id":3,"n":4}`},
			{"negative id", `{"id":-1}`},
		} {
			t.Run(c.name, func(t *testing.T) {
				if res := zoomCall(t, h, c.input); !res.IsError {
					t.Fatalf("zoom(%s) = %.60q, want a tool error", c.input, res.Content)
				}
			})
		}
		t.Run("date of an unknown id", func(t *testing.T) {
			if got := dateCall(t, h, `{"id":99}`).Content; !strings.Contains(got, "no message") {
				t.Fatalf("date = %q", got)
			}
		})
	})
}

// TestUserImagesZoomBack: a user message's images ride the log's parts_json
// and zoom replays them.
func TestUserImagesZoomBack(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "what is this"},
			fantasy.FilePart{Filename: "shot.png", MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		},
	}
	if err := h.persistMessage(msg); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogTail(1)
	if err != nil || len(rows) != 1 || rows[0].Kind != store.KindUser || rows[0].Text != "what is this" {
		t.Fatalf("user rows = %+v, %v", rows, err)
	}
	if rows[0].PartsJSON == "" {
		t.Fatal("the image did not ride the row")
	}
	res := zoomCall(t, h, `{"id":0,"n":1}`)
	if res.Type != "image" || res.MediaType != "image/png" {
		t.Fatalf("zoom replayed %s/%s, want the image", res.Type, res.MediaType)
	}
}

// TestLogHoldsTheThought: the turn's messages land in the log as they happen:
// the user's words, the reply.
func TestLogHoldsTheTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "kept")
	h.SetUIHooks(&fakeUI{})
	sendTurns(t, h, m, "hello")
	logRows, err := h.DB.LogTail(2)
	if err != nil || len(logRows) != 2 || logRows[0].Kind != store.KindUser || logRows[0].Text != "hello" || logRows[1].Kind != store.KindAgent || logRows[1].Text != "kept" {
		t.Fatalf("log rows = %+v, %v", logRows, err)
	}
}

// TestLogHoldsTheThought: a reasoning stretch lands in the log as the thought
// row it drew, ahead of the reply, with the wall time its flush measured -
// a stretch no call follows included, whose flush never happens.
func TestLogHoldsTheThought(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	var calls atomic.Int64
	h.modelFn = func() (fantasy.LanguageModel, error) {
		m := &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			if calls.Add(1) == 1 {
				return append(reasoningParts("pondering the ask."), textParts("kept")...)
			}
			return textParts("done")
		}}
		return slowThought{LanguageModel: m, gap: 60 * time.Millisecond}, nil
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	rows, err := h.DB.LogTail(3)
	if err != nil || len(rows) != 3 || rows[0].Kind != store.KindUser || rows[0].Text != "go" ||
		rows[1].Kind != store.KindThought || rows[1].Text != "pondering the ask." ||
		rows[2].Kind != store.KindAgent || rows[2].Text != "kept" {
		t.Fatalf("log rows = %+v, %v", rows, err)
	}
	var meta struct {
		MS int64 `json:"ms"`
	}
	if err := json.Unmarshal([]byte(rows[1].MetaJSON), &meta); err != nil || meta.MS < 50 {
		t.Fatalf("thought row meta = %q (%v), want the stretch's wall time", rows[1].MetaJSON, err)
	}
	tr := logRows(rows)
	if len(tr) != 3 || tr[1].Kind != RowReasoning || tr[1].Text != "pondering the ask." ||
		tr[1].Stat == nil || tr[1].Stat.ThoughtMS < 50 || tr[1].Seq != rows[1].Seq {
		t.Fatalf("rebuilt rows = %+v", tr)
	}
}

// TestReplyStatCarriesTheThought: the reply row's stat carries the turn's
// whole thought, so the live digest says what the rebuilt window reads from
// the log's reply row.
func TestReplyStatCarriesTheThought(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		m := &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			return append(reasoningParts("weighing it."), textParts("kept")...)
		}}
		return slowThought{LanguageModel: m, gap: 60 * time.Millisecond}, nil
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	h.tMu.Lock()
	tr := h.session.Transcript
	h.tMu.Unlock()
	var reply *ChatMessage
	for i := range tr {
		if tr[i].Kind == "" && tr[i].Role == fantasy.MessageRoleAssistant {
			reply = &tr[i]
		}
	}
	if reply == nil || reply.Stat == nil || reply.Stat.ThoughtMS < 50 {
		t.Fatalf("last reply row = %+v, want the turn's thought in its stat", reply)
	}
}

// slowThought streams a model's parts with a wall gap before each one after
// the first, so a thought accrues time the way a streaming provider's does.
type slowThought struct {
	fantasy.LanguageModel

	gap time.Duration
}

func (m slowThought) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	resp, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	first := true
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range resp {
			if !first {
				time.Sleep(m.gap)
			}
			first = false
			if !yield(part) {
				return
			}
		}
	}, nil
}

// TestResumedWindowKeepsTheTurn: the turn's readout and thought ride the
// log's reply row, so the window a restart rebuilds from the log keeps both,
// as the live one did.
func TestResumedWindowKeepsTheTurn(t *testing.T) {
	clock := &fakeClock{now: testEpoch}
	h := newTestHostUI(t, clock)
	h.SetUIHooks(&fakeUI{})
	// the turn as it ran: the words, a thought, a call, the reply; the log
	// rows land the way persistMessage writes them
	if _, err := h.chat.User("go", ""); err != nil {
		t.Fatal(err)
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
	hk := h.streamHooks()
	hk.ReasoningDelta("thinking it over.")
	clock.set(testEpoch.Add(3 * time.Second))
	if err := h.chat.Thought("thinking it over.", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := h.chat.Tool("read", `{"path":"a.go"}`, ""); err != nil {
		t.Fatal(err)
	}
	hk.ToolCallPending("t1", "read")
	if err := h.chat.Echo("read", true, "data", StreamTool{}); err != nil {
		t.Fatal(err)
	}
	hk.ToolCallEnd("t1", "read", out("data"), "")
	hk.StepDone(agent.Step{}) // the step's finish logged the call and its result
	hk.TextDelta("The answer.")
	if err := h.chat.Agent("The answer."); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	h.agents.turnStart, h.agents.turnModel = testEpoch, "acme-gateway/big"
	h.agents.mu.Unlock()
	(&streamAdapter{h: h}).TurnDone(nil, fantasy.Usage{OutputTokens: 895})
	// the restart: the window rebuilds from the log's tail
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	tr := h.Session().Transcript
	reply := tr[len(tr)-1]
	if reply.Stat == nil || reply.Stat.Model != "acme-gateway/big" ||
		reply.Stat.Out != 895 || reply.Stat.ElapsedMS != 3000 || reply.Stat.ThoughtMS != 3000 || reply.Stat.Ended == 0 {
		t.Fatalf("the rebuilt reply lost the turn's stat: %+v", reply.Stat)
	}
	thought := tr[len(tr)-3]
	if thought.Kind != RowReasoning || thought.Text != "thinking it over." ||
		thought.Stat == nil || thought.Stat.ThoughtMS != 3000 || thought.Seq == 0 {
		t.Fatalf("the rebuilt thought row is not the one the turn drew: %+v", thought)
	}
}

// TestResumedWindowNamesADeadTail: a log whose last turn holds no reply and
// no error row is a window that closed on it — the boot rebuild ends with
// the death marker, the way an interrupt's "■ Interrupted" does; a tail
// whose turn answered draws none.
func TestResumedWindowNamesADeadTail(t *testing.T) {
	for _, c := range []struct {
		name   string
		seed   func(h *Host) error
		marker bool
	}{
		{"a bare prompt", func(h *Host) error {
			_, err := h.chat.User("go", "")
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
			return err
		}, true},
		{"a settled call with no reply", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			if err := h.chat.Tool("read", `{"path":"a.go"}`, ""); err != nil {
				return err
			}
			return h.chat.Echo("read", true, "data", StreamTool{})
		}, true},
		{"a turn that answered", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			return h.chat.Agent("The answer.")
		}, false},
		{"an interrupted turn", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			return h.chat.Error("Interrupted", true)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t, &fakeClock{now: testEpoch})
			h.SetUIHooks(&fakeUI{})
			if err := c.seed(h); err != nil {
				t.Fatal(err)
			}
			rows, err := h.DB.LogTail(chatWindowRows)
			if err != nil {
				t.Fatal(err)
			}
			h.SetTranscript(logRows(rows))
			h.markDeadTail()
			tr := h.Session().Transcript
			last := tr[len(tr)-1]
			if c.marker != (last.Kind == RowError && last.Text == "the window closed mid-turn") {
				t.Fatalf("the tail ends with %+v, marker wanted %v", last, c.marker)
			}
		})
	}
}

// TestResumedZoomAndDateRowsKeepTheirHeads: a zoom row's summary is the line
// it named, in the id+n shape the chat reads, and its result keeps the opened
// child for the row to preview; a date row's summary is its id and its result
// the compact local stamp, not the provider's RFC3339Nano one.
func TestResumedZoomAndDateRowsKeepTheirHeads(t *testing.T) {
	h := newTestHostUI(t, &fakeClock{now: testEpoch})
	h.SetUIHooks(&fakeUI{})
	seed := func(tool, input, output string, ok bool) {
		if err := h.chat.Tool(tool, input, ""); err != nil {
			t.Fatal(err)
		}
		if err := h.chat.Echo(tool, ok, output, StreamTool{}); err != nil {
			t.Fatal(err)
		}
	}
	seed("zoom", `{"id":16032,"n":8}`, "16032+4|Rate-limit fixed: real quotas (big 50000)\n16034+2|second child line", true)
	seed("date", `{"id":16033}`, "2026-10-29T11:03:12.487654321Z", true)
	seed("zoom", `{"id":9,"n":8}`, "no line 9+8 in a chat of 10 messages", false)
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	tr := h.Session().Transcript
	if len(tr) != 3 {
		t.Fatalf("the rebuilt window holds %d rows, want the three calls: %+v", len(tr), tr)
	}
	if tr[0].Text != "16032+8" || tr[1].Text != "16033" || tr[2].Text != "9+8" {
		t.Fatalf("the rows' summaries are %q, %q, %q, want the id+n shapes", tr[0].Text, tr[1].Text, tr[2].Text)
	}
	if want := "16032+4|Rate-limit fixed: real quotas (big 50000)\n16034+2|second child line"; tr[0].Output != want {
		t.Fatalf("the zoom row keeps %q, want the opened child", tr[0].Output)
	}
	at, err := time.Parse(time.RFC3339Nano, "2026-10-29T11:03:12.487654321Z")
	if err != nil {
		t.Fatal(err)
	}
	if want := at.Local().Format(previewStamp); tr[1].Output != want {
		t.Fatalf("the date row reads %q, want the compact local %q", tr[1].Output, want)
	}
	if tr[2].Status != CallErr || tr[2].Output != "no line 9+8 in a chat of 10 messages" {
		t.Fatalf("the failed zoom row = %+v", tr[2])
	}
}

// TestResumedWindowKeepsTheFailuresAndCommands: a failed turn's error row and
// a shell command row land in the log, so the window a restart rebuilds from
// the log keeps them as the live one did.
func TestResumedWindowKeepsTheFailuresAndCommands(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{})
	// a turn that fails: the error row is the only record of it
	if _, err := h.chat.User("go", ""); err != nil {
		t.Fatal(err)
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
	(&streamAdapter{h: h}).TurnDone(errors.New("the provider failed"), fantasy.Usage{})
	// the user's own command, which logs beside the echo the model reads
	h.RunShell("echo hi")
	waitFor(t, func() error {
		rows, err := h.DB.LogKindBack(store.KindShell, 1)
		if err != nil || len(rows) == 0 {
			return fmt.Errorf("the shell row has not landed: %w", err)
		}
		return nil
	})
	live := h.Session().Transcript
	var liveErr, liveShell *ChatMessage
	for i := range live {
		if live[i].Kind == RowError {
			liveErr = &live[i]
		}
		if live[i].Kind == RowShell {
			liveShell = &live[i]
		}
	}
	if liveErr == nil || liveShell == nil {
		t.Fatalf("the live window holds no error and shell rows: %+v", live)
	}
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	tr := h.Session().Transcript
	var gotErr, gotShell *ChatMessage
	for i := range tr {
		if tr[i].Kind == RowError {
			gotErr = &tr[i]
		}
		if tr[i].Kind == RowShell {
			gotShell = &tr[i]
		}
	}
	if gotErr == nil || gotErr.Text != liveErr.Text || gotErr.Status != liveErr.Status {
		t.Fatalf("the rebuilt error row is not the one the turn drew: %+v want %+v", gotErr, liveErr)
	}
	if gotShell == nil || gotShell.Text != liveShell.Text || gotShell.Detail != liveShell.Detail || gotShell.Status != liveShell.Status {
		t.Fatalf("the rebuilt shell row is not the one the command drew: %+v want %+v", gotShell, liveShell)
	}
}

// TestToolMarkBudgetOnTheFlow: an in-turn request carries exactly the four
// marks Anthropic allows - the system block, the view's own marked part and
// the last two messages. The deleted last-tool mark would make it five,
// which the provider hard-errors on.
func TestToolMarkBudgetOnTheFlow(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "done")
	m.toolStep.Store(1) // the first step ends in a tool call, so a second runs
	h.SetUIHooks(&fakeUI{})
	for range 5 { // five lines: two view parts, so the newest stable block is marked
		if _, err := h.chat.User(strings.Repeat("w", 30), ""); err != nil {
			t.Fatal(err)
		}
	}
	prompts := sendTurns(t, h, m, "do it")
	if len(prompts) < 2 {
		t.Fatalf("%d requests recorded, want the tool step's follow-up", len(prompts))
	}
	marked := 0
	for _, msg := range prompts[1] {
		if _, ok := msg.ProviderOptions["anthropic"]; ok {
			marked++
		}
		for _, p := range msg.Content {
			if _, ok := p.Options()["anthropic"]; ok {
				marked++
			}
		}
	}
	if marked != 4 {
		t.Fatalf("%d marks on the in-turn request, want exactly 4", marked)
	}
}

// TestChatlogConcurrencyUnderTurns: appends and view reads race safely.
func TestChatlogConcurrencyUnderTurns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				if _, err := h.chat.User("x", ""); err != nil {
					t.Error(err)
					return
				}
				if _, err := h.DB.LoadView(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if n, _ := h.DB.LogCount(); n != 80 {
		t.Fatalf("log holds %d rows, want 80", n)
	}
	if lines, _ := h.DB.LoadView(); len(lines) != 80 {
		t.Fatalf("view holds %d lines, want 80", len(lines))
	}
}

// TestTurnStatSkipsPromptWait: time the user spent at a prompt (ask, plan,
// question) is not billed to the model in the reply's stat.
func TestTurnStatSkipsPromptWait(t *testing.T) {
	clock := &fakeClock{now: testEpoch}
	h := newTestHostUI(t, clock)
	h.SetUIHooks(&fakeUI{})
	if _, err := h.chat.User("go", ""); err != nil {
		t.Fatal(err)
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
	h.agents.mu.Lock()
	h.agents.turnStart = testEpoch
	h.agents.promptWait = 6 * time.Second
	h.agents.mu.Unlock()
	h.streamHooks().TextDelta("The answer.")
	clock.set(testEpoch.Add(7500 * time.Millisecond))
	(&streamAdapter{h: h}).TurnDone(nil, fantasy.Usage{OutputTokens: 100})
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	tr := h.Session().Transcript
	stat := tr[len(tr)-1].Stat
	if stat == nil || stat.ElapsedMS != 1500 {
		t.Fatalf("the reply's stat billed the prompt wait: %+v", stat)
	}
}
