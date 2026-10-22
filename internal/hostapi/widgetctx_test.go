package hostapi

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// TestRelpath: a chat row writes a path under the working directory
// relative to it and one elsewhere under home from ~, as ctx.cwd reads.
func TestRelpath(t *testing.T) {
	t.Parallel()
	runUser(t, newTestHostUI(t).RT, `
local relpath = require("zemi.tui.text").relpath
for _, c in ipairs({
  { "/h/me/p/a.go", "~/p", "/h/me", "a.go" },
  { "cat /h/me/.x/b /h/me/p/c", "~/p", "/h/me", "cat ~/.x/b c" },
  { "/h/meta/a", "~/p", "/h/me", "/h/meta/a" },
  { "/tmp/p/a.go", "/tmp/p", "/h/me", "a.go" },
}) do
  local got = relpath(c[1], c[2], c[3])
  assert(got == c[4], c[1] .. " reads " .. got .. ", want " .. c[4])
end
`)
}

// TestViewedRunIsTheLiveOne: while an agent run is shown the ctx reports that
// run's own state, not the session's, so the chat draws its last activity run
// live, its thinking tail included, and the working rule clocks it; the
// session's stream, queue and todos stay out of it.
func TestViewedRunIsTheLiveOne(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{}) // ViewAgent opens a screen
	const src = `string.format("%s|%s|%d|%d|%d|%s|%s|%s", ctx.agent_active and "active" or "idle",
	  (ctx.agent or {}).text or "", #((ctx.agent or {}).tools or {}),
	  #((ctx.agent or {}).steering or {}), #(ctx.todos or {}),
	  ((ctx.agent or {}).elapsed or 0) > 60 and "slow" or "",
	  (ctx.agent or {}).reasoning or "", (ctx.agent or {}).thinking and "thinking" or "")`
	if err := h.RT.ExecuteChunk(t.Context(),
		`zemi.ui.widget.add({ id = "probe", region = "statusline.right", render = function(ctx) return { { { text = `+src+` } } } end })`,
		"probe.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	probe := func() string {
		t.Helper()
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("probe"), 60, 1, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		return SegsText(lines[0])
	}
	// the session's own turn, its thinking, its queue and its todos, which a
	// view never reports
	h.agents.mu.Lock()
	h.agents.textBuf, h.agents.steering = "session reply", []string{"hold on"}
	h.agents.reasonBuf, h.agents.thinking = "session thought", true
	h.agents.tools = []StreamTool{{Name: "read", Status: CallRun}}
	h.agents.mu.Unlock()
	h.todoMu.Lock()
	h.todoSID, h.todos = h.AgentSessionID(), []store.Todo{{Content: "a session todo", Status: "pending"}}
	h.todoMu.Unlock()
	const session = "idle|session reply|1|1|1||session thought|thinking"
	if got := probe(); got != session {
		t.Fatalf("the session's ctx reads %q, want %q", got, session)
	}
	// a run of this process, started a minute and a half ago, thinking now
	run, _ := h.newRun(t.Context(), AgentDef{Name: "explore"}, "d", "look", true)
	run.started = time.Now().Add(-90 * time.Second)
	run.mu.Lock()
	run.reason, run.thinkAt = "run thought", time.Now()
	run.mu.Unlock()
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(run.id); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != "active||0|0|0|slow|run thought|thinking" {
		t.Fatalf("the viewed running run reads %q, want its own active state, clock and thinking", got)
	}
	if err := h.ViewAgent(""); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != session {
		t.Fatalf("leaving the view reads %q, want %q", got, session)
	}
}

// TestViewedRunRevision: the ctx reports the shown run's transcript revision,
// which moves when the run writes a row and holds while only its reply
// streams, so the chat can cache the rows it drew of it. The session reports
// none of its own: its rows are the epoch's.
func TestViewedRunRevision(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{}) // ViewAgent opens a screen
	const src = `tostring(ctx.view_revision)`
	if err := h.RT.ExecuteChunk(t.Context(),
		`zemi.ui.widget.add({ id = "probe", region = "statusline.right", render = function(ctx) return { { { text = `+src+` } } } end })`,
		"probe.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	probe := func() string {
		t.Helper()
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("probe"), 60, 1, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		return SegsText(lines[0])
	}
	if got := probe(); got != "0" {
		t.Fatalf("the session's ctx reads %q, want no revision of its own", got)
	}
	run, _ := h.newRun(t.Context(), AgentDef{Name: "explore"}, "d", "look", true)
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(run.id); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != "0" {
		t.Fatalf("a fresh run's ctx reads %q, want revision 0", got)
	}
	// a delta of the reply writes no row: the rows the chat drew still stand
	run.mu.Lock()
	run.text += "and this"
	run.mu.Unlock()
	h.refreshViewed(run)
	if got := probe(); got != "0" {
		t.Fatalf("a streamed delta moved the revision to %q", got)
	}
	(&subagentHooks{h: h, run: run, def: AgentDef{Name: "explore"}}).ToolCallStart("c1", "shell", "ls")
	if got := probe(); got == "0" {
		t.Fatalf("a written row left the revision at %q", got)
	}
}

// BenchmarkWidgetCtx renders a statusline-like widget that reads one field
// over an empty and a 500-row transcript: the gap is what building the
// transcript for widgets that never read it costs.
func BenchmarkWidgetCtx(b *testing.B) {
	for _, rows := range []int{0, 500} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: b.TempDir()})
			if err := r.Boot(b.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				b.Fatal(err)
			}
			if err := r.ExecuteChunk(b.Context(), `zemi.ui.widget.add({ id = "m", region = "statusline.right", render = function(ctx) return { { { text = ctx.model } } } end })`, "b.lua", lua.OriginUser); err != nil {
				b.Fatal(err)
			}
			diff := strings.Repeat("- old line of code here\n+ new line of code here\n", 20)
			for i := range rows {
				h.AppendChat(ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "internal/x.go", Label: "edit x", Status: "ok", Diff: diff, Output: "done\nok"})
				if i%2 == 0 {
					h.AppendChat(ChatMessage{Role: "user", Text: "please change the thing in the other place " + strconv.Itoa(i)})
				}
			}
			w := h.Widgets.Get("m")
			for b.Loop() {
				if _, _, err := h.RenderWidget(b.Context(), w, 40, 1, UIState{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestPrintedRender: a frame zemi run prints draws the rows it is handed on
// the rail, never folded into a digest, the thought whole only with
// --reasoning; the splash stays out of it.
func TestPrintedRender(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	rows := []ChatMessage{
		{Role: "assistant", Kind: RowReasoning, Text: "hmm", Stat: &TurnStat{ThoughtMS: 1200}},
		{Role: "assistant", Kind: RowTool, Tool: "shell", Text: "go test", Status: CallErr, Output: "FAIL\nexit status 1"},
		{Role: "assistant", Text: "it fails"},
	}
	h.setTranscript("t", rows)
	chat := func(state UIState) string {
		var b strings.Builder
		for _, w := range h.Widgets.InRegion("chat") {
			lines, _, err := h.RenderWidget(t.Context(), w, 80, 30, state)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range lines {
				for _, s := range l {
					b.WriteString(s.Text)
				}
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	for _, c := range []struct {
		name            string
		state           UIState
		digest, thought bool // the digest row, and the thought's text
		rail            bool // the run's own rows, the failed call's output among them
	}{
		{"the TUI folds the thinking alone", UIState{}, true, false, false},
		{"a printed frame keeps the rail", UIState{Printed: &Printed{Rows: rows}}, false, false, true},
		{"--reasoning prints the thought", UIState{Printed: &Printed{Rows: rows, Reasoning: true}}, false, true, true},
	} {
		got := chat(c.state)
		if strings.Contains(got, "· thought") != c.digest || strings.Contains(got, "hmm") != c.thought ||
			strings.Contains(got, "exit status 1") != c.rail || !strings.Contains(got, "it fails") {
			t.Errorf("%s:\n%s", c.name, got)
		}
	}
	// a run holding neither a counted call nor a thought has no digest: its
	// rail is all there is to draw
	h.setTranscript("t", []ChatMessage{{Role: "assistant", Kind: RowTool, Tool: "edit", Text: "a.go", Status: CallErr, Output: "old_string not found"}})
	if got := chat(UIState{}); strings.Contains(got, "· ") || !strings.Contains(got, "edit a.go") {
		t.Errorf("a run with nothing to fold folded:\n%s", got)
	}
	if got := chat(UIState{Printed: &Printed{}}); strings.TrimSpace(got) != "" {
		t.Errorf("a printed frame with no rows draws:\n%s", got)
	}
	// the pending row draws once: the rows printed above a tail leave it to
	// the tail, a plain log keeps it
	done := h.Busy("Checking health", nil)
	defer done()
	if got := chat(UIState{Printed: &Printed{Rows: rows}}); !strings.Contains(got, "checking health") {
		t.Errorf("the rows printed above a tail draw no pending row:\n%s", got)
	}
	if got := chat(UIState{Printed: &Printed{Rows: rows, Tail: true}}); strings.Contains(got, "checking health") {
		t.Errorf("the tail redraws the pending row the rows above it drew:\n%s", got)
	}
	// the last row printed above seeds the first unit's block gap
	prev := &Printed{Rows: rows, Prev: &ChatMessage{Role: "assistant", Kind: RowTool, Tool: "shell", Text: "ls", Status: CallOK}}
	if got := chat(UIState{Printed: prev}); strings.HasPrefix(got, "\n") {
		t.Errorf("an activity unit after an activity row starts with a blank:\n%s", got)
	}
	prev.Prev = &ChatMessage{Role: "user", Text: "go"}
	if got := chat(UIState{Printed: prev}); !strings.HasPrefix(got, "\n") {
		t.Errorf("an activity unit after a user row lost its blank:\n%s", got)
	}
}

// TestWidgetCtxLazy: the built-on-read fields work inside render, and a
// ctx kept past it keeps what render read and reads nil for the rest.
func TestWidgetCtxLazy(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	h.AppendMessage("user", "hi")
	runUser(t, r, `
		local kept
		zemi.ui.widget.add({ id = "k", region = "header", render = function(ctx)
			assert(#ctx.transcript == 1 and ctx.transcript_count == 1 and ctx.transcript[1].text == "hi", "transcript")
			assert(type(ctx.agent.tools) == "table" and ctx.recent == nil, "lazy fields")
			kept = ctx
			return {}
		end })
		zemi.command.add("check", function()
			assert(kept.transcript ~= nil and kept.agent ~= nil and kept.width == 10, "what render read stays")
			assert(kept.todos == nil, "the rest reads nil")
		end)`)
	if _, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("k"), 10, 1, UIState{}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.command.run("check"))`)
}

// TestConvertSegments: the two returns that unambiguously mean one line
// are wrapped, and the ambiguous one names both spellings (DESIGN §6.0).
func TestConvertSegments(t *testing.T) {
	seg := func(text string) map[string]any { return map[string]any{"text": text} }
	for _, c := range []struct {
		name string
		in   any
		want string // rendered text per line, joined by "|"; "!" prefixes an expected error
	}{
		{"lines", []any{[]any{"a"}, []any{"b"}}, "a|b"},
		{"bare string", "a", "a"},
		{"bare line", []any{seg("a"), seg("b")}, "ab"},
		{"one string", []any{"a"}, "a"},
		{"empty table", map[string]any{}, ""},
		{"strings", []any{"a", "b"}, "!list of strings"},
		{"single segment", seg("a"), "!widget render must return a list of lines, got a single segment"},
		{"number line", []any{42.0}, "!widget line must be a list of segments, got a number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, err := convertSegments(c.in, "w")
			if want, isErr := strings.CutPrefix(c.want, "!"); isErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, row := range rows {
				var b strings.Builder
				for _, s := range row {
					b.WriteString(s.Text)
				}
				lines = append(lines, b.String())
			}
			if got := strings.Join(lines, "|"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestTableSegments: the direct path gives what ToGo and convertSegments
// give, sanitizing included (a segment is one row, so a newline is a space),
// leaves odd shapes to them, and converts a line table it saw in the last
// render only once.
func TestTableSegments(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		local kept = { { text = "kept" } }
		local shapes = {
			{ { "a\27[31m\n", { text = "b\27]0;x\7\nc", hl = "Host:BtnSel", click = "go", copy = false, hover = false, soft_wrap = true, effect = "shimmer" } }, kept },
			{ kept },
			{ {} },
			{ { { "nested" } } },
			{ { "a", x = 1 } },
			"bare",
		}
		local n = 0
		zemi.ui.widget.add({ id = "s", region = "header", render = function()
			n = n + 1
			if n == 2 then kept[1].text = "changed" end
			return shapes[n]
		end })`)
	w := h.Widgets.Get("s")
	for i, direct := range []bool{true, true, false, false, false, false} {
		var got [][]Segment
		_, err := h.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			if err := L.CallByParam(glua.P{Fn: w.Render, NRet: 1, Protect: true}); err != nil {
				return nil, err
			}
			ret := L.Get(-1)
			L.Pop(1)
			lines, ok := w.tableSegments(ret)
			if ok != direct {
				t.Errorf("shape %d: direct = %v, want %v", i+1, ok, direct)
			}
			if v, err := lua.ToGo(ret); ok && err == nil {
				got, _ = convertSegments(v, w.ID)
				if i == 0 && (!reflect.DeepEqual(lines, got) || strings.ContainsAny(lines[0][0].Text+lines[0][1].Text, "\x1b\n")) {
					t.Errorf("direct %+v, generic %+v", lines, got)
				}
				got = lines
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && got[0][0].Text != "kept" {
			t.Errorf("a line table seen last render was converted again: %q", got[0][0].Text)
		}
	}
}

// TestInstructionsCached: ctx.instructions reads the instruction files once,
// not per render, and again after a trust change.
func TestInstructionsCached(t *testing.T) {
	t.Parallel()
	h, r := newTestHostSkills(t)
	h.SetProjectTrusted(true)
	runUser(t, r, `zemi.ui.widget.add({ id = "inc", region = "header", render = function(ctx)
		return { { { text = table.concat(ctx.instructions, ",") } } }
	end })`)
	for _, c := range []struct {
		name string
		do   func()
		want string
	}{
		{"first render", func() {}, ""},
		{"a file written since is not reread", func() {
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a trust change reads again", func() { h.SetProjectTrusted(true) }, "AGENTS.md"},
	} {
		c.do()
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("inc"), 20, 1, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		if got := lines[0][0].Text; got != c.want {
			t.Fatalf("%s: instructions %q, want %q", c.name, got, c.want)
		}
	}
}

// TestWidgetCtxRecentMarks: the splash's recent rows lead each title with the
// session's marker, derived from its id.
func TestWidgetCtxRecentMarks(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	sid := store.ShortID()
	if err := h.DB.CreateSession(sid, "", "named", ""); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, fmt.Sprintf(`
		zemi.ui.widget.add({ id = "rc", region = "header", render = function(ctx)
			assert(#ctx.recent == 1, "recent rows")
			assert(ctx.recent[1].id == %q and ctx.recent[1].title == %q .. " named", ctx.recent[1].title)
			return {}
		end })`, sid, SessionMark(sid)))
	if _, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("rc"), 20, 1, UIState{}); err != nil {
		t.Fatal(err)
	}
}
