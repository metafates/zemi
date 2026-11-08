package ui

// What the chat and the rule draw, where seeding it needs the host's
// internals: the rows that preview what a call opened, a reply's readout, the
// runs a rail and a viewed window show, and the pending row's count. Each
// test's state-side half stays pinned in internal/hostapi.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// renderAt draws the shipped widget id over a screen state a test built, as
// the frame goroutine draws it.
func renderAt(t testing.TB, app *App, id string, st *hostapi.ScreenState) [][]hostapi.Segment {
	t.Helper()
	w := app.widgetByID(id)
	if w == nil {
		t.Fatalf("the shipped %s is not registered", id)
	}
	lines, _, err := app.renderWidget(shownWidget{ID: id, Region: w.region, Wrap: w.wrap, Go: w, width: st.Width, height: st.Height}, st)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// frameText is rows as their text, one per line.
func frameText(rows [][]hostapi.Segment) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(hostapi.SegsText(row))
		b.WriteString("\n")
	}
	return b.String()
}

// TestZoomAndDateRowsPreviewTheirLine: a zoom row previews the text of the
// line it opened, alone: its child's address stays behind the click, as the
// second child does. A failed zoom's error draws once folded, from the digest
// the run folds into, and again as its own row open.
func TestZoomAndDateRowsPreviewTheirLine(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 40)
	seed := func(tool, input, output string, status hostapi.CallStatus) {
		host.AppendChat(hostapi.ChatMessage{
			Role: "assistant", Kind: hostapi.RowTool, Tool: tools.ToolName(tool), Text: input, Output: output, Status: status,
		})
	}
	seed("zoom", "16032+8", "16032+4|Rate-limit fixed: real quotas (big 50000)\n16034+2|second child line", hostapi.CallOK)
	seed("date", "16033", "2026-10-29 11:03", hostapi.CallOK)
	seed("zoom", "9+8", "no line 9+8 in a chat of 10 messages", hostapi.CallErr)
	app.renderSync(t)
	app.Frame()
	// the run opens the way a click opens its digest, and the fold outlives
	// the frames the test draws under its own state
	x, y := findCell(t, app, "zoom")
	clickAt(app, x, y)
	open := frameText(renderAt(t, app, "chat", host.ScreenState(78, 40, app.uiState())))
	if !strings.Contains(open, "zoom 16032+8") || !strings.Contains(open, "date 16033") {
		t.Fatalf("the rows' heads are not their addresses:\n%s", open)
	}
	if !strings.Contains(open, "Rate-limit fixed: real quotas (big 50000)") {
		t.Fatalf("the zoom row does not preview the line it opened:\n%s", open)
	}
	if strings.Contains(open, "16032+4|") || strings.Contains(open, "second child line") {
		t.Fatalf("the zoom preview is not the opened line's text alone:\n%s", open)
	}
	if n := strings.Count(open, "no line 9+8 in a chat of 10 messages"); n != 2 {
		t.Fatalf("the failed zoom's error drew %d times open, want the digest and its row:\n%s", n, open)
	}
	folded := func() string {
		// the digest's own click folds the run back: the fold lives in the
		// widget, not the state
		x, y := findCell(t, app, "zoom failed")
		clickAt(app, x, y)
		return frameText(renderAt(t, app, "chat", host.ScreenState(78, 40, app.uiState())))
	}()
	if n := strings.Count(folded, "no line 9+8 in a chat of 10 messages"); n != 1 {
		t.Fatalf("the failed zoom's error drew %d times folded, want its digest's once:\n%s", n, folded)
	}
}

// TestReplyStatsShed: a settled reply's readout leads with the model that
// wrote it and sheds it first, so a narrow chat keeps what the turn cost.
func TestReplyStatsShed(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "go")
	appendReply(host, "done", hostapi.TurnStat{Out: 100, ElapsedMS: 1500, Model: "anthropic/claude-sonnet-4-5"})
	wide := strings.Join(chatRows(t, app, 80), "\n")
	if !strings.Contains(wide, "anthropic/claude-sonnet-4-5 · 1.5s · 100 out") {
		t.Fatalf("the wide readout does not lead with the model:\n%s", wide)
	}
	narrow := strings.Join(chatRows(t, app, 40), "\n")
	if strings.Contains(narrow, "anthropic") || !strings.Contains(narrow, "1.5s · 100 out") {
		t.Fatalf("the narrow readout kept the model or lost the turn:\n%s", narrow)
	}
}

// TestReplyTLDR: a reply that ends with a <tldr> tag draws its body without
// the tag and the summary as a quoted line under it.
func TestReplyTLDR(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "go")
	appendReply(host, "moved the parse into shared.go and fixed the callers.\n<tldr>parse moved to shared.go</tldr>", hostapi.TurnStat{})
	rows := chatRows(t, app, 80)
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "<tldr>") {
		t.Fatalf("the tag or the body leaked:\n%s", joined)
	}
	if !slices.Contains(rows, "  │ parse moved to shared.go") {
		t.Fatalf("the summary block is missing:\n%s", joined)
	}
}

// TestThoughtDigestHoldsWhileReplying: the thought a replying turn holds draws
// as the digest row it settles as, so the settle that lands later rewrites no
// row. A blank thought names nothing, and one a call holds is the digest's,
// not a row of its own.
func TestThoughtDigestHoldsWhileReplying(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "go")
	draw := func(s hostapi.StreamSnapshot) []string {
		host.SeedStream(s)
		return chatRows(t, app, 78)
	}
	digest := func(rows []string) bool {
		return slices.ContainsFunc(rows, func(r string) bool { return strings.HasPrefix(r, "▸ thought") })
	}
	// a thought of nothing says nothing
	if blank := draw(hostapi.StreamSnapshot{Active: true, Text: "The reply", Reasoning: "   ", Thought: 3 * time.Second}); digest(blank) {
		t.Fatalf("a blank thought drew a digest row:\n%s", strings.Join(blank, "\n"))
	}
	const want = "▸ thought 3.0s"
	live := draw(hostapi.StreamSnapshot{Active: true, Text: "The reply", Reasoning: "pondering the ask.", Thought: 3 * time.Second})
	if !slices.Contains(live, want) {
		t.Fatalf("the thought's digest row is not drawn while the reply streams:\n%s", strings.Join(live, "\n"))
	}
	// the settle moves the thought into its own row: the same rows, unmoved
	appendReasoning(host, "pondering the ask.", hostapi.TurnStat{ThoughtMS: 3000})
	if settled := draw(hostapi.StreamSnapshot{Active: true, Text: "The reply"}); !slices.Equal(live, settled) {
		t.Fatalf("the settle rewrote the rows:\nlive:\n%s\nsettled:\n%s", strings.Join(live, "\n"), strings.Join(settled, "\n"))
	}
	// a thought a call holds folds into the run's digest with the call
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowTool, Tool: "read", Text: "a.go", Status: hostapi.CallOK})
	if held := draw(hostapi.StreamSnapshot{Active: true, Text: "The reply", Reasoning: "third thought.", Thought: 5 * time.Second}); digest(held) {
		t.Fatalf("a thought a call holds drew a digest row of its own:\n%s", strings.Join(held, "\n"))
	}
}

// TestGrownRowRedrawsWhileTheCacheHolds: a settled row whose text grew in
// place (a joined reasoning chunk) draws its new text, though the rows around
// it did not change: the edit counter carries it past the chat's cache.
func TestGrownRowRedrawsWhileTheCacheHolds(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "look around")
	appendReply(host, "short.", hostapi.TurnStat{})
	st := func() *hostapi.ScreenState { return host.ScreenState(78, 24, app.uiState()) }
	if got := frameText(renderAt(t, app, "chat", st())); !strings.Contains(got, "short.") {
		t.Fatalf("the reply's row is not drawn:\n%s", got)
	}
	grown := st()
	grown.Transcript = slices.Clone(grown.Transcript)
	grown.Transcript[1].Text = "short answer."
	grown.TranscriptEdit++
	// twice: the first render fills the cache, the second reads it back
	for _, pass := range []string{"fill", "hit"} {
		if got := frameText(renderAt(t, app, "chat", grown)); !strings.Contains(got, "short answer.") {
			t.Fatalf("the %s drew the stale row:\n%s", pass, got)
		}
	}
}

// runSession stores a run's conversation under a child session, the way a run
// that ended and left this process writes it, so the chat can show it again.
func runSession(t testing.TB, db *store.DB, title, status, text string) string {
	t.Helper()
	id := store.ShortID()
	if err := db.CreateSession(id, store.ShortID(), title, "explore"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSessionRunStatus(id, status); err != nil {
		t.Fatal(err)
	}
	parts, err := json.Marshal(fantasy.Message{
		Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(&store.Message{SessionID: id, Role: "assistant", PartsJSON: string(parts)}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestViewedRunDrawsNoSessionPendingRow: a run's own rows say what it is
// doing, so the chat draws none of the session's pending row while the run is
// shown: it would stand at the bottom of a run working somewhere else.
func TestViewedRunDrawsNoSessionPendingRow(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	id := runSession(t, testStore(t, host), "explore the repo", "running", "it reads a.go")
	defer host.Busy("Answering the side question", func() {})()
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	got := frameText(renderAt(t, app, "chat", host.ScreenState(78, 24, app.uiState())))
	if !strings.Contains(got, "it reads a.go") {
		t.Fatalf("the run's view does not draw its rows:\n%s", got)
	}
	if strings.Contains(got, "answering the side question") || strings.Contains(got, "thinking") {
		t.Fatalf("the run's view draws the session's pending row:\n%s", got)
	}
}

// TestViewedRunThinksLive: a run the chat shows streams its thinking the way
// the session's turn does: the tail under a live thinking row, no settled
// thought row before the call that ends it lands.
func TestViewedRunThinksLive(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	id := runSession(t, testStore(t, host), "explore the repo", "running", "it reads a.go")
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	st := host.ScreenState(78, 24, app.uiState())
	if st.View == nil {
		t.Fatal("the chat does not show the run")
	}
	// a run's live turn, which only a run this process holds streams: the chat
	// reads it off the state all the same
	st.Agent = hostapi.AgentState{Active: true, Thinking: true, Reasoning: "pondering the cause"}
	got := frameText(renderAt(t, app, "chat", st))
	if !strings.Contains(got, "thinking") || !strings.Contains(got, "pondering the cause") {
		t.Fatalf("the run's live thinking tail is not drawn:\n%s", got)
	}
	if strings.Contains(got, "✻ thought") {
		t.Fatalf("the run's thought settled before the call that ends it:\n%s", got)
	}
}

// TestWriteRowCountsStreamedBytes: a write call still streaming its input
// shows the size read so far, and drops it once the call runs.
func TestWriteRowCountsStreamedBytes(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "write", Status: "wait", Streamed: 1234},
	}})
	got := frameText(renderAt(t, app, "chat", host.ScreenState(78, 24, app.uiState())))
	if !strings.Contains(got, "1.2 kB") {
		t.Fatalf("the streaming write row does not show the size read:\n%s", got)
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "write", Status: "run", Input: "cmd/serve.go", Streamed: 1234, Started: time.Now().Add(-2 * time.Second)},
	}})
	got = frameText(renderAt(t, app, "chat", host.ScreenState(78, 24, app.uiState())))
	if strings.Contains(got, "1.2 kB") {
		t.Fatalf("a started write keeps the streamed size:\n%s", got)
	}
}

// rowsAfterPending is the chat's report of the rows it drew below its pending
// row, which the rule reads.
func rowsAfterPending() int {
	_ruleState.mu.Lock()
	defer _ruleState.mu.Unlock()
	return _ruleState.after
}

// TestPendingRowCarriesOnlyOnceItLeavesTheView: the chat draws its pending row
// above the todo list and reports how many rows sit below it; the rule never
// carries a busy label - the chat's row is where that work lives.
func TestPendingRowCarriesOnlyOnceItLeavesTheView(t *testing.T) {
	// the count is the chat render's report on the package, not the frame's:
	// no t.Parallel, or another render answers for this one
	app, host := bootAgentApp(t, 80, 40)
	host.AppendMessage("user", "hi")
	defer host.Busy("Answering the side question", func() {})()
	const label = "answering the side question"
	rule := func(below int) string {
		st := host.ScreenState(78, 1, app.uiState())
		st.RowsBelow = below
		return frameText(renderAt(t, app, "working", st))
	}
	app.renderSync(t)
	if after := rowsAfterPending(); after != 0 {
		t.Fatalf("without todos the pending row sits %d rows above the tail, want 0", after)
	}
	if got := rule(0); strings.Contains(got, label) {
		t.Fatalf("the rule carries the pending row still in view: %q", got)
	}
	if got := rule(1); strings.Contains(got, label) {
		t.Fatalf("the rule carries a busy label: %q", got)
	}
	seedTodos(t, host, []store.Todo{{Content: "a thing", Status: store.TodoPending}})
	app.renderSync(t)
	after := rowsAfterPending()
	if after == 0 {
		t.Fatal("the todo block below the pending row is not counted")
	}
	if got := rule(after + 1); strings.Contains(got, label) {
		t.Fatalf("the rule carried the pending row over %d rows still in view: %q", after+1, got)
	}
}

// TestRelPath: a row writes a path under the working directory relative to
// it and one elsewhere under home from ~.
func TestRelPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, cwd, home, want string }{
		{"/h/me/p/a.go", "~/p", "/h/me", "a.go"},
		{"cat /h/me/.x/b /h/me/p/c", "~/p", "/h/me", "cat ~/.x/b c"},
		{"/h/meta/a", "~/p", "/h/me", "/h/meta/a"},
		{"/tmp/p/a.go", "/tmp/p", "/h/me", "a.go"},
	} {
		if got := relPath(c.in, c.cwd, c.home); got != c.want {
			t.Errorf("relPath(%q, %q, %q) = %q, want %q", c.in, c.cwd, c.home, got, c.want)
		}
	}
}
