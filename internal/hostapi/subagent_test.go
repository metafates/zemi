package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// scriptModel answers every stream call with the parts its script returns
// for that call (scripts must be safe for concurrent calls); Generate is
// unused by agent turns.
type scriptModel struct {
	script func(system, last string, toolResult bool) []fantasy.StreamPart
}

func (m *scriptModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
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
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
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

	var reply, detail string
	for _, m := range h.Session().Transcript {
		if m.Kind == "tool" && m.Tool == "agent" {
			detail = m.Detail
		}
		if m.Role == "assistant" && m.Kind == "" {
			reply = m.Text
		}
	}
	if reply != "parent saw: sub report for: look around" {
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
	runs := h.tasks.list(h.AgentSessionID())
	if len(runs) != 1 || h.AgentRunNamed("find thi") != runs[0].id {
		t.Fatalf("runs = %v", runs)
	}
	status, _, _, sid := runs[0].state()
	want := []ChatMessage{{Role: "user", Text: "look around"}, {Role: "assistant", Text: "sub report for: look around"}}
	if got := runs[0].transcript(); status != "done" || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("status %s, transcript %+v", status, got)
	}
	if err := h.ViewAgent(runs[0].id); err != nil || h.viewedRun() != runs[0] {
		t.Fatalf("view: %v", err)
	}
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
	text, err := h.runSubagent(context.Background(), def, "d", "look", nil)
	if err == nil {
		t.Fatal("want the step error")
	}
	if text != "partial finding" {
		t.Fatalf("partial report = %q", text)
	}
}

// TestSubagentInstructions: every run's first read of a file carries its
// package's instruction file, and none of them uses up the main
// conversation's one delivery.
func TestSubagentInstructions(t *testing.T) {
	h, _ := newTestHostDB(t)
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
			return toolCallParts("c1", "read", fmt.Sprintf(`{"path":%q}`, file))
		}}, nil
	}
	def, err := h.agentDef("explore")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if text, err := h.runSubagent(context.Background(), def, "d", "look", nil); err != nil || !strings.Contains(text, "use tabs here") {
			t.Fatalf("run %d: read result %q, %v", i, text, err)
		}
	}
	if got := h.nearestInstructions(file, h.instructionSent); !strings.Contains(got, "use tabs here") {
		t.Fatalf("the main conversation lost its delivery: %q", got)
	}
}

// TestSubagentReportTruncated: a runaway report is cut before it lands in
// the caller's context.
func TestSubagentReportTruncated(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			return textParts(strings.Repeat("x", 40_000))
		}}, nil
	}
	resp, err := h.agentTool().Run(context.Background(), fantasy.ToolCall{
		ID: "c1", Name: "agent", Input: `{"description":"d","prompt":"look","agent":"explore"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) > 31_000 || !strings.HasSuffix(resp.Content, "... (truncated)") {
		t.Fatalf("report is %d bytes and ends %q", len(resp.Content), truncRunes(resp.Content, 20))
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
	write(filepath.Join(cfg, "agents"), "reviewer.md", "---\ndescription: reviews diffs\ntools: read, grep\nmodel: inherit\n---\nBe harsh.\n")
	write(filepath.Join(cfg, "agents"), "explore.md", "---\ndescription: my explorer\ntools: [read]\n---\nmine\n")
	write(filepath.Join(cfg, "agents"), "broken.md", "---\ntools: read\n---\nno description\n")
	write(filepath.Join(proj, ".zemi", "agents"), "general.md", "---\ndescription: project override\n---\nx\n")
	write(filepath.Join(proj, ".zemi", "agents"), "tester.md", "---\ndescription: runs tests\n---\nx\n")

	log := logging.New(t.TempDir())
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
	h.SetProjectTrusted(true)
	defs = get()
	for _, c := range []struct{ name, source, desc, tools, system string }{
		{"reviewer", "user", "reviews diffs", "read,grep", "Be harsh."},
		{"explore", "user", "my explorer", "read", "mine"},
		{"general", "builtin", builtinAgents[1].Description, "", builtinAgents[1].System},
		{"tester", "project", "runs tests", "", "x"},
	} {
		d, ok := defs[c.name]
		if !ok {
			t.Fatalf("%s missing", c.name)
		}
		if d.Source != c.source || d.Description != c.desc || strings.Join(d.Tools, ",") != c.tools || d.System != c.system || d.Model != "" {
			t.Fatalf("%s = %+v", c.name, d)
		}
	}
	if _, ok := defs["broken"]; ok {
		t.Fatal("an agent without description must be skipped")
	}
}

// TestSubagentModelChoice: a subagent's model comes from the tool call,
// then its definition, then agent.small_model for a Small one, then
// agent.subagent_model, then the session; the
// agent tool lists the configured models and refuses one it does not know.
func TestSubagentModelChoice(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.provider.register({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1", api_key = "k",
		models = { { id = "main" }, { id = "mini" } },
	})`)
	for _, c := range []struct {
		def, setting, small string
		isSmall             bool
		want                string
	}{
		{"", "", "", false, ""},
		{"", "myllm/mini", "", false, "myllm/mini"},
		{"myllm/main", "myllm/mini", "", false, "myllm/main"},
		{"", "myllm/main", "myllm/mini", false, "myllm/main"},
		{"", "myllm/main", "myllm/mini", true, "myllm/mini"},
		{"", "myllm/main", "", true, "myllm/main"},
	} {
		h.Settings.SetQuiet("agent.subagent_model", c.setting)
		h.Settings.SetQuiet("agent.small_model", c.small)
		if got := h.subagentModelID(AgentDef{Model: c.def, Small: c.isSmall}); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
	tool := h.agentTool()
	if desc := tool.Info().Description; !strings.Contains(desc, "Configured models: myllm/main, myllm/mini") {
		t.Fatalf("description lacks the model list: %q", desc)
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "agent", Input: `{"description":"x","prompt":"p","agent":"explore","model":"other/x"}`})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, `unknown model "other/x"; configured models: myllm/main, myllm/mini`) {
		t.Fatalf("unknown model: %v %q", err, resp.Content)
	}
}
