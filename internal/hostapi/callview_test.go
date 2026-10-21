package hostapi

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

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
	// the view steps along the turn's rail, and a question is a call view too
	for row, want := range map[int][2]string{
		1: {"", "call 2"}, 2: {"call 1", "call 3"}, 4: {"call 3", "call 5"},
		5: {"call 4", "call 6"}, 6: {"call 5", ""},
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
	// one live call, then a second: the view steps along the run still being
	// written, not along the rows that had settled when it opened
	if live.Next != "" || live.Prev != "" {
		t.Fatalf("one live call stepped to %q and %q", live.Prev, live.Next)
	}
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{Name: "shell", Input: "sleep 9", Status: CallRun},
		{Name: "read", Input: "a.go", Status: CallRun},
	}})
	if err := h.openCall(-1); err != nil {
		t.Fatal(err)
	}
	if first := ui.opened[len(ui.opened)-1]; first.Prev != "" || first.Next != "call -2" {
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
