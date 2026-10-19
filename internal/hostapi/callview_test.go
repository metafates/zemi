package hostapi

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestCallView: a row's view takes its facts from the call and draws what the
// call wrote in the form it is written in, apart from what it acted on.
func TestCallView(t *testing.T) {
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
	h.AppendChat(ChatMessage{Role: "assistant", Kind: RowTool, Tool: "shell", Text: "make slow", Status: CallBG, Output: "still running, so it " + tools.JobHandoff + "job-3"})

	for _, c := range []struct {
		row    int
		title  string
		fields map[string]string
		parts  []BodyPart
	}{
		{
			1, "Command",
			map[string]string{"status": "failed"},
			[]BodyPart{{Text: "go test ./...", Lang: "bash"}, {Text: "FAIL\nmain_test.go"}},
		},
		{
			2, "Edit",
			map[string]string{"status": "ok", "file": "main.go"},
			[]BodyPart{{Text: "-old\n+new", Lang: "diff"}},
		},
		{
			3, "Read",
			map[string]string{"status": "ok", "file": "main.go"},
			[]BodyPart{{Text: "1\tpackage main"}},
		},
		{
			4, "Thought",
			map[string]string{"status": "", "time": "4.2s"},
			[]BodyPart{{Text: "the tests **fail**", Lang: "markdown"}},
		},
		{
			5, "Question",
			map[string]string{"status": "ok"},
			[]BodyPart{{Text: "Where?  in main\nnote: because\nWhat?  now"}},
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
