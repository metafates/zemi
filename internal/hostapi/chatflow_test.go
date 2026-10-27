// chatflow_test.go pins the turn flip: every main-chat turn is a fresh call
// that opens on the view, the log dual-writes beside the old persist path,
// and zoom/date/note read and mark the eternal chat.

package hostapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// sendTurns runs the turns a test names and returns every non-side prompt
// the stub saw.
func sendTurns(t *testing.T, h *Host, m *reflectStubModel, texts ...string) [][]fantasy.Message {
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
		if err := h.DB.AppendViewLine(0, int64(i)); err != nil {
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
		if err := h.DB.AppendViewLine(0, int64(i)); err != nil {
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
	// the tail grew, unbuilt lines as placeholders, the view closed
	if got := two.Content[2].(fantasy.TextPart).Text; !strings.HasSuffix(got, "10+1|(not summarized yet: zoom it)\n11+1|(not summarized yet: zoom it)\n</chat>") {
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

// gateStub holds the turn's own model calls until the test releases them;
// side calls (titles, the builder) pass through.
type gateStub struct {
	*reflectStubModel
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
	return g.reflectStubModel.Stream(ctx, call)
}

// TestSteeringLogsAtInjection: a message sent mid-turn logs as a user row
// where the step boundary injected it, after the call it interrupted and
// before the step that read it.
func TestSteeringLogsAtInjection(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	base := reflectStub(t, h, "mid\n\nfinal")
	base.toolStep.Store(1) // step 1 ends in a tool call
	g := &gateStub{reflectStubModel: base, release: make(chan struct{}), arrived: make(chan struct{})}
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
		kinds = append(kinds, r.Kind)
		if r.Kind == kindUser && r.Text == "steered in" {
			steeredAt = i
		}
	}
	if steeredAt <= 0 {
		t.Fatalf("the steered row is missing or first: %v", kinds)
	}
	// it landed after the echo of the call it interrupted, before the reply
	// that read it
	if rows[steeredAt-1].Kind != kindEcho {
		t.Fatalf("the steered row follows %q, want the interrupted call's echo", rows[steeredAt-1].Kind)
	}
	if !slices.Contains(kinds[steeredAt:], kindAgent) {
		t.Fatalf("no reply after the steered row: %v", kinds)
	}
}

// TestNoteCommandAppends: /note writes one note row, mid-turn too, and the
// next turn's view carries its line.
func TestNoteCommandAppends(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	if err := h.Note("chapter one"); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogTail(1)
	if err != nil || len(rows) != 1 || rows[0].Kind != kindNote || rows[0].Text != "chapter one" {
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

// TestBuilderWakesOnRealTurns: a turn's rows reach the builder through the
// dual-write, and every one of them has a built line shortly after.
func TestBuilderWakesOnRealTurns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
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
	reflectStub(t, h, "ok") // the children's model merge reads it
	if err := h.chat.Echo("shell", true, strings.Repeat("e", 20_000)); err != nil {
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
	t.Run("errors", func(t *testing.T) {
		for _, c := range []struct {
			name, input string
		}{
			{"unknown id", `{"id":99}`},
			{"n not a power of two", `{"id":0,"n":3}`},
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
	reflectStub(t, h, "ok")
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
	if err != nil || len(rows) != 1 || rows[0].Kind != kindUser || rows[0].Text != "what is this" {
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

// TestLogHoldsTheTurn: the turn's messages land in the log as they happen:
// the user's words, the reply.
func TestLogHoldsTheTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "kept")
	h.SetUIHooks(&fakeUI{})
	sendTurns(t, h, m, "hello")
	logRows, err := h.DB.LogTail(2)
	if err != nil || len(logRows) != 2 || logRows[0].Kind != kindUser || logRows[0].Text != "hello" || logRows[1].Kind != kindAgent || logRows[1].Text != "kept" {
		t.Fatalf("log rows = %+v, %v", logRows, err)
	}
}

// TestToolMarkBudgetOnTheFlow: an in-turn request carries exactly the four
// marks Anthropic allows - the system block, the view's own marked part and
// the last two messages. The deleted last-tool mark would make it five,
// which the provider hard-errors on.
func TestToolMarkBudgetOnTheFlow(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "done")
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
