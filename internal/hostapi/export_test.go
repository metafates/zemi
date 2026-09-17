package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// TestExportAndFork: a finished exchange exports as markdown, /copy takes
// a reply with no code blocks at once, and a fork switches to a new session holding the same conversation.
func TestExportAndFork(t *testing.T) {
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "pong")
	h.reflectMu.Lock() // no detached reflector racing the session switch
	defer h.reflectMu.Unlock()
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	path, err := h.ExportSession(filepath.Join(t.TempDir(), "s.md"))
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "## You\n\nping") || !strings.Contains(string(md), "## zemi\n\npong") {
		t.Fatalf("export:\n%s", md)
	}

	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.CopyReply(""); err != nil || ui.copied != "pong" {
		t.Fatalf("copy = %q, %v", ui.copied, err)
	}

	orig := h.AgentSessionID()
	id, err := h.ForkSession()
	if err != nil {
		t.Fatal(err)
	}
	if id == orig || h.AgentSessionID() != id {
		t.Fatalf("fork %q, live %q, original %q", id, h.AgentSessionID(), orig)
	}
	a, _ := h.DB.Messages(orig)
	b, _ := h.DB.Messages(id)
	if len(a) == 0 || len(a) != len(b) || len(h.Session().Transcript) != 2 {
		t.Fatalf("fork copied %d of %d messages, transcript %d rows", len(b), len(a), len(h.Session().Transcript))
	}
}

// TestCopyReply: the direct forms copy a block, every block or the reply,
// bad ones fail, and a bare /copy opens the picker.
func TestCopyReply(t *testing.T) {
	h, _ := newTestHostDB(t)
	reply := "Two:\n\n```go\nx := 1\n```\n\n~~~\ny\n~~~"
	reflectStub(t, h, reply)
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
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
	for _, args := range []string{"code 3", "code x", "code 1 2", "nope"} {
		if h.CopyReply(args) == nil {
			t.Errorf("/copy %s should fail", args)
		}
	}
	if err := h.CopyReply(""); err != nil || len(ui.opened) != 1 {
		t.Fatalf("picker: %v, %d opened", err, len(ui.opened))
	}
	var runs []string
	for _, it := range ui.opened[0].Items {
		runs = append(runs, it.(map[string]any)["run"].(string))
	}
	if strings.Join(runs, ",") != "copy code 1,copy code 2,copy code,copy reply" || ui.opened[0].Items[1].(map[string]any)["hotkey"] != "2" {
		t.Fatalf("picker rows: %v", ui.opened[0].Items)
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
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.ToolCallStart("e1", "edit", `{"path":"a.go","old_string":"x","new_string":"y"}`)
	sa.ToolCallEnd("e1", "edit", true, "")
	sa.ToolCallStart("e2", "edit", `{"path":"b.go","old_string":"p","new_string":"q"}`)
	sa.ToolCallEnd("e2", "edit", false, "old_string not found")
	sa.TurnDone(nil, fantasy.Usage{})
	var diffs []string
	for _, m := range h.Session().Transcript {
		if m.Kind == "tool" {
			diffs = append(diffs, m.Diff)
		}
	}
	if len(diffs) != 2 || diffs[0] != "- x\n+ y" || diffs[1] != "" {
		t.Fatalf("diffs = %q", diffs)
	}

	reflectStub(t, h, "ok")
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
		`{"command":"go test ./...","description":"Run the  unit tests"}`: "Run the unit tests",
		`{"command":"ls"}`: "",
		`{"description":"Find the config loader","prompt":"..."}`: "",
	} {
		if got := toolLabel(in); got != want {
			t.Errorf("toolLabel(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestToolSummary(t *testing.T) {
	for in, want := range map[string]string{
		`{"todos":[{"content":"a","status":"done"},{"content":"b","status":"pending"}]}`: "2 todos, 1 done",
		`{"command":"python3","args":["-m","http.server","8765"]}`:                       "python3 -m http.server 8765",
		`{"id":"job-1"}`: "job-1",
		`{"questions":[{"question":"Which color?","options":[]},{"question":"Size?"}]}`: "Which color? +1 more",
		`{"plan":"\n# Goal\n\n1. write x.txt"}`:                                         "Goal",
	} {
		if got := toolSummary(in); got != want {
			t.Errorf("toolSummary(%s) = %q, want %q", in, got, want)
		}
	}
}
