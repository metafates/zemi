package hostapi

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/tools"
)

// TestPlanHold: the plan file is written for an approval only, so a dismissal
// leaves the session's saved plan alone, and a hold is saved but starts nothing.
func TestPlanHold(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	sid := seedSession(t, h)
	if v, ok := h.approvePlan("# The first plan"); !ok || v.Path == "" {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	path := h.planPath(sid)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// a dismissed plan is not written: it would overwrite the saved one
	ui.planOK = false
	ui.planVerdict = tools.PlanVerdict{}
	if _, ok := h.approvePlan("# A plan the user dismissed"); ok {
		t.Fatal("a dismissed plan was approved")
	}
	if now, err := os.ReadFile(path); err != nil || string(now) != string(saved) {
		t.Fatalf("the saved plan moved: %q, %v", now, err)
	}
	// a hold saves and starts nothing
	ui.planOK = true
	ui.planVerdict = tools.PlanVerdict{Approved: true, Hold: true}
	v, ok := h.approvePlan("# The plan to keep")
	if !ok || !v.Approved || !v.Hold || v.Path == "" {
		t.Fatalf("hold: %+v ok=%v", v, ok)
	}
	if now, err := os.ReadFile(path); err != nil || !strings.Contains(string(now), "The plan to keep") {
		t.Fatalf("the held plan was not saved: %q, %v", now, err)
	}
}

// TestResumeShowsPlan: a resumed session draws the plan as a message above
// its call's row, as the live turn did; the row alone keeps only the verdict.
func TestResumeShowsPlan(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return textParts("done")
			}
			return toolCallParts("c1", "plan", `{"plan":"# The plan\nstep one"}`)
		}}, nil
	}
	if err := h.AgentSend("plan it"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	_, tr := historyFrom(rows)
	body := slices.IndexFunc(tr, func(m ChatMessage) bool { return m.Text == "# The plan\nstep one" })
	if body < 1 {
		t.Fatalf("no plan message: %+v", tr)
	}
	row := tr[body+1]
	if row.Kind != RowTool || row.Tool != tools.ToolPlan || !strings.Contains(row.Output, "approved") {
		t.Fatalf("plan message not above its row: %+v", row)
	}
}

// TestResumeShowsPlan: a resumed session draws the plan as a message above
// reply (the agent forgetting the plan tool) rather than a tool call.
func TestPlainTextEnd(t *testing.T) {
	plain := &agent.Runner{History: []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "# plan\nsteps"}}},
	}}
	if got := plainTextEnd(plain); got != "# plan\nsteps" {
		t.Fatalf("plain-text end = %q", got)
	}
	toolCall := &agent.Runner{History: []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{}}},
	}}
	if got := plainTextEnd(toolCall); got != "" {
		t.Fatalf("tool-call end = %q", got)
	}
	toolResult := &agent.Runner{History: []fantasy.Message{
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{}},
	}}
	if got := plainTextEnd(toolResult); got != "" {
		t.Fatalf("tool-result end = %q", got)
	}
}

// TestShellNotes: a !command shows in the transcript and its output rides
// along with the next message to the model, behind the attachment marker.
func TestShellNotes(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	var seen atomic.Value
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			seen.Store(last)
			return textParts("ok")
		}}, nil
	}
	// the model reads it as a shell result: escapes stripped
	h.RunShell(`printf '\033[1mshell-output\033[0m\n'`)
	waitFor(t, func() error {
		if tr := h.Session().Transcript; len(tr) != 1 || tr[0].Kind != "shell" || tr[0].Detail != "shell-output" {
			return fmt.Errorf("shell row missing: %+v", tr)
		}
		return nil
	})
	if err := h.AgentSend("what did it print?"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	got, _ := seen.Load().(string)
	if !strings.HasPrefix(got, "what did it print?"+agent.AttachMarker) || !strings.Contains(got, "\nshell-output\n") {
		t.Fatalf("model saw %q", got)
	}
	if h.takeShellNotes() != "" {
		t.Fatal("notes must be consumed by the message")
	}
}
