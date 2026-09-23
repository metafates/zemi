package hostapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestPlanMode: a read-only agent (the planner profile) refuses file changes
// even under an allow rule and turns allowed commands that change things into
// asks, and the prompt says so; it works whatever the permission mode.
func TestPlanMode(t *testing.T) {
	h, _ := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "make deploy*", Decision: perm.Allow})
	check := func(subject, arg string) tools.Verdict {
		pipe := h.PipelineFor()
		pipe.Ask = nil
		return pipe.Check(tools.Request{Subject: subject, Arg: arg})
	}
	if v := check("fs.write", "/p/a.go"); v.Decision != perm.Allow {
		t.Fatalf("allowed write = %v", v.Decision)
	}
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	// the reason says what the planner can do; it cannot pick another agent
	if v := check("fs.write", "/p/a.go"); v.Decision != perm.Deny || v.DecidedBy != "profile:readonly" || !strings.HasPrefix(v.Reason, "the user is planning: describe") {
		t.Fatalf("read-only write = %v by %s: %s", v.Decision, v.DecidedBy, v.Reason)
	}
	if v := check("tool.bash", "make deploy"); v.Decision != perm.Ask {
		t.Fatalf("read-only bash = %v", v.Decision)
	}
	// the sandbox is what keeps the project read-only, so leaving it is denied
	// before any mode (yolo would settle it) or the user sees the ask
	for _, mode := range []string{"auto", "yolo"} {
		h.Settings.SetHost("permissions.mode", mode)
		pipe := h.PipelineFor()
		pipe.Ask = func(tools.Request) (bool, string, string) { return true, "user", "" }
		if v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "make deploy", Unsandboxed: true}); v.Decision != perm.Deny || v.DecidedBy != "profile:readonly" {
			t.Fatalf("%s: sandbox: false for a planner = %v by %s: %s", mode, v.Decision, v.DecidedBy, v.Reason)
		}
	}
	p, _ := h.resolveProfile("planner")
	if !strings.Contains(h.buildSystem(p), "# Planning") {
		t.Fatal("system prompt misses the planning section")
	}
}

// TestPlanSandbox: a read-only agent's commands run confined under every
// mode, but only with a backend; a policy with none fails every command.
func TestPlanSandbox(t *testing.T) {
	h, _ := newTestHost(t)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		live bool
		mode string
		want bool
	}{{false, "auto", false}, {false, "yolo", false}, {true, "auto", true}, {true, "yolo", true}} {
		fakeSandbox(h, c.live)
		h.Settings.SetHost("permissions.mode", c.mode)
		if got := h.PipelineFor().Sandbox() != nil; got != c.want {
			t.Fatalf("live=%v mode=%s: policy=%v, want %v", c.live, c.mode, got, c.want)
		}
	}
}

// TestPlanApprovalSwitchesBack: approving a read-only agent's plan returns to
// the agent and mode it replaced, is no longer read-only, and records the
// implementation handoff planFollowup sends.
func TestPlanApprovalSwitchesBack(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	if !h.readOnly() {
		t.Fatal("planner must be read-only")
	}
	v, ok := h.approvePlan("# The plan")
	if !ok || !v.Approved || v.Profile != "default" {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	if got := h.Settings.Str("agent.profile"); got != "default" {
		t.Fatalf("profile not restored to default, got %q", got)
	}
	if h.readOnly() {
		t.Fatal("still read-only after approval")
	}
	got := h.takePlanApproved()
	if got != implementNow {
		t.Fatalf("planApproved handoff = %q", got)
	}
	// the handoff is zemi's, not the user's: no transcript row, nothing to recall
	h.startTurn(got)
	for _, m := range h.Session().Transcript {
		if m.Role == "user" {
			t.Fatalf("handoff shown as the user's message: %q", m.Text)
		}
	}
	parts, _ := json.Marshal(fantasy.NewUserMessage(got))
	if _, ok := userText(&store.Message{Role: "user", PartsJSON: string(parts)}); ok {
		t.Fatal("handoff would be recalled from the store as typed input")
	}
}

// TestPlanFeedbackKeepsPlanner: a read-only plan the user asks to change does
// not switch the agent, so the planner revises and presents again.
func TestPlanFeedbackKeepsPlanner(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Feedback: "use sqlite"}}
	h.SetUIHooks(ui)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	v, ok := h.approvePlan("# The plan")
	if !ok || v.Approved {
		t.Fatalf("feedback must not approve: %+v ok=%v", v, ok)
	}
	if h.Settings.Str("agent.profile") != "planner" {
		t.Fatalf("feedback must keep the planner, got %q", h.Settings.Str("agent.profile"))
	}
	if h.takePlanApproved() != "" {
		t.Fatal("feedback must not set the implementation handoff")
	}
}

// TestPlanFollowupClaims: a planner reply in plain text is presented under a
// claim of its own, released after; a submit that claimed the agent first
// keeps its turn and scope, and no popup opens over it.
func TestPlanFollowupNudgesOnce(t *testing.T) {
	runner := &agent.Runner{History: []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "which file did you mean?"}}},
	}}
	h, _ := newTestHostDB(t)
	ui := &fakeUI{planOK: true}
	h.SetUIHooks(ui)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			mu.Lock()
			sent = append(sent, last)
			mu.Unlock()
			return textParts("still text") // every reply forgets the tools
		}}, nil
	}
	nudges := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range sent {
			if strings.Contains(s, "ended in plain text") {
				n++
			}
		}
		return n
	}
	// a text ending is not a plan to approve: the planner is told once
	h.planFollowup(runner)
	waitFor(t, func() error {
		if n := nudges(); n != 1 {
			return fmt.Errorf("nudges = %d", n)
		}
		return nil
	})
	waitIdle(t, h)
	if ui.planAsked != 0 || nudges() != 1 {
		t.Fatalf("asked %d, nudges %d after the nudged reply ended in text again", ui.planAsked, nudges())
	}
	// a submit that claimed the agent first is left alone
	h.agents.mu.Lock()
	h.agents.active = true
	scope := h.beginScope()
	h.agents.mu.Unlock()
	h.planFollowup(runner)
	h.endScope(scope)
	if nudges() != 1 {
		t.Fatal("nudged under another claim")
	}
	// the user's next message re-arms the nudge
	if err := h.AgentSend("go on"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if n := nudges(); n != 2 {
			return fmt.Errorf("nudges after the user spoke = %d", n)
		}
		return nil
	})
	waitIdle(t, h)
}

// TestNextTurnHandsOffApprovedPlan: an approved plan is sent to the restored
// agent under the claim the planner's turn held, so nothing runs in between.
func TestNextTurnHandsOffApprovedPlan(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	var mu sync.Mutex
	var sent []string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			mu.Lock()
			sent = append(sent, last)
			mu.Unlock()
			return textParts("done")
		}}, nil
	}
	h.agents.mu.Lock()
	h.agents.active = true
	scope := h.beginScope()
	h.agents.mu.Unlock()
	h.setPlanApproved(implementNow)
	if next, text := h.nextTurn(scope); next != nil || text != "" {
		t.Fatalf("the handoff must not run on the ended runner: %v %q", next, text)
	}
	if h.takePlanApproved() != "" {
		t.Fatal("handoff left behind")
	}
	waitIdle(t, h)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(sent, "\n"); !strings.Contains(got, "approved the plan") {
		t.Fatalf("implementation never reached the model: %q", got)
	}
}

// TestPlainTextEnd reports whether an idle run ended in a plain-text assistant
// reply (the planner forgetting the plan tool) rather than a tool call.
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
	h, _ := newTestHostDB(t)
	var seen atomic.Value
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			seen.Store(last)
			return textParts("ok")
		}}, nil
	}
	h.RunShell("echo shell-output")
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
	if !strings.HasPrefix(got, "what did it print?"+attachMarker) || !strings.Contains(got, "shell-output") {
		t.Fatalf("model saw %q", got)
	}
	if h.takeShellNotes() != "" {
		t.Fatal("notes must be consumed by the message")
	}
}
