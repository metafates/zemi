package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// scriptModel answers every turn's stream call with the parts its script
// returns for that call (scripts must be safe for concurrent calls); side
// calls fail.
type scriptModel struct {
	script func(system, last string, toolResult bool) []fantasy.StreamPart
}

func (m *scriptModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if sideCall(call) {
		return nil, errors.New("not implemented")
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
	if err != nil || fmt.Sprint(stored.transcript()) != fmt.Sprint(want) {
		t.Fatalf("stored run: %v %+v", err, stored)
	}
}

// TestSubagentFailureKeepsReport: a step failing after the agent already
// reported something returns that partial report with the error, instead of
// throwing the finished work away.
func TestSubagentFailureKeepsReport(t *testing.T) {
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
	_, text, err := h.runSubagent(context.Background(), def, "d", "look")
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
		check    func([]fantasy.Message) bool
	}{
		{
			"old tool output is cleared",
			[]fantasy.Message{agent.UserMessage("look"), toolResult("old", 30_000), toolResult("new", toolKeepTokens)},
			1,
			func(h []fantasy.Message) bool {
				return toolText(h[1].Content[0].(fantasy.ToolResultPart)) == clearedOutput
			},
		},
		{
			"nothing to clear drops the middle, not the task",
			[]fantasy.Message{agent.UserMessage("look"), big},
			1,
			func(h []fantasy.Message) bool {
				return concatText(h[0].Content) == "look" && estTokens(h...) < 1_000
			},
		},
		{"a second refusal ends the run", []fantasy.Message{agent.UserMessage("look"), big}, 9, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			err := h.sendPruning(context.Background(), runner, "p/m", "go")
			wantErr := c.refusals > 1
			if (err != nil) != wantErr || sends != 2 {
				t.Fatalf("send: %v (streams %d)", err, sends)
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
			h, r := newTestHostDB(t)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
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
			if _, _, err := h.runSubagent(context.Background(), def, "d", "look"); err != nil {
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
		if _, text, err := h.runSubagent(context.Background(), def, "d", "look"); err != nil || !strings.Contains(text, "use tabs here") {
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
		_, text, err := h.runSubagent(context.Background(), def, "d", "look")
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
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			return textParts(strings.Repeat("x", 40_000))
		}}, nil
	}
	// the schema words a wrong type as the builtin tools do
	for in, want := range map[string]string{`{"prompt":1}`: `field "prompt" must be string`, `[1]`: "input must be a JSON object"} {
		if resp, _ := h.agentTool(h.PipelineFor()).Run(context.Background(), fantasy.ToolCall{ID: "c0", Name: "agent", Input: in}); !resp.IsError || resp.Content != want {
			t.Fatalf("%s: %+v", in, resp)
		}
	}
	resp, err := h.agentTool(h.PipelineFor()).Run(context.Background(), fantasy.ToolCall{
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

	if builtinAgents[1].Name != "general" {
		t.Fatal("builtinAgents order changed; fix the general row below")
	}
	defs := get()
	if _, ok := defs["tester"]; ok {
		t.Fatal("project agents must wait for trust")
	}
	if _, err := h.agentDef("tester"); err == nil || !strings.Contains(err.Error(), "runs zemi run --trust") || !strings.Contains(err.Error(), ".agents") {
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
		{"general", "builtin", builtinAgents[1].Description, "", builtinAgents[1].System, false},
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
// models.chat. A reference config does not know is refused with the roles
// and the configured models.
func TestSubagentModelChoice(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.provider.add({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1", api_key = "k",
		models = { { id = "main" }, { id = "mini" } },
	})`)
	for _, c := range []struct {
		def, chat, agents string
		want              string
	}{
		{"", "", "", "myllm/main"}, // nothing set: the provider's default
		{"", "myllm/mini", "", "myllm/mini"},
		{"", "myllm/main", "myllm/mini", "myllm/mini"},
		{"myllm/main", "myllm/mini", "myllm/mini", "myllm/main"},
		{"chat", "myllm/main", "myllm/mini", "myllm/main"},
	} {
		h.Settings.SetQuiet("models.chat", c.chat)
		h.Settings.SetQuiet("models.agents", c.agents)
		if got := h.subagentModelID(AgentDef{Model: c.def}); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
	// the catalog can be thousands of tokens; a bad id names it instead
	if desc := h.agentTool(nil).Info().Description; strings.Contains(desc, "myllm/main") {
		t.Fatalf("description lists models: %q", desc)
	}
	if err := h.checkModelID("other/x"); err == nil || err.Error() != `unknown model "other/x"; roles: chat, agents, titles, compact, memory; configured models: myllm/main, myllm/mini` {
		t.Fatalf("unknown model: %v", err)
	}
}

// TestSubagentMemoryKeepsItsSession: a memory a run writes after the user
// moved to another session is judged by the run's own session, which read
// outside text, so auto autonomy still queues it.
func TestSubagentMemoryKeepsItsSession(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetQuiet("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	h.recordToolCall(sid, StreamTool{ID: "c1", Name: "fetch"}, true)
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
	props := h.memoryProposals()
	if len(props) != 1 || props[0].ProvenanceSessionID != sid || len(projectMemories(t, h)) != 0 {
		t.Fatalf("want one proposal from %s, nothing stored: %+v", sid, props)
	}
}

// TestAgentsList: /agents leads with New agent, shows a model's background
// job with its output and stops it through the [s] Stop action's command,
// and details each agent, the user's first, opening the ones that are files.
func TestAgentsList(t *testing.T) {
	h, r := newTestHost(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	id, err := h.jobs.Start(t.TempDir(), "echo up; sleep 30", nil, nil, func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	j, _ := h.jobs.Get(id)
	for deadline := time.Now().Add(3 * time.Second); j.Output() == "" && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
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
		m := it.(map[string]any)
		rows[fmt.Sprint(m["text"])] = fmt.Sprint(m)
	}
	if first := fmt.Sprint(ui.opened[0].Items[0]); !strings.Contains(first, "fill:/agents new ") || !strings.Contains(first, "text:New agent") {
		t.Errorf("first row = %s", first)
	}
	if s := ui.opened[0].Items[2].(map[string]any)["section"]; s != "User agents" { // under the job
		t.Errorf("first agent section %v, want User agents", s)
	}
	for _, c := range []struct {
		row  string
		want []string
		not  []string
	}{
		{id, []string{"up", "hotkey:s run:agents kill " + id}, nil},
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
	if err := agents.GoFn(h, "kill "+id); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); j.Running() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if j.Running() {
		t.Fatal("job still running after kill")
	}
}

// TestUpdateToolReportsAFailedSend: a child whose line never reached the
// caller is told so, instead of reading "Sent to the caller." forever.
func TestUpdateToolReportsAFailedSend(t *testing.T) {
	h, _ := newTestHostDB(t) // no model: the parent's send refuses
	h.SetUIHooks(&fakeUI{})
	run, _ := h.newRun(context.Background(), AgentDef{Name: "general"}, "d", "look", true)
	tool := &updateTool{h: h, pipe: h.PipelineFor()}
	ctx := context.WithValue(context.Background(), runKey{}, run)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tools.ToolUpdate, Input: `{"text":"found it"}`})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "did not reach the caller") {
		t.Fatalf("a failed update = %+v, %v", resp, err)
	}
}

// TestChildToolsAcceptsUpdate: naming the update tool in an agent file is
// accepted and ignored, like the tools a subagent never gets, because every
// run holds it whatever the file's allowlist says.
func TestChildToolsAcceptsUpdate(t *testing.T) {
	h, _ := newTestHostDB(t)
	for _, want := range [][]string{{"read", "update"}, {"read", "-update"}, {"-update"}} {
		kept, unknown := childTools(h, h.PipelineFor(), want)
		if len(unknown) > 0 {
			t.Errorf("%v: unknown = %v, want none", want, unknown)
		}
		if !hasTool(kept, tools.ToolUpdate) {
			t.Errorf("%v: the run has no %s tool", want, tools.ToolUpdate)
		}
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
	h, r := newTestHostDB(t)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
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
