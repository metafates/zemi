package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestFailPrintJSON: a failure before the turn still reaches a script on
// stdout as the report it parses.
func TestFailPrintJSON(t *testing.T) {
	var out, errOut strings.Builder
	if code := failPrint(errors.New("no model configured"), true, &out, &errOut); code != 1 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	var rep report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || !rep.IsError || rep.Error != "no model configured" {
		t.Fatalf("stdout = %q (%v)", out.String(), err)
	}
	out.Reset()
	if code := failPrint(errors.New("boom"), false, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "boom") {
		t.Fatalf("text mode: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestTurnReport(t *testing.T) {
	rep := turnReport([]hostapi.ChatMessage{
		{Role: "user", Text: "go"},
		{Role: "assistant", Text: "looking"},
		{Role: "assistant", Kind: "tool", Tool: "agent", Text: "find it", Status: "ok", Detail: "explore · 3 tool calls"},
		{Role: "assistant", Kind: "tool", Tool: "bash", Text: "rm -rf /", Status: "err", Output: "rm: /: is a directory\nexit status 1\nchanged: x"},
		{
			Role: "assistant", Kind: "tool", Tool: "write", Text: "x.go", Status: "err",
			Output: "permission denied (mode): fs.write /x.go: print mode cannot ask",
		},
		{
			Role: "assistant", Kind: "tool", Tool: tools.ToolLua, Text: "zemi.mcp.add{}", Status: "err",
			Output: "permission denied (mode): tool.lua: print mode cannot ask",
		},
		{
			Role: "assistant", Kind: "tool", Tool: tools.ToolMemory, Text: "forget x", Status: "err",
			Output: "permission denied (nobody): memory.write: no one can approve in a headless run",
		},
		{Role: "assistant", Kind: "compact", Text: "cleared 40k tokens of old tool output"},
		{Role: "assistant", Text: "found"},
	})
	if rep.Result != "looking\n\nfound" || rep.IsError || rep.Compactions != 1 || len(rep.Tools) != 5 || !rep.Tools[0].OK || rep.Tools[1].OK || rep.Tools[0].Detail == "" {
		t.Fatalf("report = %+v", rep)
	}
	// a failed call says why and whether the gate refused it
	if rt := rep.Tools[1]; rt.Error != "exit status 1" || rt.Denied || rep.Tools[0].Error != "" {
		t.Fatalf("bash row = %+v", rt)
	}
	if rt := rep.Tools[2]; !rt.Denied || !strings.HasPrefix(rt.Error, "permission denied (mode)") {
		t.Fatalf("write row = %+v", rt)
	}
	// an unanswered ask is the gate's to count; the rest were refused
	if rep.refused != 2 {
		t.Fatalf("refused = %d, want 2", rep.refused)
	}
	if rep := turnReport([]hostapi.ChatMessage{{Role: "assistant", Kind: "error", Text: "rate limited"}}); !rep.IsError || rep.Error != "rate limited" {
		t.Fatalf("error report = %+v", rep)
	}
}

func TestDeniedHint(t *testing.T) {
	for _, c := range []struct {
		name        string
		unsandboxed int
		trusted     bool
		want        string
	}{
		{"untrusted", 0, false, "try --trust"},
		{"trusted", 0, true, "the mode left it to the user"},
		{"outside the sandbox", 1, true, "outside the sandbox needs the TUI"},
		{"lua script", 0, true, "only the TUI can approve a Lua script"},
	} {
		rep := report{Denied: 1, unsandboxed: c.unsandboxed}
		if c.name == "lua script" {
			rep.tuiOnly = 1
		}
		if got := deniedHint(rep, c.trusted); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// fakeOpenAI is an OpenAI-compatible chat server: "call NAME {json}" calls
// that tool, "say X" replies X, a message ending in "hang" waits for the
// request to end (10s at most, so a cancel that never comes fails a test
// instead of hanging it), and a tool result gets "done".
func fakeOpenAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream   bool
		Messages []struct {
			Role    string
			Content any
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	last := body.Messages[len(body.Messages)-1]
	text, _ := last.Content.(string)
	if parts, ok := last.Content.([]any); ok {
		for _, p := range parts {
			m, _ := p.(map[string]any)
			s, _ := m["text"].(string)
			text += s
		}
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
	if !body.Stream {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(delta map[string]any, fin any) {
		b, _ := json.Marshal(map[string]any{
			"id": "c", "object": "chat.completion.chunk", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
	}
	call := regexp.MustCompile(`call (\w+) (\{.*\})`).FindStringSubmatch(text)
	_, say, _ := strings.Cut(text, "say ")
	switch {
	case last.Role == "user" && call != nil:
		ev(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": "t1", "type": "function",
			"function": map[string]any{"name": call[1], "arguments": call[2]},
		}}}, nil)
		ev(map[string]any{}, "tool_calls")
	case last.Role == "user" && strings.HasSuffix(text, "hang"):
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
			http.Error(w, "hung", http.StatusBadRequest) // not retried
		}
		return
	default:
		ev(map[string]any{"role": "assistant", "content": cmp.Or(say, "done")}, nil)
		ev(map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// printHost is a host with /new and a fake model, in an untrusted project.
func printHost(t *testing.T) *hostapi.Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fakeOpenAI))
	t.Cleanup(srv.Close)
	_, h, r := newTrustGate(t)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`zemi.provider.add({ id = "fake", kind = zemi.provider.kind.OPENAI_COMPAT, api_key = "x", base_url = %q, models = { { id = "m", context_window = 128000 } } })
		zemi.config.set("models.chat", "fake/m")`, srv.URL+"/v1")
	if err := r.ExecuteChunk(context.Background(), cfg, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestPrintTurn: what zemi run prints for a turn, and why calls were denied.
func TestPrintTurn(t *testing.T) {
	for _, c := range []struct {
		name, prompt      string
		first             []string // turns run before, -c style
		setup             func(h *hostapi.Host)
		stdout, stderr    string // wanted substrings
		notStdout, notErr string
		denied            int
	}{
		{name: "escapes are stripped", prompt: "say \x1b]52;c;SGk=\a\x1b[31mhi", stdout: "hi", notStdout: "\x1b"},
		{name: "a deny rule is no ask", prompt: `call bash {"command":"ls -la"}`, setup: func(h *hostapi.Host) {
			h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "ls*", Decision: perm.Deny})
		}, stderr: "1 tool call denied by a rule", notErr: "allow rule"},
		{name: "the planner does not write", prompt: `call write {"path":"x.txt","content":"x"}`, setup: func(h *hostapi.Host) {
			if err := h.SetProfile("planner"); err != nil {
				t.Fatal(err)
			}
		}, stderr: "1 tool call denied by a rule", notErr: "allow rule"},
		{
			name: "a subagent's unanswered ask counts", prompt: `call agent {"agent":"general","description":"d","prompt":"call write {\"path\":\"zz.txt\",\"content\":\"x\"}"}`,
			stderr: "1 tool call denied; nobody can approve a call in zemi run", denied: 1,
		},
		{name: "a swapped-in session is not this run's", first: []string{"say hi", "/new"}, prompt: "/resume", notStdout: "hi"},
	} {
		h := printHost(t)
		if c.setup != nil {
			c.setup(h)
		}
		for _, p := range c.first {
			if printTurn(h, p, false, io.Discard, io.Discard) != 0 {
				t.Fatalf("%s: %q failed", c.name, p)
			}
		}
		var out, errOut strings.Builder
		code := printTurn(h, c.prompt, false, &out, &errOut)
		if code != 0 || !strings.Contains(out.String(), c.stdout) || !strings.Contains(errOut.String(), c.stderr) ||
			c.notStdout != "" && strings.Contains(out.String(), c.notStdout) || c.notErr != "" && strings.Contains(errOut.String(), c.notErr) {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out.String(), errOut.String())
		}
		out.Reset()
		if c.denied > 0 {
			var rep report
			printTurn(h, c.prompt, true, &out, io.Discard)
			if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || rep.Denied != c.denied {
				t.Errorf("%s: json denied = %d (%v)", c.name, rep.Denied, err)
			}
		}
	}
}

// TestPrintScreenCommands: a command that opens a screen fails in zemi run
// and says why, /trust and /health naming their flags; /btw prints its answer.
func TestPrintScreenCommands(t *testing.T) {
	h := printHost(t)
	if printTurn(h, "say hi", false, io.Discard, io.Discard) != 0 {
		t.Fatal("first turn failed")
	}
	for _, c := range []struct {
		prompt, stdout, stderr string
		code                   int
	}{
		{"/help", "", "zemi: /help needs the TUI\n", 1},
		{"/sessions", "", "zemi: /sessions needs the TUI\n", 1},
		{"/rewind", "", "zemi: /rewind needs the TUI\n", 1},
		{"/profile", "", "zemi: /profile needs the TUI\n", 1},
		{"/usage", "", "zemi: /usage needs the TUI\n", 1},
		{"/prompt", "", "zemi: /prompt needs the TUI\n", 1},
		{"/messages", "", "zemi: /messages needs the TUI\n", 1},
		{"/copy", "", "zemi: /copy needs the TUI\n", 1},
		{"/reload", "", "zemi: /reload needs the TUI\n", 1},
		{"/errors", "", "zemi: /errors needs the TUI\n", 1},
		{"/trust", "", "zemi: /trust needs the TUI; --trust trusts the project for one run\n", 1},
		{"/health", "", "zemi: /health needs the TUI; zemi health prints the checks\n", 1},
		{"/model", "", "zemi: /model needs the TUI\n", 1},
		{"/model update", "", "zemi: /model needs the TUI; zemi models update fetches the catalog\n", 1},
		{"/permissions", "", "zemi: /permissions needs the TUI\n", 1},
		{"/mode", "", "zemi: /mode needs the TUI\n", 1},
		{"/memory", "", "zemi: /memory needs the TUI\n", 1},
		{"/skills", "", "zemi: /skills needs the TUI\n", 1},
		{"/skills open zemi", "", "zemi: /skills needs the TUI\n", 1},
		{"/agents", "", "zemi: /agents needs the TUI\n", 1},
		{"/agents view x", "", "zemi: /agents needs the TUI\n", 1},
		{"/workflow", "", "zemi: /workflow needs the TUI\n", 1},
		{"/mcp", "", "zemi: /mcp needs the TUI\n", 1},
		{"/lsp", "", "zemi: /lsp needs the TUI\n", 1},
		{"/btw why", "why", "", 0},
	} {
		var out, errOut strings.Builder
		if code := printTurn(h, c.prompt, false, &out, &errOut); code != c.code || !strings.Contains(out.String(), c.stdout) || c.stdout == "" && out.Len() > 0 || errOut.String() != c.stderr {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.prompt, code, out.String(), errOut.String())
		}
	}
}

// TestPrintInterruptJSON: an interrupted run, /btw included, still prints
// its report, so a script keeps the session id it resumes.
func TestPrintInterruptJSON(t *testing.T) {
	for _, prompt := range []string{"hang", "/btw hang"} {
		h := printHost(t)
		if printTurn(h, "say hi", false, io.Discard, io.Discard) != 0 {
			t.Fatal("first turn failed")
		}
		var out strings.Builder
		code := make(chan int, 1)
		go func() { code <- printTurn(h, prompt, true, &out, io.Discard) }()
		for !h.AgentActive() {
			select {
			case c := <-code:
				t.Fatalf("%s: ended with %d before the interrupt", prompt, c)
			default:
				runtime.Gosched()
			}
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		var rep report
		if c := <-code; c != 130 || json.Unmarshal([]byte(out.String()), &rep) != nil || !rep.IsError || rep.Error != "interrupted" || rep.SessionID == "" || h.AgentActive() {
			t.Errorf("%s: code %d, stdout %q, still active %v", prompt, c, out.String(), h.AgentActive())
		}
	}
}

// TestPipedStdinIsNotTyped: piped stdin reaches the model behind the
// marker, never in a row shown as the user's words, and names a session it
// starts alone.
func TestPipedStdinIsNotTyped(t *testing.T) {
	const diff = "diff --git a/x b/x"
	for _, c := range []struct{ prompt, title string }{
		{"/review", "/review"},
		{"", diff},
	} {
		h := printHost(t)
		if _, err := h.Settings.SetQuiet("session.auto_title", false); err != nil {
			t.Fatal(err)
		}
		if code := printTurn(h, c.prompt+agent.Attached("stdin", diff), false, io.Discard, io.Discard); code != 0 {
			t.Fatalf("%q: code %d", c.prompt, code)
		}
		for _, r := range h.Session().Transcript {
			if r.Role == "user" && strings.Contains(r.Text, diff) {
				t.Errorf("%q: stdin shown as typed: %q", c.prompt, r.Text)
			}
		}
		if s, err := h.DB.GetSession(h.AgentSessionID()); err != nil || !strings.HasPrefix(s.Title, c.title) {
			t.Errorf("%q: session %+v, %v", c.prompt, s, err)
		}
	}
}
