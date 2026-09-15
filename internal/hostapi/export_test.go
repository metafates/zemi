package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// TestExportAndFork: a finished exchange exports as markdown, and a fork
// switches to a new session holding the same conversation.
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
	} {
		if got := toolSummary(in); got != want {
			t.Errorf("toolSummary(%s) = %q, want %q", in, got, want)
		}
	}
}
