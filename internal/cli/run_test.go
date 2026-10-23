package cli

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/ui"
)

func TestFollower(t *testing.T) {
	tool := func(id string, s hostapi.CallStatus) hostapi.StreamTool {
		return hostapi.StreamTool{ID: id, Name: "shell", Input: "ls", Status: s, Output: "x\nexit status 1"}
	}
	row := func(t hostapi.StreamTool) hostapi.ChatMessage {
		return hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowTool, CallID: t.ID, Tool: t.Name, Text: t.Input, Status: t.Status, Output: t.Output}
	}
	live := func(ts ...hostapi.StreamTool) hostapi.RunFrame {
		return hostapi.RunFrame{Stream: hostapi.StreamSnapshot{Tools: ts}}
	}
	ask := hostapi.Denial{Tool: "write", Subject: "fs.write", Arg: "x", Reason: "agent general: untrusted", Why: hostapi.DenyApproval}
	for _, c := range []struct {
		name    string
		frames  []hostapi.RunFrame
		denials [][]hostapi.Denial // the run's denials at each frame
		want    string             // the events' types, and a tool event's id, status and error
		result  string
		isError bool
		denied  int
	}{
		{name: "a fast call gets its start before its end", frames: []hostapi.RunFrame{live(tool("a", hostapi.CallOK))}, want: "tool_start:a tool_end:a:ok"},
		{name: "a call handed to the background has no error", frames: []hostapi.RunFrame{live(tool("a", hostapi.CallBG))}, want: "tool_start:a tool_end:a:bg"},
		{name: "a running call starts once", frames: []hostapi.RunFrame{live(tool("a", hostapi.CallWait)), live(tool("a", hostapi.CallRun)), live(tool("a", hostapi.CallRun))}, want: "tool_start:a"},
		{
			name:   "a finished live call ends before its row settles, and the row sends nothing",
			frames: []hostapi.RunFrame{live(tool("a", hostapi.CallRun)), {Pending: []hostapi.ChatMessage{row(tool("a", hostapi.CallErr))}}, {Settled: []hostapi.ChatMessage{row(tool("a", hostapi.CallErr))}}},
			want:   "tool_start:a tool_end:a:err:exit status 1",
		},
		{
			name:   "the last message is the result",
			frames: []hostapi.RunFrame{{Settled: []hostapi.ChatMessage{{Role: "user", Text: "go"}, {Role: "assistant", Text: "looking"}, {Role: "assistant", Kind: hostapi.RowCompact, Text: "cleared"}, {Role: "assistant", Text: "found"}}}},
			want:   "message compact message", result: "found",
		},
		{name: "an error row makes the run an error", frames: []hostapi.RunFrame{{Settled: []hostapi.ChatMessage{{Role: "assistant", Kind: hostapi.RowError, Text: "rate limited"}}}}, want: "error", isError: true},
		{name: "a denial is an ask and a done.denied row", frames: []hostapi.RunFrame{{}, {}}, denials: [][]hostapi.Denial{{ask}, {ask}}, want: "ask", denied: 1},
		{
			name:   "a retry is one event per attempt, numbered as the working rule does",
			frames: []hostapi.RunFrame{{Stream: hostapi.StreamSnapshot{Retries: 1}}, {Stream: hostapi.StreamSnapshot{Retries: 1}}, {Stream: hostapi.StreamSnapshot{Retries: 2}}},
			want:   "retry:2 retry:3",
		},
		{
			name: "an id-less call collapses into one start and one end",
			frames: []hostapi.RunFrame{
				{Pending: []hostapi.ChatMessage{row(hostapi.StreamTool{Name: "shell", Input: "ls", Status: hostapi.CallErr, Output: "x\nexit status 1"})}},
				{Pending: []hostapi.ChatMessage{row(hostapi.StreamTool{Name: "shell", Input: "ls", Status: hostapi.CallErr, Output: "x\nexit status 1"}), row(hostapi.StreamTool{Name: "edit", Input: "a", Status: hostapi.CallOK})}},
			},
			want: "tool_start tool_end:err:exit status 1",
		},
		{
			// the turns of one run chain under one active stretch, so the
			// second turn's first retry is a new attempt, not the first turn's
			name: "a chained turn's retry refires once, an unchanged turn never",
			frames: []hostapi.RunFrame{
				{Stream: hostapi.StreamSnapshot{Active: true, Retries: 2, Turn: 1}},
				{Stream: hostapi.StreamSnapshot{Active: true, Retries: 2, Turn: 1}},
				{Stream: hostapi.StreamSnapshot{Active: true, Retries: 1, Turn: 2}},
				{Stream: hostapi.StreamSnapshot{Active: true, Retries: 1, Turn: 2}},
			},
			want: "retry:3 retry:2",
		},
	} {
		f := newFollower()
		var got []string
		for i, fr := range c.frames {
			var ds []hostapi.Denial
			if i < len(c.denials) {
				ds = c.denials[i]
			}
			for _, ev := range f.step(fr, ds) {
				s := string(ev.Type)
				if ev.ID != "" {
					s += ":" + ev.ID
				}
				if ev.Status != "" {
					s += ":" + ev.Status
				}
				if ev.Error != "" {
					s += ":" + ev.Error
				}
				if ev.Attempt != 0 {
					s += fmt.Sprint(":", ev.Attempt)
				}
				got = append(got, s)
			}
		}
		if strings.Join(got, " ") != c.want || f.done.Result != c.result || f.done.IsError != c.isError || len(f.done.Denied) != c.denied {
			t.Errorf("%s: %q, done %+v", c.name, got, f.done)
		}
	}
}

func TestDeniedHint(t *testing.T) {
	for _, c := range []struct {
		trusted bool
		want    string
	}{
		{false, "--trust trusts this project"},
		{true, "a plugin's permission.request handler"},
	} {
		if got := deniedHint(c.trusted); !strings.Contains(got, c.want) {
			t.Errorf("trusted %v: %q", c.trusted, got)
		}
	}
}

// fakeOpenAI is an OpenAI-compatible chat server: "call NAME {json}" calls
// that tool, "say X" replies X, a message ending in "hang" waits for the
// request to end (10s at most, so a cancel that never comes fails a test
// instead of hanging it), and a tool result gets "done".
func fakeOpenAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	_ = json.UnmarshalRead(r.Body, &body)
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
		_ = json.MarshalWrite(w, map[string]any{
			"id": "c", "object": "chat.completion", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
		}, json.Deterministic(true))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(delta map[string]any, fin any) {
		b, _ := json.Marshal(map[string]any{
			"id": "c", "object": "chat.completion.chunk", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		}, json.Deterministic(true))
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

// printHost is a host with /new, the shipped widgets and a fake model, in
// an untrusted project.
func printHost(t *testing.T) *hostapi.Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fakeOpenAI))
	t.Cleanup(srv.Close)
	_, h, r := newTrustGate(t)
	if err := r.Boot(t.Context(), coreSteps(h, mainCommands(func() {}, func() {}))); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`zemi.provider.add({ id = "fake", kind = zemi.provider.kind.OPENAI_COMPAT, api_key = "x", base_url = %q, models = { { id = "m", context_window = 128000 } } })
		zemi.config.set("models.chat", "fake/m")`, srv.URL+"/v1")
	if err := r.ExecuteChunk(t.Context(), cfg, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	return h
}

// runPrompt is one zemi run over h, its stderr a file as a CI log is.
func runPrompt(t *testing.T, h *hostapi.Host, o options) (code int, stdout, stderr string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	view := ui.NewRunView(f, o.quiet || o.json)
	h.Stderr = view
	var out strings.Builder
	code = runHeadless(h, view, o, &out)
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), string(b)
}

// lastDone is the done line a --json run ends with.
func lastDone(t *testing.T, stdout string) doneEvent {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var d doneEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &d); err != nil || d.Type != "done" {
		t.Fatalf("last line of %q: %v", stdout, err)
	}
	return d
}

// TestRunHeadless: what zemi run prints for a turn, and which asks it names.
func TestRunHeadless(t *testing.T) {
	for _, c := range []struct {
		name, prompt      string
		first             []string // turns run before, -c style
		cont              bool     // the boot resumed a session, -c style
		setup             func(h *hostapi.Host)
		stdout, stderr    string // wanted substrings
		notStdout, notErr string
		denied            int
		deniedTool        string
		jsonDone          func(t *testing.T, d doneEvent, out string)
	}{
		{
			name: "escapes are stripped", prompt: "say \x1b]52;c;SGk=\a\x1b[31mhi", stdout: "hi", stderr: "resume   zemi -r ", notStdout: "\x1b", notErr: "\x1b",
			jsonDone: func(t *testing.T, d doneEvent, out string) {
				if strings.ContainsRune(out, '\x1b') || d.Result != "hi" {
					t.Errorf("--json: raw escape on stdout or result %q", d.Result)
				}
			},
		},
		{name: "an ask policy call is a denied row", prompt: `call shell {"command":"ls -la"}`, setup: func(h *hostapi.Host) {
			if err := h.RT.ExecuteChunk(t.Context(), `zemi.event.on("permission.request", function() return { decision = "ask" } end)`, "init.lua", lua.OriginUser); err != nil {
				t.Fatal(err)
			}
		}, stderr: "✗ run ls -la", denied: 1, deniedTool: "shell"},
		{name: "a read-only agent does not write", prompt: `call write {"path":"x.txt","content":"x"}`, setup: func(h *hostapi.Host) {
			if err := h.RT.ExecuteChunk(t.Context(), `zemi.agent.profile.add("planner", { read_only = true })`, "init.lua", lua.OriginUser); err != nil {
				t.Fatal(err)
			}
			if err := h.SetProfile("planner"); err != nil {
				t.Fatal(err)
			}
		}, stderr: "permission denied (read-only agent)", notErr: "nobody could answer"},
		{
			name: "a subagent's unanswered ask is named", prompt: `call agent {"agent":"general","description":"d","prompt":"call write {\"path\":\"zz.txt\",\"content\":\"x\"}"}`,
			setup: func(h *hostapi.Host) {
				if err := h.RT.ExecuteChunk(t.Context(), `zemi.event.on("permission.request", function() return { decision = "ask" } end)`, "init.lua", lua.OriginUser); err != nil {
					t.Fatal(err)
				}
			},
			stderr: "zemi: 1 ask nobody could answer in a headless run:\n  write  ", denied: 1, deniedTool: "write",
		},
		{name: "a swapped-in session is not this run's", first: []string{"say hi", "/new"}, prompt: "/resume", notStdout: "hi"},
		{
			// the resume is the boot's: its swap must read as spent before the
			// run's send, or the turn's rows and usage would drop off the done
			name: "a boot resume's swap is not this run's", first: []string{"say hi", "/new"}, cont: true, prompt: "say done", stdout: "done",
			jsonDone: func(t *testing.T, d doneEvent, _ string) {
				if d.Result != "done" || d.Usage.Input != 10 || d.Usage.Output != 2 {
					t.Errorf("--json done: result %q, usage %+v", d.Result, d.Usage)
				}
			},
		},
		{name: "/btw prints only its answer", first: []string{"say hello"}, prompt: "/btw why", stdout: "why", notErr: "hello"},
	} {
		h := printHost(t)
		if c.setup != nil {
			c.setup(h)
		}
		for _, p := range c.first {
			if code, _, _ := runPrompt(t, h, options{prompt: p, quiet: true}); code != 0 {
				t.Fatalf("%s: %q failed", c.name, p)
			}
		}
		if c.cont {
			if err := h.ResumeSession(""); err != nil {
				t.Fatal(err)
			}
		}
		code, out, errOut := runPrompt(t, h, options{prompt: c.prompt})
		if code != 0 || !strings.Contains(out, c.stdout) || !strings.Contains(errOut, c.stderr) ||
			c.notStdout != "" && strings.Contains(out, c.notStdout) || c.notErr != "" && strings.Contains(errOut, c.notErr) {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out, errOut)
		}
		if c.denied > 0 {
			_, out, _ := runPrompt(t, h, options{prompt: c.prompt, json: true})
			if d := lastDone(t, out); len(d.Denied) != c.denied || !strings.Contains(out, `{"type":"ask","tool":"`+c.deniedTool+`"`) {
				t.Errorf("%s: --json denied %+v in %s", c.name, d.Denied, out)
			}
		}
		if c.jsonDone != nil {
			_, out, _ := runPrompt(t, h, options{prompt: c.prompt, json: true})
			c.jsonDone(t, lastDone(t, out), out)
		}
	}
}

// TestRunScreenCommands: a command that opens a screen fails in zemi run
// and says why, /trust and /health naming their flags; a /goal chain exits 1
// when it leaves the goal set.
func TestRunScreenCommands(t *testing.T) {
	h := printHost(t)
	if code, _, _ := runPrompt(t, h, options{prompt: "say hi", quiet: true}); code != 0 {
		t.Fatal("first turn failed")
	}
	// stdin after - is always a prompt: piped text starting with / runs nothing
	if code, _, errOut := runPrompt(t, h, options{prompt: "/help", text: true, quiet: true}); code != 0 || errOut != "" {
		t.Errorf("/help from stdin: code %d, stderr %q", code, errOut)
	}
	for _, c := range []struct {
		prompt, stdout, stderr string
		code                   int
	}{
		{"/help", "", "zemi: /help needs the TUI\n", 1},
		{"/sessions", "", "zemi: /sessions needs the TUI; zemi sessions lists them\n", 1},
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
		{"/model", "", "zemi: /model needs the TUI; zemi models lists the ids, /model [<role>] <id> sets one\n", 1},
		{"/model update", "", "zemi: /model needs the TUI; zemi models update fetches the catalog\n", 1},
		{"/memory", "", "zemi: /memory needs the TUI\n", 1},
		{"/skills", "", "zemi: /skills needs the TUI\n", 1},
		{"/skills open zemi", "", "zemi: /skills needs the TUI\n", 1},
		{"/agents", "", "zemi: /agents needs the TUI\n", 1},
		{"/agents view x", "", "zemi: /agents needs the TUI\n", 1},
		{"/workflows", "", "zemi: /workflows needs the TUI\n", 1},
		{"/mcp", "", "zemi: /mcp needs the TUI\n", 1},
		{"/lsp", "", "zemi: /lsp needs the TUI\n", 1},
		{"/goal", "", "zemi: /goal needs the TUI\n", 1},
		{"/loop 5m check", "", "zemi: /loop needs the TUI; from a shell, run zemi run in a loop or from cron\n", 1},
		// the fake model says what follows "say " in the goal, the check too
		{"/goal say MET: yes", "MET: yes", "", 0},
		{"/goal say BLOCKED: needs a key", "BLOCKED", "zemi: goal blocked: needs a key\n", 1},
	} {
		if code, out, errOut := runPrompt(t, h, options{prompt: c.prompt, quiet: true}); code != c.code || !strings.Contains(out, c.stdout) || c.stdout == "" && out != "" || errOut != c.stderr {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.prompt, code, out, errOut)
		}
	}
}

// TestRunFailedJSON: a run that fails before its turn is one done line.
func TestRunFailedJSON(t *testing.T) {
	var out, errOut strings.Builder
	if code := runFailed(errors.New("no model configured"), true, &out, &errOut); code != 1 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if d := lastDone(t, out.String()); !d.IsError || d.Error != "no model configured" || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout = %q", out.String())
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
		if code, _, _ := runPrompt(t, h, options{prompt: c.prompt + agent.Attached("stdin", diff), quiet: true}); code != 0 {
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
