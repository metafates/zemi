package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestExportAndCopy: a finished exchange exports the log as markdown, /copy
// takes a reply with no code blocks at once.
func TestExportAndCopy(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "pong")
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	path, err := h.ExportSession(filepath.Join(t.TempDir(), "s.md"), false)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "## You\n\nping") || !strings.Contains(string(md), "## zemi\n\npong") {
		t.Fatalf("export:\n%s", md)
	}
	if _, err := h.ExportSession(path, true); err != nil {
		t.Fatal(err)
	}
	if md, _ = os.ReadFile(path); !strings.Contains(string(md), "## You\n\nping") {
		t.Fatalf("full export:\n%s", md)
	}
	// the toast names the file as the user finds it, short enough to read whole
	if _, err := h.ExportSession("", false); err != nil {
		t.Fatal(err)
	}
	if n := h.evlog.notes[len(h.evlog.notes)-1]; n.body != "saved to zemi-chat.md" {
		t.Fatalf("export toast: %q", n.body)
	}

	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.CopyReply(""); err != nil || ui.copied != "pong" {
		t.Fatalf("copy = %q, %v", ui.copied, err)
	}
}

// TestCopyReply: the direct forms copy a block, every block or the reply,
// bad ones fail, and a bare /copy over a reply with code blocks opens their
// picker, where one block can be picked.
func TestCopyReply(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reply := "Two:\n\n```go\nx := 1\n```\n\n~~~\ny\n~~~"
	replyStub(t, h, reply)
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	for args, want := range map[string]string{"code 2": "y\n", "code": "x := 1\n\ny\n", "reply": reply} {
		if err := h.CopyReply(args); err != nil || ui.copied != want {
			t.Errorf("/copy %s = %q, %v", args, ui.copied, err)
		}
	}
	for _, args := range []string{"code 3", "code x", "code 1 2", "nope", "list"} {
		if h.CopyReply(args) == nil {
			t.Errorf("/copy %s should fail", args)
		}
	}
	ui.opened = nil
	if err := h.CopyReply(""); err != nil || len(ui.opened) != 1 || ui.opened[0].ID != "copy" || !ui.opened[0].CloseOnCopy {
		t.Fatalf("/copy: %v, %v", err, ui.opened)
	}
	type copyRow struct{ text, desc, copy string }
	var rows []copyRow
	for _, it := range ui.opened[0].Items {
		rows = append(rows, copyRow{it.Text, it.Desc, it.Copy})
	}
	wantRows := []copyRow{
		{"1  go", "1 line · x := 1", "x := 1\n"},
		{"2  text", "1 line · y", "y\n"},
		{"every block", "2 code blocks", "x := 1\n\ny\n"},
		{"the whole reply", "9 lines", reply},
	}
	if !slices.Equal(rows, wantRows) {
		t.Fatalf("copy picker rows = %+v, want %+v", rows, wantRows)
	}
}

// TestCopyRunReply: with a run's transcript in view, /copy takes the run's
// last reply, not the chat's.
func TestCopyRunReply(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "pong")
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	run := "a1b2c3d4"
	h.DB.CreateSession(run, store.MainSessionID, "look around", "explore")
	addText(t, h, run, "user", "find the widget")
	addText(t, h, run, "assistant", "it is in the corner")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.ViewAgent(run); err != nil {
		t.Fatal(err)
	}
	if err := h.CopyReply("reply"); err != nil || ui.copied != "it is in the corner" {
		t.Fatalf("run copy = %q, %v", ui.copied, err)
	}
	if err := h.ViewAgent(""); err != nil {
		t.Fatal(err)
	}
	if err := h.CopyReply("reply"); err != nil || ui.copied != "pong" {
		t.Fatalf("chat copy = %q, %v", ui.copied, err)
	}
}

// TestExportLogMarkdown renders log rows as markdown, one header per run of
// one speaker, tool calls as list rows or expanded in full.
func TestExportLogMarkdown(t *testing.T) {
	rows := []store.LogEntry{
		{Kind: store.KindUser, Text: "hi"},
		{Kind: store.KindTool, Text: `{"command":"go test ./..."}`, MetaJSON: `{"tool":"shell"}`},
		{Kind: store.KindAgent, Text: "done"},
		{Kind: store.KindNote, Text: "chapter"},
	}
	var b strings.Builder
	exportLog(&b, rows, 2, false)
	want := "\n## You\n\nhi\n\n## zemi\n- `shell` go test ./...\n\ndone\n\n> _note: chapter_\n"
	if b.String() != want {
		t.Errorf("export:\n%q\nwant\n%q", b.String(), want)
	}
	var full strings.Builder
	exportLog(&full, rows[:3], 2, true)
	if !strings.Contains(full.String(), "**shell**") || !strings.Contains(full.String(), "```") {
		t.Errorf("full export lost the tool body: %q", full.String())
	}
}

func TestCodeBlocks(t *testing.T) {
	for _, c := range []struct {
		name, md string
		want     []codeBlock
	}{
		{"none", "just `code` and text", nil},
		{"backticks with info", "a\n``` go\ttitle=x\nfunc f() {}\n\n```\nb", []codeBlock{{"go", "func f() {}\n\n"}}},
		{"tildes hold backticks", "~~~\n```\n~~~", []codeBlock{{"", "```\n"}}},
		{"longer fence holds a short one", "````md\n```go\nx\n```\n````", []codeBlock{{"md", "```go\nx\n```\n"}}},
		{"close needs its own length", "````\nx\n```\n````", []codeBlock{{"", "x\n```\n"}}},
		{"close takes no info", "```\n```go\n```", []codeBlock{{"", "```go\n"}}},
		{"indented in a list", "1. step\n   ```sh\n   make\n     -j\n  x\n   ```", []codeBlock{{"sh", "make\n  -j\nx\n"}}},
		{"unclosed runs to the end", "```py\nprint()\n", []codeBlock{{"py", "print()\n"}}},
		{"empty", "```\n```", []codeBlock{{"", ""}}},
		{"inline triple backticks", "use ```x``` here", nil},
		{"two blocks", "```a\n1\n```\ntext\n~~~b\n2\n~~~", []codeBlock{{"a", "1\n"}, {"b", "2\n"}}},
		{"crlf", "```\r\nx\r\n```\r\n", []codeBlock{{"", "x\r\n"}}},
	} {
		if got := codeBlocks(c.md); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTurnExtras: a successful edit keeps its diff under the tool row, a
// failed one does not, and todo writes reach the widget snapshot.
func TestTurnExtras(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.ToolCallStart("e1", "edit", `{"path":"a.go","old_string":"x","new_string":"y"}`)
	sa.ToolCallEnd("e1", "edit", out(""), "")
	sa.ToolCallStart("e2", "edit", `{"path":"b.go","old_string":"p","new_string":"q"}`)
	sa.ToolCallEnd("e2", "edit", outErr("old_string not found"), "")
	sa.TurnDone(nil, fantasy.Usage{})
	var diffs []string
	for _, m := range h.Session().Transcript {
		if m.Kind == "tool" {
			diffs = append(diffs, m.Diff)
		}
	}
	if len(diffs) != 2 || diffs[0] != "--- a.go\n+++ a.go\n@@\n-x\n+y" || diffs[1] != "" {
		t.Fatalf("diffs = %q", diffs)
	}

	replyStub(t, h, "ok")
	if err := h.AgentSend("hi"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if err := h.PipelineFor().TodosWrite([]store.Todo{{Content: "write tests", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	if todos := h.todoSnapshot(); len(todos) != 1 || todos[0].Content != "write tests" {
		t.Fatalf("todos = %+v", todos)
	}
}

func TestToolLabel(t *testing.T) {
	for in, want := range map[string]string{
		`{"about":"Run the  tests","command":"go test ./..."}`: "Run the tests",
		`{"command":"ls"}`:                                        "",
		`{"about":"Look up the ticket","id":"FE-1"}`:              "Look up the ticket",
		`{"description":"Find the config loader","prompt":"..."}`: "",
		`{"questions":[{"question":"Which one?","header":"Auth method"},{"question":"How big?"},{"question":"Keep?","header":"API key"}]}`: "auth method, API key",
		`{"questions":[{"question":"Which one?"},{"question":"How big?"}]}`:                                                                "2 questions",
		`{"about":"` + strings.Repeat("word ", 20) + `"}`:                                                                                  strings.TrimSpace(strings.Repeat("word ", 16)) + "…",
	} {
		if got := toolLabel(in); got != want {
			t.Errorf("toolLabel(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestPendingRowLabel(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	s := &streamAdapter{h: h}
	s.ToolCallPending("1", "shell")
	label := func() string {
		h.agents.mu.Lock()
		defer h.agents.mu.Unlock()
		return h.agents.tools[0].Label
	}
	for _, c := range []struct{ delta, want string }{
		{`{"about": "Say \"hi`, ""},
		{`\"  twice", "comm`, `Say "hi" twice`},
		{`and": "echo hi; echo hi"}`, `Say "hi" twice`},
	} {
		if s.ToolInputDelta("1", c.delta); label() != c.want {
			t.Fatalf("after %q: label %q, want %q", c.delta, label(), c.want)
		}
	}
}

func TestToolSummary(t *testing.T) {
	for _, c := range []struct{ tool, in, want string }{
		{"", `{"items":[{"content":"a","status":"done"},{"content":"b","status":"pending"}]}`, "2 todos, 1 done"},
		{"", `{"command":"python3","args":["-m","http.server","8765"]}`, "python3 -m http.server 8765"},
		{"", `{"id":"job-1"}`, "job-1"},
		{"", `{"questions":[{"question":"Which color?","options":[]},{"question":"Size?"}]}`, "Which color? +1 more"},
		{"", `{"plan":"\n# Goal\n\n1. write x.txt"}`, "Goal"},
		{"", `{"plan":"# Plan\n1. x"}`, ""},
		{"", `{"plan":"# Plan: add caching\n1. x"}`, "add caching"},
		{"", `{"plan":"# Planning notes"}`, "Planning notes"},
		{"", `{"plan":"# Plan: ȺȺȺȺȺ İş"}`, "ȺȺȺȺȺ İş"},
		{"", `{"action":"text"}`, "text"},
		{"", `{"action":"close","id":2}`, "close 2"},
		{"", `{"action":"definition","path":"/p/a.go","symbol":"Run"}`, "definition Run"},
		{"", `{"agent":"explore","prompt":"look\n around"}`, "explore: look around"},
		{"", `{"agent":"explore","description":"Look around","prompt":"x"}`, "Look around"},
		{"", `{"wait":"all"}`, "all"},
		{"", `{}`, ""},
		{"", `{"limit":5,"filter":{"a":"b"},"q":"open","z":"x"}`, "open"},
		{"", `{"limit":5}`, `{"limit":5}`},
		{"", `{"send_to":"dc11086e","prompt":"second question"}`, "to dc11086e: second question"},
		{string(tools.ToolWorkflow), `{"script":"local wf = zemi.workflow\nreturn 1"}`, "workflow"},
		{string(tools.ToolWorkflow), `{"script":"-- scan the tree\nreturn 1"}`, "scan the tree"},
		{string(tools.ToolWorkflow), `{"run":"abc123","action":"rerun","description":"slow scan again"}`, "slow scan again"},
		{string(tools.ToolWorkflow), `{"run":"abc123","action":"stop"}`, "stop abc123"},
		{string(tools.ToolWorkflow), `{"run":"abc123","action":"wait"}`, "wait abc123"},
		{string(tools.ToolWorkflow), `{"run":"abc123"}`, "status abc123"},
		{string(tools.ToolLua), `{"about":"Check the plugin","script":"return 1"}`, ""},
		{string(tools.ToolLua), `{"about":"Run the check","path":"x.lua"}`, "x.lua"},
		{string(tools.ToolZoom), `{"id":16032}`, "16032"},
		{string(tools.ToolZoom), `{"id":16032,"n":8}`, "16032+8"},
		{string(tools.ToolZoom), `{"id":16032,"n":1,"page":1}`, "16032 page 2"},
		{string(tools.ToolZoom), `{"id":16032,"page":1}`, "16032 page 2"},
		{string(tools.ToolZoom), `{"id":16032,"n":2,"page":1}`, "16032+2"},
		{string(tools.ToolZoom), `{"id":1603200}`, "1603200"},
		{string(tools.ToolZoom), `{"id":"16032"}`, "16032"},
		{string(tools.ToolDate), `{"id":16032}`, "16032"},
	} {
		if got := toolSummary(c.tool, c.in); got != c.want {
			t.Errorf("toolSummary(%s, %s) = %q, want %q", c.tool, c.in, got, c.want)
		}
	}
}

// TestCost: a cost under half a cent reads as one, not as free.
func TestCost(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		usd  float64
		want string
	}{{0.00375, "<$0.01"}, {0.005, "$0.01"}, {0.42, "$0.42"}} {
		if got := fmtCost(c.usd); got != c.want {
			t.Errorf("fmtCost(%v) = %q, want %q", c.usd, got, c.want)
		}
	}
}

// TestContextPct: /cost and the statusline read one percent, and 79.5 is
// the same 79 in both (a threshold reads the number, not a rounded copy).
func TestContextPct(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, window int64
		want       float64
	}{{0, 0, 0}, {500, 0, 0}, {1000, 10000, 10}, {795, 1000, 79.5}} {
		if got := contextPct(c.in, c.window); got != c.want {
			t.Errorf("contextPct(%d, %d) = %v, want %v", c.in, c.window, got, c.want)
		}
	}
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.SeedStream(StreamSnapshot{LastIn: 795, CtxWindow: 1000})
	if err := h.OpenUsage(); err != nil {
		t.Fatal(err)
	}
	if body := ui.opened[0].Body; !strings.Contains(body, "· 79%") {
		t.Fatalf("/cost reads another percent than the statusline:\n%s", body)
	}
}

// TestTokens: a token count rides k, M then B, a round unit losing its ".0".
func TestTokens(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{999, "999"},
		{1000, "1k"},
		{1234, "1.2k"},
		{1000000, "1M"},
		{1234000, "1.2M"},
		{1000000000, "1B"},
		{1234567890, "1.2B"},
		{999000000000, "999B"},
	} {
		if got := fmtTokens(c.n); got != c.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestUsageBreaksThePromptDown: the fixed prompt rides every request, so
// /usage says what it is made of instead of one aggregate context line.
func TestUsageBreaksThePromptDown(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	toolset := h.mainTools()
	if _, err := h.ensureRunner(toolset); err != nil {
		t.Fatal(err)
	}
	if err := h.OpenUsage(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("usage: %v %+v", err, ui.opened)
	}
	body := ui.opened[0].Body
	for _, want := range []string{"Prompt", "system ", "tools "} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %q in the usage body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "cached") {
		t.Fatalf("no turn ran, so nothing is cached:\n%s", body)
	}
	h.SeedStream(StreamSnapshot{SessionCost: 0.27}) // a session with a bill draws the cost column
	h.agents.cacheRead, h.agents.cacheBilled = 9989, 10000
	if err := h.OpenUsage(); err != nil {
		t.Fatal(err)
	}
	// the share trails the priced column head, so the columns stay over
	// their values whatever the note adds to the title's row
	body = ui.opened[1].Body
	if !strings.Contains(body, "Tokens                in      out      cost  cached 99.9%\n  this session         0        0     $0.27") {
		t.Fatalf("the cache note shifted the columns:\n%s", body)
	}
}

// TestPromptCommandShowsTheText: /prompt shows the assembled system
// prompt, and says so before anything assembled one.
func TestPromptCommandShowsTheText(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.OpenPrompt(); err != nil || len(ui.opened) != 0 || h.evlog.notes[len(h.evlog.notes)-1].title != "Nothing to show yet" {
		t.Fatalf("before the first send: %v %+v", err, ui.opened)
	}
	toolset := h.mainTools()
	if _, err := h.ensureRunner(toolset); err != nil {
		t.Fatal(err)
	}
	if err := h.OpenPrompt(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("prompt: %v %+v", err, ui.opened)
	}
	if body := ui.opened[0].Body; body != h.SystemPrompt() || !strings.Contains(body, "# Environment") {
		t.Fatalf("overlay body is not the prompt:\n%s", truncRunes(body, 200))
	}
}
