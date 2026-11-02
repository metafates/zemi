package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
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
	v, ok := h.approvePlan("# The first plan", "first")
	if !ok || v.Path == "" {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	saved, err := os.ReadFile(v.Path)
	if err != nil {
		t.Fatal(err)
	}
	// a dismissed plan is not written: it would overwrite the saved one
	ui.planOK = false
	ui.planVerdict = tools.PlanVerdict{}
	if _, ok := h.approvePlan("# A plan the user dismissed", "dismissed"); ok {
		t.Fatal("a dismissed plan was approved")
	}
	if now, err := os.ReadFile(v.Path); err != nil || string(now) != string(saved) {
		t.Fatalf("the saved plan moved: %q, %v", now, err)
	}
	// a hold saves and starts nothing
	ui.planOK = true
	ui.planVerdict = tools.PlanVerdict{Approved: true, Hold: true}
	held, ok := h.approvePlan("# The plan to keep", "kept")
	if !ok || !held.Approved || !held.Hold || held.Path == "" {
		t.Fatalf("hold: %+v ok=%v", held, ok)
	}
	if now, err := os.ReadFile(held.Path); err != nil || !strings.Contains(string(now), "The plan to keep") {
		t.Fatalf("the held plan was not saved: %q, %v", now, err)
	}
}

// TestPlanSavesUnderItsProject: a plan is saved in the state dir of the
// project it was approved in, under the name the model gave it, and a second
// plan of that name never writes over the first.
func TestPlanSavesUnderItsProject(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	approve := func(h *Host, plan, name string) string {
		t.Helper()
		h.SetUIHooks(&fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}})
		v, ok := h.approvePlan(plan, name)
		if !ok || v.Path == "" {
			t.Fatalf("approve %q: %+v ok=%v", name, v, ok)
		}
		return v.Path
	}
	one, _ := newWindow(t, state, t.TempDir())
	two, _ := newWindow(t, state, t.TempDir())
	projects := filepath.Join(state, "projects") + string(filepath.Separator)
	first, second := approve(one, "# One\nfirst steps", "the fix"), approve(two, "# Two\nother steps", "the fix")
	if first == second || !strings.HasPrefix(first, projects) {
		t.Fatalf("two projects share a plan: %q, %q", first, second)
	}
	for path, want := range map[string]string{first: "# One", second: "# Two"} {
		if got, err := os.ReadFile(path); err != nil || !strings.Contains(string(got), want) {
			t.Fatalf("%s: %q, %v", path, got, err)
		}
	}
	// the same plan approved again keeps its name; another plan of that name
	// takes the next one, so a path handed out once still holds its plan
	if again := approve(one, "# One\nfirst steps", "the fix"); again != first {
		t.Fatalf("the same plan moved: %q, %q", again, first)
	}
	next := approve(one, "# One\nrevised steps", "the fix")
	if next != strings.TrimSuffix(first, ".md")+"-2.md" {
		t.Fatalf("a second plan of the same name: %q", next)
	}
	if got, err := os.ReadFile(first); err != nil || !strings.Contains(string(got), "first steps") {
		t.Fatalf("the first plan moved: %q, %v", got, err)
	}
}

// TestPlanName: a plan's file is named by the model's own name, else by the
// plan's first heading.
func TestPlanName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, given, plan, want string }{
		{"the model named it", "Fix the plan path", "# Plan\nbody", "fix-the-plan-path"},
		{"no name: the heading", "", "## Plans per project\nbody", "plans-per-project"},
		{"no name, no heading", "", "1. do it", "plan"},
		{"a name with nothing to make of it", "!!!", "# Named\nbody", "named"},
	} {
		if got := planName(c.given, c.plan); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
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
	tr := h.Session().Transcript
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
