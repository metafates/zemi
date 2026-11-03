package hostapi

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// TestCallView: a row's view takes its facts from the call and draws what the
// call wrote in the form it is written in, apart from what it acted on.
func TestCallView(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	ui := h.hooks().(*fakeUI)
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "shell", Text: "go test ./...", Label: "run the tests", Status: CallErr, Output: "FAIL\nmain_test.go"})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "edit", Text: "main.go", Status: CallOK, Diff: "-old\n+new"})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "main.go", Status: CallOK, Output: "1\tpackage main"})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowReasoning, Text: "the tests **fail**", Stat: &TurnStat{ThoughtMS: 4200}})
	h.AppendChat(ChatMessage{
		Role: "assistant", Kind: RowTool, Tool: "question", Text: "Where?", Status: CallOK,
		Output: "Where?\tin main\tbecause\nWhat?\tnow\t\n",
	})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "shell", Text: "make slow", Status: CallBG, Job: "job-3", Output: "still running"})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "agent", Run: "r9", Text: "look around", Label: "look around", Status: CallOK, Output: "the report"})

	for _, c := range []struct {
		row    int
		title  string
		fields map[string]string
		parts  []BodyPart
	}{
		{
			1, "Command",
			map[string]string{"status": "failed"},
			[]BodyPart{{Text: "go test ./...", Lang: "bash", Fill: FillCanvas}, {Text: "FAIL\nmain_test.go", Fill: FillWell}},
		},
		{
			2, "Edit",
			map[string]string{"status": "ok", "file": "main.go"},
			[]BodyPart{{Text: "-old\n+new", Lang: "diff"}},
		},
		{
			3, "Read",
			map[string]string{"status": "ok", "file": "main.go"},
			[]BodyPart{{Text: "1\tpackage main", Fill: FillWell}},
		},
		{
			4, "Thought",
			map[string]string{"status": "", "time": "4.2s"},
			[]BodyPart{{Text: "the tests **fail**", Lang: "markdown"}},
		},
		{
			5, "Question",
			map[string]string{"status": "ok"},
			[]BodyPart{{Text: "Where?  in main\nnote: because\nWhat?  now", Fill: FillWell}},
		},
	} {
		row, took, ok := h.chatRow(c.row)
		if !ok {
			t.Fatalf("row %d: the chat shows no such row", c.row)
		}
		o := h.callView(c.row, row, took)
		if o.ID != "call:"+strconv.Itoa(c.row) || o.Title != c.title || !slices.Equal(o.Parts, c.parts) {
			t.Errorf("row %d: %q %q parts %+v, want %q and %+v", c.row, o.ID, o.Title, o.Parts, c.title, c.parts)
		}
		for name, want := range c.fields {
			if got := fieldOf(o, name); got != want {
				t.Errorf("row %d: field %s = %q, want %q (fields %+v)", c.row, name, got, want, o.Fields)
			}
		}
	}
	// the view steps along the turn's rail, a question is a call view too,
	// and a run row is a step target like any other, landing on its own view
	for row, want := range map[int][2]string{
		1: {"", "call 2"}, 2: {"call 1", "call 3"}, 4: {"call 3", "call 5"},
		5: {"call 4", "call 6"}, 6: {"call 5", "agents show r9"}, 7: {"call 6", ""},
	} {
		o := h.callView(row, mustRow(t, h, row), 0)
		if o.Prev != want[0] || o.Next != want[1] {
			t.Errorf("row %d: prev %q next %q, want %q and %q", row, o.Prev, o.Next, want[0], want[1])
		}
	}

	// a call that went on as a background job offers to open it
	job := h.callView(6, mustRow(t, h, 6), 0)
	if fieldOf(job, "job") != "job-3" || !slices.ContainsFunc(job.Keys, func(k OverlayKey) bool { return k.Run == "jobs view job-3" }) {
		t.Errorf("the job's row: fields %+v keys %+v", job.Fields, job.Keys)
	}
	// a live call is its own view, refreshed as it runs
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{{Name: "shell", Input: "sleep 9", Status: CallRun}}})
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	live := ui.opened[len(ui.opened)-1]
	if live.ID != "call:-1" || !live.Live || fieldOf(live, "status") != "running" {
		t.Fatalf("the live call's view = %+v", live)
	}
	// two live calls: the view steps along the run still being written, and
	// back into the rows the turn settled before it
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{Name: "shell", Input: "sleep 9", Status: CallRun},
		{Name: "read", Input: "a.go", Status: CallRun},
	}})
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	if first := ui.opened[len(ui.opened)-1]; first.Prev != "agents show r9" || first.Next != "call -2" {
		t.Fatalf("the first live call steps to %q and %q", first.Prev, first.Next)
	}
	if err := h.openCall(-2); err != nil {
		t.Fatal(err)
	}
	if last := ui.opened[len(ui.opened)-1]; last.Prev != "call -1" || last.Next != "" {
		t.Fatalf("the second live call steps to %q and %q", last.Prev, last.Next)
	}
	if err := h.openCall(99); err == nil || !strings.Contains(err.Error(), "no row 99") {
		t.Fatalf("row 99: %v, want a refusal", err)
	}
	if err := h.openCall(0); err == nil || !strings.Contains(err.Error(), "no row 0") {
		t.Fatalf("row 0: %v, want a refusal", err)
	}
}

// TestCallInput: the input a call row copies - the command, or the thing the
// call acted on; a tool whose input is no value copies nothing.
func TestCallInput(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tool, text, want string
	}{
		{"shell", "make check", "make check"},
		{"lua", "return 1", "return 1"},
		{"read", "main.go", "main.go"},
		{"grep", "callInput", "callInput"},
		{"skill", "mine", "mine"},
		{"question", "Where?", ""},
		{"shell", "", ""},
	} {
		if got := callInput(ChatMessage{Tool: tools.ToolName(c.tool), Text: c.text}); got != c.want {
			t.Errorf("callInput(%s, %q) = %q, want %q", c.tool, c.text, got, c.want)
		}
	}
}

// TestCallStepCrossesIntoTheLiveTurn: the turn settles its calls into the
// transcript's tail while its later ones still stream, so one rail runs
// through both and a step crosses the boundary both ways.
func TestCallStepCrossesIntoTheLiveTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.AppendMessage("user", "go")
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{{Name: "shell", Input: "sleep 9", Status: CallRun}}})
	// the live calls are the turn's first: no settled row to step to yet
	if prev, next := h.callStep(-1); prev != "" || next != "" {
		t.Fatalf("the lone live call steps to %q and %q", prev, next)
	}
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "a.go", Status: CallOK})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "b.go", Status: CallOK})
	if o := h.callView(3, mustRow(t, h, 3), 0); o.Prev != "call 2" || o.Next != "call -1" {
		t.Fatalf("the last settled call steps to %q and %q", o.Prev, o.Next)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	if o := ui.opened[len(ui.opened)-1]; o.Prev != "call 3" || o.Next != "" {
		t.Fatalf("the first live call steps to %q and %q", o.Prev, o.Next)
	}
	// a live call that went on as an agent run is a run row: a step lands on
	// its preview, both ways
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{Name: "agent", Run: "r1", Status: CallRun},
		{Name: "read", Input: "c.go", Status: CallRun},
	}})
	if err := h.openCall(-2); err != nil {
		t.Fatal(err)
	}
	if o := ui.opened[len(ui.opened)-1]; o.Prev != "agents show r1" || o.Next != "" {
		t.Fatalf("the call past a run row steps to %q and %q", o.Prev, o.Next)
	}
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	if o := ui.opened[len(ui.opened)-1]; o.Prev != "call 3" || o.Next != "call -2" {
		t.Fatalf("the run row's view steps to %q and %q", o.Prev, o.Next)
	}
	// a shown run has no live stream: the crossing is gated off both ways
	h.tMu.Lock()
	h.view = &agentRun{id: "r1", status: RunDone, tr: []ChatMessage{
		{Role: "user", Text: "look"}, {Kind: RowTool, Tool: "read", Text: "a.go", Status: CallOK},
	}}
	h.tMu.Unlock()
	if o := h.callView(2, mustRow(t, h, 2), 0); o.Prev != "" || o.Next != "" {
		t.Fatalf("a run's row steps to %q and %q", o.Prev, o.Next)
	}
	if prev, _ := h.callStep(-1); prev != "" {
		t.Fatalf("a live view under a run steps back to %q", prev)
	}
}

// TestCallRowInAView: the chat shows a run's own rows while a run is on
// screen, so the index names those.
func TestCallRowInAView(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	h.AppendChat(ChatMessage{Role: "user", Text: "the session's"})
	h.tMu.Lock()
	h.view = &agentRun{id: "r1", status: RunDone, tr: []ChatMessage{
		{Role: "user", Text: "look"}, {Kind: RowTool, Tool: "read", Text: "a.go", Status: CallOK, Output: "x"},
	}}
	h.tMu.Unlock()
	row, _, ok := h.chatRow(2)
	if !ok || row.Text != "a.go" {
		t.Fatalf("row 2 = %+v, want the shown run's second row", row)
	}
	if _, _, ok := h.chatRow(3); ok {
		t.Fatal("the run has two rows")
	}
	// a prompt is no call: only tool and thought rows open a view
	if err := h.openCall(1); err == nil || !strings.Contains(err.Error(), "not a call") {
		t.Fatalf("row 1: %v, want a refusal", err)
	}
}

func mustRow(t *testing.T, h *Host, n int) ChatMessage {
	t.Helper()
	row, _, ok := h.chatRow(n)
	if !ok {
		t.Fatalf("the chat shows no row %d", n)
	}
	return row
}

func fieldOf(o Overlay, name string) string {
	for _, f := range o.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

// TestCallWatcherSuperseded: one watcher serves the open call view, so
// stepping through live calls never piles watchers up whose clustered ticks
// blink the view's timer.
func TestCallWatcherSuperseded(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	ui := h.hooks().(*fakeUI)
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{Name: "shell", Input: "sleep 9", Status: CallRun},
		{Name: "read", Input: "a.go", Status: CallRun},
	}})
	h.watchCall(-1)
	h.watchCall(-2)
	if h.callWatchTick(-1) {
		t.Fatal("the superseded watcher keeps ticking")
	}
	if !h.callWatchTick(-2) {
		t.Fatal("the open view's watcher stopped")
	}
	rebuilt := 0
	for _, id := range ui.refreshed {
		if id == "call:-2" {
			rebuilt++
		}
	}
	if rebuilt != 1 { // the one live tick rebuilt its own view alone
		t.Fatalf("the ticks rebuilt %d views: %v", rebuilt, ui.refreshed)
	}
}

// TestCallViewFollowsItsCall: a live call view stays on the call it opened
// for as the turn writes on - a flush prunes settled calls out of the
// stream, so the index the view opened at names a later call afterwards.
func TestCallViewFollowsItsCall(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	ui := h.hooks().(*fakeUI)
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{ID: "call_1", Name: "shell", Input: "make build", Status: CallRun},
		{ID: "call_2", Name: "read", Input: "a.go", Status: CallRun},
	}})
	if err := h.openCall(-2); err != nil { // the last action
		t.Fatal(err)
	}
	// the first call settles and a third starts: the flush prunes the
	// settled prefix, so call_2 moves to the front of the stream
	h.agents.mu.Lock()
	h.agents.tools[0].Status = CallOK
	h.flushOutput(TurnStat{})
	h.agents.tools = append(h.agents.tools, StreamTool{ID: "call_3", Name: "read", Input: "b.go", Status: CallRun})
	h.agents.mu.Unlock()
	h.watchCall(-2)
	if !h.callWatchTick(-2) {
		t.Fatal("the view's watcher stopped while its call still runs")
	}
	ui.mu.Lock()
	got := ui.rebuilt[len(ui.rebuilt)-1]
	ui.mu.Unlock()
	if got.Title != "Read" || len(got.Fields) == 0 || fieldOf(got, "file") != "a.go" {
		t.Fatalf("the view swapped to %q (%+v), want the call it opened for", got.Title, got.Fields)
	}
}

// TestCallViewWaitingHasNoClock: a call waiting on its gate has not started,
// so its view names no time instead of one measured off a zero start.
func TestCallViewWaitingHasNoClock(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	ui := h.hooks().(*fakeUI)
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{ID: "call_1", Name: "edit", Input: "a.go", Status: CallWait},
	}})
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	opened := ui.overlays()
	if len(opened) == 0 {
		t.Fatal("the call view did not open")
	}
	got := opened[len(opened)-1]
	if got.Title != "Edit" || fieldOf(got, "time") != "" {
		t.Fatalf("a waiting call's view reads %+v, want no time field", got.Fields)
	}
}

// TestSkillBody: a skill's view reads the whole file, not the cut rows a
// transcript keeps, so the procedure is there to read.
func TestSkillBody(t *testing.T) {
	t.Parallel()
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, filepath.Join(h.Dirs.ConfigHome, "skills"), "mine", "a test skill", "")
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	h.syncSkills()
	body := h.skillBody(ChatMessage{Kind: RowTool, Tool: tools.ToolSkill, Text: "mine"})
	if !strings.Contains(body, "Body.") || strings.Contains(body, "Skill directory") {
		t.Fatalf("skill body = %q", body)
	}
	if got := h.skillBody(ChatMessage{Kind: RowTool, Tool: tools.ToolSkill, Text: "nope"}); got != "" {
		t.Fatalf("a skill gone since reads %q", got)
	}
	if got := h.skillBody(ChatMessage{Kind: RowTool, Tool: tools.ToolRead, Text: "mine"}); got != "" {
		t.Fatalf("a read is no skill: %q", got)
	}
}

// TestCallViewFollowsTheTranscript: a call view's step targets come from the
// transcript, so a row that lands after the view opened is one at once,
// without the reader reopening it.
func TestCallViewFollowsTheTranscript(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.AppendMessage("user", "ping")
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "a.go", Status: CallOK})
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "b.go", Status: CallOK})
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openCall(3); err != nil { // the run's last call: nowhere to step
		t.Fatal(err)
	}
	if got := ui.overlays(); got[len(got)-1].Next != "" {
		t.Fatalf("the last call's view steps to %q", got[len(got)-1].Next)
	}
	// the turn settles one more call: the open view grows a step target
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "read", Text: "c.go", Status: CallOK})
	ui.mu.Lock()
	rebuilt, ids := slices.Clone(ui.rebuilt), slices.Clone(ui.refreshed)
	ui.mu.Unlock()
	if len(rebuilt) == 0 || !slices.Contains(ids, "call:3") {
		t.Fatalf("the open view was not refreshed: %v", ids)
	}
	if got := rebuilt[len(rebuilt)-1].Next; got != "call 4" {
		t.Fatalf("the refreshed view steps to %q, want the call that landed", got)
	}
}

// TestRefreshLevelsUnderTheTurnLock: a turn settles its rows under the
// agent lock and refreshes the tracked levels there, so nothing that hook
// reaches may take that lock again - a live call's view maps to the row it
// lands on through the transcript alone.
func TestRefreshLevelsUnderTheTurnLock(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.AppendMessage("user", "ping")
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{{ID: "call_1", Name: "shell", Input: "sleep 9", Status: CallRun}}})
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		h.agents.mu.Lock()
		h.refreshLevels()
		h.agents.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshLevels deadlocks under the agent lock a turn's flush holds")
	}
}
