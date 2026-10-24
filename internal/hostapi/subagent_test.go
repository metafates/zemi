package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// scriptModel answers every turn's stream call with the parts its script
// returns for that call (scripts must be safe for concurrent calls); side
// calls fail.
type scriptModel struct {
	script func(system, last string, toolResult bool) []fantasy.StreamPart
	// record, when set, sees every call the host makes, real ones only.
	record func(fantasy.Call)
}

func (m *scriptModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if sideCall(call) {
		return nil, errors.New("not implemented")
	}
	if m.record != nil {
		m.record(call)
	}
	var system, last string
	toolResult := false
	for _, msg := range call.Prompt {
		for _, p := range msg.Content {
			switch p := p.(type) {
			case fantasy.TextPart:
				if msg.Role == fantasy.MessageRoleSystem {
					system += p.Text
				} else {
					last, toolResult = p.Text, false
				}
			case fantasy.ToolResultPart:
				if txt, ok := p.Output.(fantasy.ToolResultOutputContentText); ok {
					last = txt.Text
				}
				toolResult = true
			}
		}
	}
	parts := m.script(system, last, toolResult)
	if parts == nil { // a provider that never answers: the call ends with its cancel
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return slices.Values(parts), nil
}

func (m *scriptModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptModel) Provider() string { return "stub" }
func (m *scriptModel) Model() string    { return "script-1" }

func textParts(text string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}
}

func toolCallParts(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
	}
}

func reasoningParts(text string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
		{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: text},
		{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
	}
}

// TestAgentToolRunsSubagent drives one parent turn that launches a
// subagent: its report returns as the tool result, the tool row keeps the
// progress detail, and the run is a child session hidden from the list.
func TestAgentToolRunsSubagent(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				return textParts("sub report for: " + last)
			case toolResult:
				return textParts("parent saw: " + last)
			}
			return toolCallParts("call-1", "agent", `{"description":"find things","prompt":"look around","agent":"explore"}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	var reply, detail, runID string
	for _, m := range h.Session().Transcript {
		if m.Kind == "tool" && m.Tool == "agent" {
			detail, runID = m.Detail, m.Run
		}
		if m.Role == "assistant" && m.Kind == "" {
			reply = m.Text
		}
	}
	// the run id heads the report, so send_to can continue the run
	if !strings.HasPrefix(reply, "parent saw: [run "+runID+"]\n\nsub report for: look around") {
		t.Fatalf("reply = %q", reply)
	}
	if !strings.HasPrefix(detail, "explore · 0 tool calls") {
		t.Fatalf("detail = %q", detail)
	}
	list, err := h.DB.ListSessions(10)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions = %v %v", list, err)
	}

	// /agents: the run keeps its transcript, the chat can show it, and a run
	// this process no longer holds reads the same rows from the store.
	sid := h.AgentSessionID()
	runs := h.tasks.runsOf(sid)
	if len(runs) != 1 || runID != runs[0].id {
		t.Fatalf("runs = %v", runs)
	}
	status, _, _, sid := runs[0].state()
	want := []ChatMessage{{Role: "user", Text: "look around"}, {Role: "assistant", Text: "sub report for: look around"}}
	if got := runs[0].transcript(); status != "done" || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("status %s, transcript %+v", status, got)
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.ViewAgent(runs[0].id); err != nil || h.viewedRun() != runs[0] {
		t.Fatalf("view: %v", err)
	}
	forgetRun(h, runID)
	stored, err := h.loadRun(sid)
	if err != nil {
		t.Fatalf("stored run: %v", err)
	}
	for i := range stored.tr {
		// the store stamps a prompt's clock and names its row; the live row
		// has neither yet
		stored.tr[i].SentAt, stored.tr[i].ID = 0, ""
	}
	if got := stored.transcript(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored run: %+v", got)
	}
}

// TestSeedStreamSeedsEveryField: a field a caller seeds must read back from
// AgentStream, or a golden silently draws the live turn's zero instead. The
// skips are the fields the read derives rather than holds.
func TestSeedStreamSeedsEveryField(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	s := StreamSnapshot{
		Active: true, ByLoop: true, Text: "t", Reasoning: "r", Thinking: true,
		Model: "a/b", Thought: 2 * time.Second, Elapsed: 3 * time.Second,
		SessionIn: 1, SessionOut: 2, SessionCost: 3.5, LastIn: 4, CtxWindow: 5000,
		CacheRead: 6, CacheBilled: 7, Retrying: true, RetryIn: 8 * time.Second, RetryWhy: "overloaded",
		Retries: 9, Turn: 10, Compacting: true, CheckingGoal: true,
		Tools: []StreamTool{{ID: "c1", Name: "shell"}},
	}
	h.SeedStream(s)
	// CtxWindow falls back to the model's; Model to the live one; Thought,
	// Elapsed and RetryIn count from the seed on the host clock; Steering
	// takes AgentSend, which builds the queue.
	derived := map[string]string{
		"Model":     "empty seeds the live model",
		"CtxWindow": "0 seeds the model's window", "Thought": "counts from the seed",
		"Elapsed": "counts from the seed", "RetryIn": "counts from the seed",
		"Steering": "AgentSend builds it",
	}
	got := h.AgentStream()
	want, have := reflect.ValueOf(s), reflect.ValueOf(got)
	for i := range want.NumField() {
		name := want.Type().Field(i).Name
		if _, ok := derived[name]; ok {
			continue
		}
		if !reflect.DeepEqual(want.Field(i).Interface(), have.Field(i).Interface()) {
			t.Errorf("SeedStream does not seed %s: %v, want %v", name, have.Field(i).Interface(), want.Field(i).Interface())
		}
	}
}

// TestSubagentStoresThinkingTime: a run's stored reasoning row carries the
// run's own thinking time, like a session's, so a run read back from the
// store (/agents) shows it.
func TestSubagentStoresThinkingTime(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) { return thoughtModel{gap: 5 * time.Millisecond}, nil }
	def, err := h.agentDef("explore")
	if err != nil {
		t.Fatal(err)
	}
	id, text, err := h.runSubagent(t.Context(), def, "d", "look")
	if err != nil || text != "the report" {
		t.Fatalf("run: %v %q", err, text)
	}
	forgetRun(h, id)
	run, err := h.loadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range run.transcript() {
		if m.Kind == RowReasoning && m.Stat != nil && m.Stat.ThoughtMS >= 5 {
			return
		}
	}
	t.Fatalf("the stored run keeps no thinking time: %+v", run.transcript())
}

// thoughtModel streams a thought, waits its gap, then the report: a run's
// thinking time is wall time, so a test needs a gap the clock can see.
type thoughtModel struct{ gap time.Duration }

func (m thoughtModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range reasoningParts("thinking about it") {
			if !yield(p) {
				return
			}
		}
		time.Sleep(m.gap)
		for _, p := range textParts("the report") {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (thoughtModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (thoughtModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (thoughtModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (thoughtModel) Provider() string { return "stub" }
func (thoughtModel) Model() string    { return "script-1" }

// TestSubagentFailureKeepsReport: a step failing after the agent already
// reported something returns that partial report with the error, instead of
// throwing the finished work away.
func TestSubagentFailureKeepsReport(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("provider down")}}
			}
			return append(textParts("partial finding")[:3], toolCallParts("c1", "read", `{"file_path":"/nope"}`)...)
		}}, nil
	}
	def, err := h.agentDef("explore")
	if err != nil {
		t.Fatal(err)
	}
	_, text, err := h.runSubagent(t.Context(), def, "d", "look")
	if err == nil {
		t.Fatal("want the step error")
	}
	if text != "partial finding" {
		t.Fatalf("partial report = %q", text)
	}
}

// TestSubagentSurvivesAFullWindow: a subagent has no summarizer, so a
// context the provider refuses clears its old tool output, or with none to
// clear drops the middle of its history once, and the run goes on.
func TestSubagentSurvivesAFullWindow(t *testing.T) {
	big := agent.UserMessage(strings.Repeat("r", 120_000))
	big.Role = fantasy.MessageRoleAssistant
	for _, c := range []struct {
		name     string
		history  []fantasy.Message
		refusals int
		base     int // where the run's own messages start
		wantBase int
		check    func([]fantasy.Message) bool
	}{
		{
			"old tool output is cleared",
			[]fantasy.Message{agent.UserMessage("look"), toolResult("old", 30_000), toolResult("new", toolKeepTokens)},
			1, 0, 0,
			func(h []fantasy.Message) bool {
				return toolText(h[1].Content[0].(fantasy.ToolResultPart)) == clearedOutput
			},
		},
		{
			"nothing to clear drops the middle, not the task",
			[]fantasy.Message{agent.UserMessage("look"), big},
			1, 0, 0,
			func(h []fantasy.Message) bool {
				return concatText(h[0].Content) == "look" && estTokens(h...) < 1_000
			},
		},
		{
			"the drop carries the run's own opening with it",
			[]fantasy.Message{agent.UserMessage("look"), big, agent.UserMessage("more")},
			1, 3, 2, nil,
		},
		{"a second refusal ends the run", []fantasy.Message{agent.UserMessage("look"), big}, 9, 0, 0, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			sends := 0
			model := &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
				if sends++; sends <= c.refusals {
					return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{StatusCode: 400, Message: "prompt is too long", ContextTooLargeErr: true}}}
				}
				return textParts("report")
			}}
			runner := agent.NewRunner(model, nil, "sys")
			runner.Hooks = &subagentHooks{h: h, def: AgentDef{Name: "explore"}, run: &agentRun{status: "running", rows: map[string]int{}}}
			runner.History = c.history
			base, err := h.sendPruning(t.Context(), runner, "p/m", "go", c.base)
			wantErr := c.refusals > 1
			if (err != nil) != wantErr || sends != 2 {
				t.Fatalf("send: %v (streams %d)", err, sends)
			}
			if base != c.wantBase {
				t.Fatalf("the run's own messages start at %d, want %d", base, c.wantBase)
			}
			if !wantErr && lastAssistantText(runner.History) != "report" || c.check != nil && !c.check(runner.History) {
				t.Fatalf("history after the retry: %d messages, last text %q", len(runner.History), lastAssistantText(runner.History))
			}
		})
	}
}

// TestSubagentAsksAsAgent: a subagent's host-checked tools ask through its
// own pipeline, so the ask names the agent.
func TestSubagentAsksAsAgent(t *testing.T) {
	for _, tc := range []struct{ name, tool string }{
		{"lua tool", "weather"},
		{"forgetting a memory the user saved", tools.ToolMemory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHostDB(t)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			askEverything(t, r)
			runUser(t, r, `zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })`)
			if err := h.Remember("deploy by hand"); err != nil {
				t.Fatal(err)
			}
			args := `{"text": "", "replaces": "` + projectMemories(t, h)[0].ID + `"}`
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
					if toolResult {
						return textParts("done")
					}
					return toolCallParts("c1", tc.tool, args)
				}}, nil
			}
			def, err := h.agentDef("general")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.runSubagent(t.Context(), def, "d", "look"); err != nil {
				t.Fatal(err)
			}
			if len(ui.asked) != 1 || ui.asked[0].Agent != "general agent" {
				t.Fatalf("asked: %+v", ui.asked)
			}
		})
	}
}

// TestSubagentInstructions: every run's first read of a file carries its
// package's instruction file, and none of them uses up the main
// conversation's one delivery.
func TestSubagentInstructions(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetProjectTrusted(true)
	pkg := filepath.Join(h.Dirs.ProjectDir, "sub")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pkg, "AGENTS.md"), []byte("use tabs here"), 0o644)
	file := filepath.Join(pkg, "x.go")
	os.WriteFile(file, []byte("package sub\n"), 0o644)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return textParts(last)
			}
			return toolCallParts("c1", "read", fmt.Sprintf(`{"path":%q,"limit":0}`, file))
		}}, nil
	}
	def, err := h.agentDef("explore")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, text, err := h.runSubagent(t.Context(), def, "d", "look"); err != nil || !strings.Contains(text, "use tabs here") {
			t.Fatalf("run %d: read result %q, %v", i, text, err)
		}
	}
	if got := h.nearestInstructions(file, h.instructionFiles(), h.instructionSent); !strings.Contains(got, "use tabs here") {
		t.Fatalf("the main conversation lost its delivery: %q", got)
	}
}

// TestSubagentOwnDir: runs of one agent share a system prompt, so the
// provider cache serves it, and each is told its own directory in the pad in
// its task.
func TestSubagentOwnDir(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	var systems []string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, _ bool) []fantasy.StreamPart {
			systems = append(systems, system)
			return textParts(last)
		}}, nil
	}
	def, err := h.agentDef("general")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for range 2 {
		_, text, err := h.runSubagent(t.Context(), def, "d", "look")
		_, dir, ok := strings.Cut(text, "keep your own files in "+filepath.Join(h.ScratchDir(), "general-"))
		if err != nil || !ok {
			t.Fatalf("task %q, %v", text, err)
		}
		dirs = append(dirs, dir)
	}
	if len(systems) != 2 || systems[0] != systems[1] || dirs[0] == dirs[1] {
		t.Errorf("%d prompts, equal %v; dirs %q", len(systems), len(systems) == 2 && systems[0] == systems[1], dirs)
	}
}

// TestSubagentReportTruncated: a runaway report is cut before it lands in
// the caller's context, and the whole text waits in the scratchpad.
func TestSubagentReportTruncated(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			return textParts(strings.Repeat("x", 40_000))
		}}, nil
	}
	// the schema words a wrong type as the builtin tools do
	for in, want := range map[string]string{`{"prompt":1}`: `field "prompt" must be string`, `[1]`: "input must be a JSON object"} {
		if resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "c0", Name: "agent", Input: in}); !resp.IsError || resp.Content != want {
			t.Fatalf("%s: %+v", in, resp)
		}
	}
	resp, err := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{
		ID: "c1", Name: "agent", Input: `{"description":"d","prompt":"look","agent":"explore"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) > 31_200 || !strings.Contains(resp.Content, " cut) ...") {
		t.Fatalf("report is %d bytes and ends %q", len(resp.Content), truncRunes(resp.Content, 20))
	}
	_, path, ok := strings.Cut(resp.Content, "(output saved to ")
	path = strings.TrimSuffix(strings.TrimSpace(path), ")")
	if !ok {
		t.Fatalf("no spill file named: %q", truncRunes(resp.Content, 40))
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 40_000 {
		t.Fatalf("spilled file = %d bytes, %v", len(b), err)
	}
}

func TestAgentDefs(t *testing.T) {
	cfg, proj := t.TempDir(), t.TempDir()
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(cfg, "agents"), "reviewer.md", "---\ndescription: reviews diffs\ntools: read, grep\nmodel: inherit\nread_only: true\n---\nBe harsh.\n")
	write(filepath.Join(cfg, "agents"), "explore.md", "---\ndescription: my explorer\ntools: [read]\n---\nmine\n")
	write(filepath.Join(cfg, "agents"), "broken.md", "---\ntools: read\n---\nno description\n")
	write(filepath.Join(proj, ".agents", "agents"), "general.md", "---\ndescription: project override\n---\nx\n")
	write(filepath.Join(proj, ".agents", "agents"), "tester.md", "---\ndescription: runs tests\n---\nx\n")
	write(filepath.Join(proj, ".claude", "agents"), "claudely.md", "---\ndescription: not read\n---\nx\n")

	log := slog.New(slog.DiscardHandler)
	h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{ConfigHome: cfg, ProjectDir: proj})
	get := func() map[string]AgentDef {
		out := map[string]AgentDef{}
		for _, d := range h.agentDefs() {
			out[d.Name] = d
		}
		return out
	}

	if builtinAgents[0].Name != forkAgent {
		t.Fatal("builtinAgents order changed; fix the general row below")
	}
	general := slices.IndexFunc(builtinAgents, func(d AgentDef) bool { return d.Name == "general" })
	if general < 0 {
		t.Fatal("no general agent among the builtins")
	}
	defs := get()
	if _, ok := defs["tester"]; ok {
		t.Fatal("project agents must wait for trust")
	}
	if _, err := h.agentDef("tester"); err == nil || !strings.Contains(err.Error(), "runs /trust") || !strings.Contains(err.Error(), ".agents") {
		t.Fatalf("untrusted project agent = %v", err)
	}
	h.SetProjectTrusted(true)
	defs = get()
	for _, c := range []struct {
		name, source, desc, tools, system string
		readOnly                          bool
	}{
		{"reviewer", "user", "reviews diffs", "read,grep", "Be harsh.", true},
		{"explore", "user", "my explorer", "read", "mine", false},
		{"general", "builtin", builtinAgents[general].Description, "", builtinAgents[general].System, false},
		{"tester", "project", "runs tests", "", "x", false},
	} {
		d, ok := defs[c.name]
		if !ok {
			t.Fatalf("%s missing", c.name)
		}
		if string(d.Source) != c.source || d.Description != c.desc || strings.Join(d.Tools, ",") != c.tools || d.System != c.system || d.Model != "" || d.ReadOnly != c.readOnly {
			t.Fatalf("%s = %+v", c.name, d)
		}
	}
	if _, ok := defs["broken"]; ok {
		t.Fatal("an agent without description must be skipped")
	}
	if _, ok := defs["claudely"]; ok {
		t.Fatal(".claude/agents must not load")
	}
	// the skipped file is reported to /errors, once over both get() calls
	if n := len(h.errs.entries); n != 1 || !strings.Contains(h.errs.entries[0].msg, "broken.md") {
		t.Fatalf("errors=%d %+v", n, h.errs.entries)
	}
}

// TestSubagentModelChoice: a subagent's model comes from its definition's
// reference, which may name a role, then models.agents, which follows
// models.default. A reference config does not know is refused with the roles
// and the configured models.
func TestSubagentModelChoice(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.provider.add({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1", api_key = "k",
		models = { { id = "main" }, { id = "mini" } },
	})`)
	for _, c := range []struct {
		def, deflt, agents string
		want               string
	}{
		{"", "", "", "myllm/main"}, // nothing set: the provider's default
		{"", "myllm/mini", "", "myllm/mini"},
		{"", "myllm/main", "myllm/mini", "myllm/mini"},
		{"myllm/main", "myllm/mini", "myllm/mini", "myllm/main"},
		{"default", "myllm/main", "myllm/mini", "myllm/main"},
	} {
		h.Settings.SetQuiet("models.default", c.deflt)
		h.Settings.SetQuiet("models.agents", c.agents)
		if got := h.subagentModelID(AgentDef{Model: c.def}); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
	// the catalog can be thousands of tokens; a bad id names it instead
	if desc := h.agentTool(nil).Info().Description; strings.Contains(desc, "myllm/main") {
		t.Fatalf("description lists models: %q", desc)
	}
	if err := h.checkModelID("other/x"); err == nil || err.Error() != `unknown model "other/x"; targets: default, chat, agents, titles, compact, memory; configured models: myllm/main, myllm/mini` {
		t.Fatalf("unknown model: %v", err)
	}
}

// TestSubagentMemoryKeepsItsSession: a memory a run writes after the user
// moved to another session carries the run's own session as provenance.
func TestSubagentMemoryKeepsItsSession(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Settings.SetQuiet("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return textParts("done")
			}
			seedSession(t, h) // the user ran /new and sent a message
			return toolCallParts("c1", "memory", `{"scope":"project","text":"the build uses make"}`)
		}}, nil
	}
	def, err := h.agentDef("general")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.runSubagent(t.Context(), def, "d", "look"); err != nil {
		t.Fatal(err)
	}
	live := projectMemories(t, h)
	if len(live) != 1 || live[0].ProvenanceSessionID != sid || len(h.memoryProposals()) != 0 {
		t.Fatalf("want one memory from %s, nothing queued: %+v", sid, live)
	}
}

// TestAgentsList: /agents leads with New agent and details each agent, the
// user's first, opening the ones that are files. A background job is no
// agent: /jobs has it.
func TestAgentsList(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	id, err := h.jobs.Start(t.TempDir(), "echo up; sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := h.jobs.Get(id)
	waitFor(t, func() error {
		if j.Output() == "" {
			return errors.New("the job never printed")
		}
		return nil
	})
	defer h.jobs.KillAll()
	h.Dirs.ConfigHome = t.TempDir()
	if err := os.MkdirAll(filepath.Join(h.Dirs.ConfigHome, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "agents", "reviewer.md"), []byte("---\ndescription: reviews diffs\nread_only: true\n---\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := h.Commands.Lookup("agents")
	if err := agents.GoFn(h, ""); err != nil || len(ui.opened) != 1 {
		t.Fatalf("open: %v", err)
	}
	rows := map[string]string{}
	for _, it := range ui.opened[0].Items {
		rows[it.Text] = dumpItem(it)
	}
	if first := ui.opened[0].Items[0]; first.Text != "New agent" || first.Fill != "/agents new " {
		t.Errorf("first row = %+v", first)
	}
	if s := ui.opened[0].Items[1].Section; s != "User agents" {
		t.Errorf("first agent section %q, want User agents", s)
	}
	if _, listed := rows[id]; listed {
		t.Errorf("job %s is listed in /agents: %s", id, rows[id])
	}
	for _, c := range []struct {
		row  string
		want []string
		not  []string
	}{
		{"reviewer", []string{"[path ", "reviewer.md]", "[access read only]", "run:agents open reviewer"}, nil},
		{"general", []string{"[access can change files]"}, []string{"[path ", "actions:"}},
		{"explore", []string{"[access read only]"}, []string{"actions:"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(rows[c.row], w) {
				t.Errorf("%s row lacks %q: %s", c.row, w, rows[c.row])
			}
		}
		for _, w := range c.not {
			if strings.Contains(rows[c.row], w) {
				t.Errorf("%s row has %q: %s", c.row, w, rows[c.row])
			}
		}
	}
	if err := h.Commands.Lookup("jobs").GoFn(h, "stop "+id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if j.Running() {
			return errors.New("job still running after stop")
		}
		return nil
	})
}

// TestUpdateToolReportsAFailedSend: a child whose line never reached the
// caller is told so, instead of reading "Sent to the caller." forever.
func TestUpdateToolReportsAFailedSend(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t) // no model: the parent's send refuses
	h.SetUIHooks(&fakeUI{})
	run, _ := h.newRun(t.Context(), AgentDef{Name: "general"}, "d", "look", true)
	tool := &updateTool{h: h, pipe: h.PipelineFor()}
	ctx := context.WithValue(t.Context(), runKey{}, run)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tools.ToolUpdate, Input: `{"text":"found it"}`})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "did not reach the caller") {
		t.Fatalf("a failed update = %+v, %v", resp, err)
	}
}

// TestFilterToolsNamesTheUnknown: an agent file spelling its tools another
// harness's way names nothing here, and a run with no tools would invent
// its report instead of failing.
func TestFilterToolsNamesTheUnknown(t *testing.T) {
	h, _ := newTestHostDB(t)
	all := tools.All(h.PipelineFor())
	for _, c := range []struct {
		name    string
		want    []string
		kept    []string // the whole set, when the case pins it
		gone    []string
		unknown []string
	}{
		{name: "allowlist", want: []string{"read", "Grep", "agent", "lua"}, kept: []string{"read"}, unknown: []string{"Grep"}},
		{name: "removal keeps the rest", want: []string{"-read", "-Grep"}, gone: []string{"read"}, unknown: []string{"Grep"}},
		{name: "removal wins over the allowlist", want: []string{"read", "grep", "-grep"}, kept: []string{"read"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			kept, unknown := filterTools(all, c.want, subagentExcluded)
			var names []string
			for _, k := range kept {
				names = append(names, k.Info().Name)
			}
			if c.kept != nil && !slices.Equal(names, c.kept) {
				t.Errorf("kept %q, want %q", names, c.kept)
			}
			for _, g := range c.gone {
				if slices.Contains(names, g) || len(names) < 2 {
					t.Errorf("kept %q", names)
				}
			}
			if !slices.Equal(unknown, c.unknown) {
				t.Errorf("unknown = %q, want %q", unknown, c.unknown)
			}
		})
	}
}

// TestHaltKeepsTheStep: a tool.pre halt on one call of a parallel step ends
// the turn after the step, so the write that already ran stays in the
// history and the store instead of vanishing with a failed step.
func TestHaltKeepsTheStep(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("tool.pre", function(ev) if ev.tool == "glob" then return { decision = "halt", reason = "stop" } end end)`)
	file := filepath.Join(h.Dirs.ProjectDir, "new.txt")
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return textParts("went on past the halt")
			}
			return append(toolCallParts("w1", "write", fmt.Sprintf(`{"path":%q,"content":"x"}`, file))[:4], toolCallParts("g1", "glob", `{"pattern":"*"}`)...)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	history := fmt.Sprint(h.agents.runner.History)
	h.agents.mu.Unlock()
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	stored := ""
	for _, m := range rows {
		stored += m.PartsJSON
	}
	for name, got := range map[string]string{"history": history, "store": stored} {
		if !strings.Contains(got, "w1") || !strings.Contains(got, "halted by hook: stop") || strings.Contains(got, "went on") {
			t.Errorf("%s lost the step or ran past the halt: %s", name, got)
		}
	}
}

// pinState is the pinned prefix the provider caches: which key built it, the
// system prompt it holds and the tools' schemas, as bytes.
func pinState(h *Host) string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	b, _ := json.Marshal(h.agents.pin.infos, json.Deterministic(true))
	return h.agents.pin.key + "\x00" + h.agents.pin.text + "\x00" + string(b)
}

// TestAgentUpdateReachesTheCaller: what a running agent sends with the update
// tool joins the caller's turn at its next step boundary, drawn as a notice
// row of its own rather than as something the user typed, and leaves the
// pinned prompt and tools, which the provider caches, alone.
func TestAgentUpdateReachesTheCaller(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	var marks [2]string // the pinned prefix at the child's call, and at the caller's next step
	saw := ""
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				if strings.HasPrefix(last, "look") {
					marks[0] = pinState(h)
					// a child that finds something the task waits on says so at once
					return toolCallParts("t1", "update", `{"text":"the flaky test is in x_test.go"}`)
				}
				return textParts("child done")
			case strings.Contains(last, "<agent-update "):
				marks[1], saw = pinState(h), last
				return textParts("read it")
			case strings.Contains(last, "<agent-report "):
				// the call woke on the update, so the run reports as a message
				return textParts("noted")
			}
			return toolCallParts("c1", "agent", `{"description":"d","prompt":"look","agent":"general"}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if !strings.Contains(saw, "the flaky test is in x_test.go") {
		t.Fatalf("the caller never read the update: %q", saw)
	}
	notices, typed := 0, 0
	for _, m := range h.Session().Transcript {
		switch {
		case m.Kind == RowNotice && strings.HasPrefix(m.Text, "Agent ") && strings.Contains(m.Text, "the flaky test is in x_test.go"):
			notices++
		case m.Role == fantasy.MessageRoleUser && strings.Contains(m.Text, "flaky test"):
			typed++
		}
	}
	if notices != 1 || typed != 0 {
		t.Fatalf("the update drew %d notice rows and %d user rows: %+v", notices, typed, h.Session().Transcript)
	}
	if marks[0] == "" || marks[0] != marks[1] {
		t.Fatalf("the pinned prefix moved: %q -> %q", marks[0], marks[1])
	}
}

// TestAgentMessageKeepsThePin: a caller's steer on a running agent rides the
// child's queue, never the pinned prefix.
func TestAgentMessageKeepsThePin(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{}) // a detached run is a TUI feature
	sid := seedSession(t, h)
	release := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				<-release
				return textParts("child done")
			case toolResult:
				return textParts("started it")
			case strings.Contains(last, "<agent-report "):
				return textParts("got the report")
			}
			return toolCallParts("c1", "agent", `{"description":"d","prompt":"look","agent":"general","background":true}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	runs := h.tasks.runsOf(sid)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	before := pinState(h)
	if err := h.MessageAgent(runs[0].id, "also check y"); err != nil {
		t.Fatal(err)
	}
	if q := runs[0].queued(); len(q) != 1 || q[0] != "also check y" {
		t.Fatalf("queue = %q", q)
	}
	if after := pinState(h); after != before {
		t.Fatalf("a steer moved the pinned prefix: %q -> %q", before, after)
	}
	close(release)
	waitIdle(t, h)
}

// TestSubagentUpdatePrompt: every child holds the update tool and is told about
// it, so an agent file's allowlist cannot take the way back to its caller away.
func TestSubagentUpdatePrompt(t *testing.T) {
	h, _ := newTestHostDB(t)
	def := AgentDef{Name: "general"}
	for _, c := range []struct {
		name  string
		tools []string
	}{
		{name: "the full set"},
		{name: "an allowlist without it", tools: []string{"read"}},
		{name: "an allowlist naming it", tools: []string{"read", "update"}},
		{name: "an allowlist removing it", tools: []string{"-update"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			kept, unknown := childTools(h, h.PipelineFor(), c.tools)
			if len(unknown) > 0 {
				t.Fatalf("unknown tools %q", unknown)
			}
			if sys := h.subagentSystem(def, kept, false); !strings.Contains(sys, "update tool") {
				t.Fatal("the prompt does not tell the child about updates")
			}
			if !hasTool(kept, tools.ToolUpdate) {
				t.Fatal("the tool set lost the update tool")
			}
		})
	}
}

// TestForkContinuesTheCallersPrefix: a fork sends the caller's own request -
// its tools, system prompt, context message and messages, under its cache key -
// and then its task. That prefix is what makes the provider read the
// conversation from its cache instead of writing it out again.
func TestForkContinuesTheCallersPrefix(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.Dirs.ConfigHome = t.TempDir()
	if err := os.MkdirAll(filepath.Join(h.Dirs.ConfigHome, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// the fork has its own model to resolve and options of its own to build, so
	// the rows below hold on values rather than on nil: the run it continues is
	// on the chat model, and the agents role is another
	runUser(t, r, `zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", body = { service_tier = "flex" },
		models = { { id = "big", max_output = 1000 }, { id = "small", max_output = 2000 } } })
		zemi.config.set("models.agents", "p/small")`)
	var mu sync.Mutex
	var calls []fantasy.Call
	m := &scriptModel{record: func(c fantasy.Call) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, c)
	}}
	m.script = func(_, last string, toolResult bool) []fantasy.StreamPart {
		switch {
		case strings.Contains(last, forkTask):
			return textParts("the fork's report")
		case toolResult:
			return textParts("parent saw: " + last)
		}
		// a definition written now must not move the fork's tool array: it
		// sends the caller's own schemas
		if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "agents", "extra.md"), []byte("---\ndescription: late\n---\nbody"), 0o644); err != nil {
			t.Error(err)
		}
		return toolCallParts("call-1", "agent", `{"description":"check it","prompt":"review what we did","agent":"fork"}`)
	}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	if err := h.AgentSend("hello"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		for i, c := range calls {
			last := c.Prompt[len(c.Prompt)-1]
			t.Logf("call %d: %d messages, last %s %q", i, len(c.Prompt), last.Role, concatText(last.Content))
		}
		t.Fatalf("calls = %d, want the caller's step, the fork's and the caller's next", len(calls))
	}
	caller, fork, next := calls[0], calls[1], calls[2]
	if len(fork.Prompt) != len(caller.Prompt)+1 {
		t.Fatalf("the fork sent %d messages, want the caller's %d plus its task", len(fork.Prompt), len(caller.Prompt))
	}
	// the cache breakpoints ride on the last two messages of a request, so they
	// move with the fork's task and are not part of what it continues
	for i, msg := range caller.Prompt {
		if msg.Role != fork.Prompt[i].Role || !reflect.DeepEqual(msg.Content, fork.Prompt[i].Content) {
			t.Fatalf("message %d is not the caller's:\ncaller %+v\nfork   %+v", i, msg, fork.Prompt[i])
		}
	}
	// the # Brain section rides the caller's prefix, so the fork's first
	// request reuses its bytes: the per-message loop above holds it
	var ctxMsg string
	for _, msg := range caller.Prompt {
		if s := concatText(msg.Content); strings.Contains(s, "<context>") {
			ctxMsg = s
			break
		}
	}
	if !strings.Contains(ctxMsg, "# Brain") || !strings.Contains(ctxMsg, "deploy by hand") {
		t.Fatalf("the caller's context message does not carry the brain:\n%.400s", ctxMsg)
	}
	task := concatText(fork.Prompt[len(fork.Prompt)-1].Content)
	if !strings.HasPrefix(task, forkTask) || !strings.Contains(task, "review what we did") {
		t.Fatalf("the fork's task = %q", task)
	}
	for _, c := range []struct {
		what      string
		got, want any
	}{
		{"tools", fork.Tools, caller.Tools},
		{"provider options", fork.ProviderOptions, caller.ProviderOptions},
		{"headers", fork.Headers, caller.Headers},
		{"temperature", fork.Temperature, caller.Temperature},
		{"max output tokens", fork.MaxOutputTokens, caller.MaxOutputTokens},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Fatalf("the fork's %s differ from the caller's: %.200s vs %.200s", c.what, fmt.Sprint(c.got), fmt.Sprint(c.want))
		}
	}
	// the caller's own next step still opens with the prefix it had cached
	for i, msg := range caller.Prompt {
		if msg.Role != next.Prompt[i].Role || !reflect.DeepEqual(msg.Content, next.Prompt[i].Content) {
			t.Fatalf("the caller's next step changed message %d:\nbefore %+v\nnext   %+v", i, msg, next.Prompt[i])
		}
	}

	var reply string
	for _, m := range h.Session().Transcript {
		if m.Role == "assistant" && m.Kind == "" {
			reply = m.Text
		}
	}
	if !strings.Contains(reply, "the fork's report") {
		t.Fatalf("reply = %q", reply)
	}
	runs := h.tasks.runsOf(h.AgentSessionID())
	if len(runs) != 1 || runs[0].agent != forkAgent {
		t.Fatalf("runs = %+v", runs)
	}
	sess, err := h.DB.GetSession(runs[0].sessionID)
	if err != nil || sess.Agent != forkAgent || sess.ParentID != h.AgentSessionID() {
		t.Fatalf("fork session = %+v %v", sess, err)
	}
}

// TestForkKnowsItIsAFork: a fork that asks for another fork is refused in the
// words that tell it what it is, and the refusal is stored in its own session
// like any other tool result, so it reads it and does the work itself.
func TestForkKnowsItIsAFork(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case toolResult:
				return textParts("after the tool: " + last)
			case strings.Contains(last, forkTask):
				return toolCallParts("c2", "agent", `{"description":"again","prompt":"more","agent":"fork"}`)
			}
			return toolCallParts("c1", "agent", `{"description":"check it","prompt":"review","agent":"fork"}`)
		}}, nil
	}
	if err := h.AgentSend("hello"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	runs := h.tasks.runsOf(h.AgentSessionID())
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want one fork and no fork of it", len(runs))
	}
	rows, err := h.DB.Messages(runs[0].sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var said string
	for _, row := range rows {
		said += row.PartsJSON
	}
	if !strings.Contains(said, "already a fork") || !strings.Contains(said, "after the tool") {
		t.Fatalf("the fork read %q", said)
	}
	var reply string
	for _, m := range h.Session().Transcript {
		if m.Role == fantasy.MessageRoleAssistant && m.Kind == "" {
			reply = m.Text
		}
	}
	if !strings.Contains(reply, "after the tool") {
		t.Fatalf("reply = %q", reply)
	}
}

// TestForkRefusals: a fork continues the live conversation, so a run, an idle
// session, a fork the process forgot and a tool set that moved all answer no,
// in words the caller and the model can act on.
func TestForkRefusals(t *testing.T) {
	t.Run("a run cannot fork", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		run := func(agent string) error {
			ctx := context.WithValue(t.Context(), runKey{}, &agentRun{agent: agent})
			_, err := h.forkSetup(ctx, &agentRun{}, nil, "p")
			return err
		}
		if err := run(forkAgent); err == nil || !strings.Contains(err.Error(), "already a fork") {
			t.Fatalf("a fork asking for a fork = %v", err)
		}
		if err := run("explore"); err == nil || !strings.Contains(err.Error(), "is a run of it") {
			t.Fatalf("a subagent asking for a fork = %v", err)
		}
	})
	t.Run("a fork cannot be detached", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		h.SetUIHooks(&fakeUI{})
		tool := h.agentTool(h.PipelineFor())
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c1", Input: `{"prompt":"p","agent":"fork","background":true}`})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, "cannot be detached") {
			t.Fatalf("a detached fork = %+v %v", resp, err)
		}
	})
	t.Run("a detached run keeps its origin", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		def, err := h.agentDef(forkAgent)
		if err != nil {
			t.Fatal(err)
		}
		// the run a detached fork is asked from must survive the detach, or the
		// fork would continue the conversation from outside itself
		ctx := context.WithValue(t.Context(), runKey{}, &agentRun{agent: forkAgent})
		id, err := h.startBackgroundAgent(ctx, def, "d", "p")
		if err != nil {
			t.Fatal(err)
		}
		run := h.tasks.find(id)
		waitFor(t, func() error {
			if run.isRunning() {
				return errors.New("the detached run is still running")
			}
			return nil
		})
		tr := run.transcript()
		if last := tr[len(tr)-1]; !strings.Contains(last.Text, "already a fork") {
			t.Fatalf("the detached fork said %q", last.Text)
		}
	})
	t.Run("a fork needs the turn that is running", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		want := "continues the turn that is running"
		if _, err := h.forkSetup(t.Context(), &agentRun{}, nil, "p"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("a fork before anything was sent = %v", err)
		}
		h.modelFn = func() (fantasy.LanguageModel, error) {
			return &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
				return textParts("done")
			}}, nil
		}
		if err := h.AgentSend("hello"); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, h)
		// the runner outlives the turn, so only active says one is running
		if _, err := h.forkSetup(t.Context(), &agentRun{}, nil, "p"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("a fork after the turn ended = %v", err)
		}
	})
	t.Run("a forgotten fork cannot be continued", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		if err := h.DB.CreateSession("forgotten", "gone", "the task", forkAgent); err != nil {
			t.Fatal(err)
		}
		run, err := h.loadRun("forgotten")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.continueRun(t.Context(), run, "more", nil); err == nil || !strings.Contains(err.Error(), "no longer holds this fork's conversation") {
			t.Fatalf("continuing a forgotten fork = %v", err)
		}
	})
	t.Run("agents/fork.md is ignored", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		h.Dirs.ConfigHome = t.TempDir()
		dir := filepath.Join(h.Dirs.ConfigHome, "agents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "fork.md")
		if err := os.WriteFile(file, []byte("---\ndescription: mine\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
		def, err := h.agentDef(forkAgent)
		if err != nil || def.Path != "" {
			t.Fatalf("agents/fork.md shadowed the builtin: %+v %v", def, err)
		}
		n := 0
		for _, d := range h.agentDefs() {
			if d.Name == forkAgent {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("fork is listed %d times", n)
		}
	})
	t.Run("a tool the caller sent is gone", func(t *testing.T) {
		t.Parallel()
		if _, err := pickTools(nil, []fantasy.ToolInfo{{Name: "read"}}); err == nil || !strings.Contains(err.Error(), "no tool is named read") {
			t.Fatalf("a fork missing a caller tool = %v", err)
		}
	})
}

// TestAgentToolHandsOff: a foreground agent call hands its run off when the
// user's own message arrives, so the turn reads that message instead of waiting
// on the run: the call answers with the background text, the run stays live,
// and its report arrives as exactly one message.
func TestAgentToolHandsOff(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	gate := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, _ string, _ bool) []fantasy.StreamPart {
			if strings.Contains(system, "a subagent of zemi") {
				<-gate
				return textParts("looked around")
			}
			return textParts("ok") // the turn the report starts
		}}, nil
	}
	calls := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
			fantasy.ToolCall{ID: "c1", Input: `{"description":"look around","agent":"explore","prompt":"look"}`})
		calls <- resp
	}()
	var run *agentRun
	waitFor(t, func() error {
		runs := h.tasks.runsOf(h.AgentSessionID())
		if len(runs) != 1 {
			return fmt.Errorf("%d runs started, want one", len(runs))
		}
		if run = runs[0]; !run.isRunning() {
			return errors.New("the run ended before the call was answered")
		}
		return nil
	})
	h.UserSpoke()
	resp := waitCall(t, calls)
	if !strings.HasPrefix(resp.Content, bgAgentStarted) || toolStatus(out(resp.Content), false, tools.Outcome{Run: run.id}) != CallBG {
		t.Fatalf("handoff = %q (error %v), want the background text", resp.Content, resp.IsError)
	}
	if !run.isRunning() {
		t.Fatal("the run ended with the call that was handed off")
	}
	close(gate)
	waitFor(t, func() error {
		if run.isRunning() {
			return errors.New("the run never ended")
		}
		return nil
	})
	waitIdle(t, h)
	waitFor(t, func() error {
		if n := reportRows(h, run.id); n != 1 {
			return fmt.Errorf("%d report rows for %s, want exactly 1", n, run.id)
		}
		return nil
	})
}

// TestForegroundEscStopsTheRun: esc cancels the call's own context, and the run
// waiting on it must stop - the arm the agent tool shares with a workflow call.
func TestForegroundEscStopsTheRun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	runCtx, runCancel := context.WithCancel(t.Context())
	run := &agentRun{id: "r1", agent: "explore", status: RunRunning, cancel: runCancel}
	call, esc := context.WithCancel(t.Context())
	defer esc()
	answered := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := (&agentTool{h: h}).foreground(call, "c1", run, func(func(string)) (string, error) {
			<-runCtx.Done() // the run keeps going until the call stops it
			return "", runCtx.Err()
		})
		answered <- resp
	}()
	esc()
	resp := waitCall(t, answered)
	if !strings.Contains(resp.Content, "this agent was stopped") {
		t.Fatalf("esc answered %q, want the run stopped", resp.Content)
	}
}

// TestForegroundRunPanicAnswersTheCall: a panic inside a foreground run must not
// leave the call waiting for an answer that never comes.
// TestAgentToolStopsARun: the caller kills a running run with send_to and
// stop; a stopped run reports nothing, and stopping it again names no run.
func TestAgentToolStopsARun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, _ string, _ bool) []fantasy.StreamPart {
			if strings.Contains(system, "a subagent of zemi") {
				return nil // a provider that never answers: the run ends when it is stopped
			}
			return textParts("ok")
		}}, nil
	}
	calls := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
			fantasy.ToolCall{ID: "c1", Input: `{"description":"look around","agent":"explore","prompt":"look"}`})
		calls <- resp
	}()
	var run *agentRun
	waitFor(t, func() error {
		runs := h.tasks.runsOf(h.AgentSessionID())
		if len(runs) != 1 {
			return fmt.Errorf("%d runs started, want one", len(runs))
		}
		if run = runs[0]; !run.isRunning() {
			return errors.New("the run ended before the call was answered")
		}
		return nil
	})
	stop, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
		fantasy.ToolCall{ID: "c2", Input: fmt.Sprintf(`{"send_to":%q,"stop":true}`, run.id)})
	if stop.IsError || !strings.Contains(stop.Content, "Stopped agent") {
		t.Fatalf("stop = %q (error %v), want the stopped text", stop.Content, stop.IsError)
	}
	resp := waitCall(t, calls)
	if !resp.IsError || !strings.Contains(resp.Content, "this agent was stopped") {
		t.Fatalf("the run's call answered %q (error %v), want the stopped notice", resp.Content, resp.IsError)
	}
	waitFor(t, func() error {
		if status, _, _, _ := run.state(); status != RunCancelled {
			return fmt.Errorf("run status %q, want cancelled", status)
		}
		return nil
	})
	waitIdle(t, h)
	if n := reportRows(h, run.id); n != 0 {
		t.Fatalf("%d report rows for a stopped run, want 0", n)
	}
	again, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
		fantasy.ToolCall{ID: "c3", Input: fmt.Sprintf(`{"send_to":%q,"stop":true}`, run.id)})
	if !again.IsError || !strings.Contains(again.Content, "no running agent") {
		t.Fatalf("stopping it again = %q (error %v), want no running agent", again.Content, again.IsError)
	}
}

func TestForegroundRunPanicAnswersTheCall(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	resp, err := (&agentTool{h: h}).foreground(t.Context(), "c1", &agentRun{id: "r1", agent: "explore"}, func(func(string)) (string, error) {
		panic("boom")
	})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "boom") {
		t.Fatalf("a panicked run answered %q (error %v), want the panic as a tool error", resp.Content, err)
	}
}

// TestWaitValidation: wait stands alone - it refuses stop, send_to, a prompt,
// and a run of another session.
func TestWaitValidation(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	other := &agentRun{id: "rOther", agent: "explore", parent: "another-session", status: RunRunning, cancel: func() {}}
	if err := h.tasks.add(other); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		input string
		want  string
	}{
		{"wait beside stop", `{"wait":"rOther","stop":true}`, "set one of wait or stop, not both"},
		{"wait beside send_to", `{"wait":"rOther","send_to":"rOther"}`, "set one of wait or send_to, not both"},
		{"wait with a prompt", `{"wait":"all","prompt":"hello"}`, "wait takes no prompt"},
		{"another session's run", `{"wait":"rOther"}`, "belongs to another session"},
		{"no such run", `{"wait":"nope"}`, "no agent run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, err := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "w", Input: c.input})
			if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) {
				t.Fatalf("%s = %q (error %v), want %q", c.name, resp.Content, err, c.want)
			}
		})
	}
}

// finishedRows counts the report notices a run drew: its update notice names
// it too, so the count is of the "finished" row alone.
func finishedRows(h *Host, id string) int {
	n := 0
	for _, m := range h.Session().Transcript {
		if m.Kind == RowNotice && strings.Contains(m.Text, " "+id+" ") && strings.Contains(m.Text, "finished") {
			n++
		}
	}
	return n
}

// TestWaitWakesOnUpdate: a wait on a running run returns when the run sends an
// update, naming it, while the run goes on; the run's report still arrives as
// a message when it ends.
func TestWaitWakesOnUpdate(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	start := make(chan struct{})
	hold := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				if !toolResult {
					<-start // the child sends its update only once the wait holds
					return toolCallParts("u1", "update", `{"text":"found the flake"}`)
				}
				<-hold // and ends only once the test saw the wait wake
				return textParts("child done")
			case strings.Contains(last, "<agent-update"), strings.Contains(last, "<agent-report"):
				return textParts("noted")
			}
			return textParts("ok")
		}}, nil
	}
	def, _ := h.agentDef("explore")
	id, err := h.startBackgroundAgent(t.Context(), def, "dig", "look")
	if err != nil {
		t.Fatal(err)
	}
	run := h.tasks.find(id)
	answers := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
			fantasy.ToolCall{ID: "w", Input: `{"wait":"` + id + `"}`})
		answers <- resp
	}()
	waitFor(t, func() error {
		run.mu.Lock()
		defer run.mu.Unlock()
		if len(run.updates) == 0 {
			return errors.New("the wait never registered")
		}
		return nil
	})
	close(start)
	resp := waitCall(t, answers)
	if !strings.Contains(resp.Content, "sent an update") || !strings.Contains(resp.Content, id) {
		t.Fatalf("the wait answered %q, want the update named", resp.Content)
	}
	if !run.isRunning() {
		t.Fatal("the wait woke only at the run's end")
	}
	close(hold)
	waitFor(t, func() error {
		if run.isRunning() {
			return errors.New("the run never ended")
		}
		return nil
	})
	waitIdle(t, h)
	if n := finishedRows(h, id); n != 1 {
		t.Fatalf("%d report rows for %s, want 1", n, id)
	}
}

// TestForegroundWakesOnUpdate: a foreground run's call returns when the run
// sends an update, handing the run off like a message the user took; the
// caller then waits on the run and its report arrives as a message.
func TestForegroundWakesOnUpdate(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	sid := seedSession(t, h)
	start := make(chan struct{})
	hold := make(chan struct{})
	var run *agentRun
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				if !toolResult {
					<-start // the child sends its update only once the call waits
					return toolCallParts("u1", "update", `{"text":"need a decision"}`)
				}
				<-hold // and ends only once the test saw the call wake
				return textParts("child done")
			case strings.Contains(last, "<agent-update"):
				// the caller reads the update, then waits on the run it handed off
				return toolCallParts("w1", "agent", fmt.Sprintf(`{"wait":%q}`, run.id))
			case strings.Contains(last, "<agent-report"), strings.Contains(last, "its report arrives as a message"):
				return textParts("noted")
			}
			return toolCallParts("c1", "agent", `{"description":"dig","prompt":"look","agent":"general"}`)
		}}, nil
	}
	calls := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(),
			fantasy.ToolCall{ID: "c1", Input: `{"description":"dig","prompt":"look","agent":"general"}`})
		calls <- resp
	}()
	waitFor(t, func() error {
		runs := h.tasks.runsOf(sid)
		if len(runs) != 1 {
			return fmt.Errorf("%d runs started, want one", len(runs))
		}
		run = runs[0]
		run.mu.Lock()
		defer run.mu.Unlock()
		if len(run.updates) == 0 {
			return errors.New("the call never registered")
		}
		return nil
	})
	close(start)
	resp := waitCall(t, calls)
	if !strings.Contains(resp.Content, "sent an update") || !strings.Contains(resp.Content, "call agent with wait") {
		t.Fatalf("the call answered %q, want the update and the wait advice", resp.Content)
	}
	if !run.isRunning() || !run.handedOff() {
		t.Fatal("the run did not go on detached")
	}
	waitFor(t, func() error {
		run.mu.Lock()
		defer run.mu.Unlock()
		if len(run.updates) == 0 {
			return errors.New("the follow-up wait never registered")
		}
		return nil
	})
	close(hold)
	waitIdle(t, h)
	if n := finishedRows(h, run.id); n != 1 {
		t.Fatalf("%d report rows for %s, want 1", n, run.id)
	}
}
