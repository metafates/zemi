package hostapi

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// plannerProfile defines a read-only profile named planner, the recipe the
// shipped profile used to be synthesized for (DESIGN §6.3).
func plannerProfile(t *testing.T, r *lua.Runtime) {
	t.Helper()
	runUser(t, r, `zemi.agent.profile.add("planner", { read_only = true })`)
}

// TestPlanMode: a read-only agent may change nothing
// and the prompt says so; it works whatever the permission mode.
func TestPlanMode(t *testing.T) {
	h, r := newTestHost(t)
	plannerProfile(t, r)
	check := func(subject, arg string) tools.Verdict {
		pipe := h.PipelineFor()
		pipe.Ask = nil
		return pipe.Check(tools.Request{Subject: subject, Arg: arg})
	}
	if v := check("fs.write", "/p/a.go"); !v.Allowed {
		t.Fatalf("allowed write = %+v", v)
	}
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	// the reason names the read-only denial; it cannot pick another agent
	if v := check("fs.write", "/p/a.go"); v.Allowed || v.DecidedBy != tools.ByReadOnly || !strings.Contains(v.Reason, "may not change anything") {
		t.Fatalf("read-only write = %+v by %s: %s", v, v.DecidedBy, v.Reason)
	}
	p, _ := h.resolveProfile("planner")
	if !strings.Contains(h.buildSystem(p), "# Planning") {
		t.Fatal("system prompt misses the planning section")
	}
}

// TestPlanApprovalSwitchesBack: approving a read-only agent's plan returns to
// the agent and mode it replaced, is no longer read-only, and records the
// implementation handoff planFollowup sends.
func TestPlanApprovalSwitchesBack(t *testing.T) {
	h, r := newTestHost(t)
	plannerProfile(t, r)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	if !h.readOnly() {
		t.Fatal("the profile must be read-only")
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
	parts, _ := json.Marshal(fantasy.NewUserMessage(got), json.Deterministic(true))
	if _, ok := userText(&store.Message{Role: "user", PartsJSON: string(parts)}); ok {
		t.Fatal("handoff would be recalled from the store as typed input")
	}
}

// TestPlanHold: the plan file is written for an approval only, so a dismissal
// leaves the session's saved plan alone, and a hold puts the agent's
// replacement back without recording the implementation handoff.
func TestPlanHold(t *testing.T) {
	h, r := newTestHostDB(t)
	plannerProfile(t, r)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	sid := seedSession(t, h)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	if v, ok := h.approvePlan("# The first plan"); !ok || v.Path == "" {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	if h.takePlanApproved() != implementNow {
		t.Fatal("a plain approval recorded no handoff")
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
	// a hold saves and restores, and starts nothing
	ui.planOK = true
	ui.planVerdict = tools.PlanVerdict{Approved: true, Hold: true}
	v, ok := h.approvePlan("# The plan to keep")
	if !ok || !v.Approved || !v.Hold || v.Path == "" {
		t.Fatalf("hold: %+v ok=%v", v, ok)
	}
	if got := h.Settings.Str("agent.profile"); got != "default" {
		t.Fatalf("profile after a hold = %q", got)
	}
	if h.takePlanApproved() != "" {
		t.Fatal("a hold recorded the implementation handoff")
	}
	if now, err := os.ReadFile(path); err != nil || !strings.Contains(string(now), "The plan to keep") {
		t.Fatalf("the held plan was not saved: %q, %v", now, err)
	}
}

// TestPlanFeedbackKeepsPlanner: a read-only plan the user asks to change does
// not switch the agent, so the agent revises and presents again.
func TestPlanFeedbackKeepsPlanner(t *testing.T) {
	h, r := newTestHost(t)
	plannerProfile(t, r)
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
		t.Fatalf("feedback must keep the agent, got %q", h.Settings.Str("agent.profile"))
	}
	if h.takePlanApproved() != "" {
		t.Fatal("feedback must not set the implementation handoff")
	}
}

// TestPlanFollowupClaims: a read-only reply in plain text is presented under a
// claim of its own, released after; a submit that claimed the agent first
// keeps its turn and scope, and no popup opens over it.
func TestPlanFollowupNudgesOnce(t *testing.T) {
	runner := &agent.Runner{History: []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "which file did you mean?"}}},
	}}
	h, r := newTestHostDB(t)
	plannerProfile(t, r)
	ui := &fakeUI{planOK: true}
	h.SetUIHooks(ui)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	ro, _ := h.resolveProfile("")
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
	// a text ending is not a plan to approve: the agent is told once
	h.planFollowup(runner, h.nudgesPlan(ro))
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
	// the second reply follows a notice row, never a bare gap
	if !slices.ContainsFunc(h.Session().Transcript, func(m ChatMessage) bool { return m.Kind == "notice" && strings.Contains(m.Text, "Asked the planner") }) {
		t.Fatalf("no nudge notice: %+v", h.Session().Transcript)
	}
	// a submit that claimed the agent first is left alone
	h.agents.mu.Lock()
	scope := h.claim()
	h.agents.mu.Unlock()
	h.planFollowup(runner, h.nudgesPlan(ro))
	h.endScope(scope)
	if nudges() != 1 {
		t.Fatal("nudged under another claim")
	}
	// headless has no plan tool to present with: the reply is the plan
	headless, r := newTestHostDB(t)
	plannerProfile(t, r)
	if err := headless.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	var headlessSent atomic.Int32
	headless.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart { headlessSent.Add(1); return textParts("text") }}, nil
	}
	headlessRO, _ := headless.resolveProfile("")
	headless.planFollowup(runner, headless.nudgesPlan(headlessRO))
	waitIdle(t, headless)
	if n := headlessSent.Load(); n != 0 {
		t.Fatalf("headless agent nudged %d times", n)
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

// TestPlanNudgeFollowsTheTurn: picking a read-only agent while the default
// agent replies does not nudge that reply, which it never wrote.
func TestPlanNudgeFollowsTheTurn(t *testing.T) {
	h, r := newTestHostDB(t)
	plannerProfile(t, r)
	h.SetUIHooks(&fakeUI{})
	var mu sync.Mutex
	var sent []string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			mu.Lock()
			sent = append(sent, last)
			mu.Unlock()
			if last == "fix it" {
				if err := h.SetProfile("planner"); err != nil {
					t.Error(err)
				}
			}
			return textParts("Fixed the typo.")
		}}, nil
	}
	if err := h.AgentSend("fix it"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	// a nudge the ended turn sent would reach the model before this message,
	// which the default agent answers without one
	if err := h.SetProfile("default"); err != nil {
		t.Fatal(err)
	}
	if err := h.AgentSend("what next?"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) < 2 || sent[1] != "what next?" {
		t.Fatalf("the model saw %q", sent)
	}
}

// TestNextTurnHandsOffApprovedPlan: an approved plan is sent to the restored
// agent under the claim the read-only turn held, so nothing runs in between.
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
	scope := h.claim()
	h.agents.mu.Unlock()
	h.setPlanApproved(implementNow)
	if next, text := h.nextTurn(scope, nil); next != nil || text != "" {
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

// TestPlanApprovalFromConfigPick: a read-only profile picked from config
// (no SetProfile, so nothing was recorded) restores default on approval.
func TestPlanApprovalFromConfigPick(t *testing.T) {
	h, r := newTestHost(t)
	plannerProfile(t, r)
	h.SetUIHooks(&fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}})
	runUser(t, r, `zemi.config.set("agent.profile", "planner")`)
	if !h.readOnly() {
		t.Fatal("the picked profile must be read-only")
	}
	if v, ok := h.approvePlan("# The plan"); !ok || !v.Approved || v.Profile != "default" {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	if p := h.Settings.Str("agent.profile"); p != "default" {
		t.Fatalf("profile %q, want default", p)
	}
}
