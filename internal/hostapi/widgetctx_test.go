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

	"charm.land/fantasy"
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

// TestAgentTableCachesToolRows: a render reuses a finished tool's subtable,
// refreshes only a running row's elapsed, and rebuilds one whose fields
// moved - same length included - so a long turn builds each row once.
func TestAgentTableCachesToolRows(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	start := time.Unix(1000, 0)
	tools := func() []StreamTool {
		return []StreamTool{
			{Name: "read", Input: "a.go", Status: CallOK, Label: "read it", Diff: "+ x", Started: start},
			{Name: "shell", Input: "ls", Status: CallRun, Detail: "working", Started: start},
		}
	}
	rows := func(stream []StreamTool, now time.Time) []*glua.LTable {
		var out []*glua.LTable
		_, err := h.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			tb := h.agentTable(L, StreamSnapshot{Tools: stream}, now).RawGetString("tools").(*glua.LTable)
			for i := 1; i <= tb.Len(); i++ {
				out = append(out, tb.RawGetInt(i).(*glua.LTable))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := rows(tools(), start)
	again := rows(tools(), start.Add(3*time.Second))
	for i := range first {
		if first[i] != again[i] {
			t.Fatalf("tool %d rebuilt", i+1)
		}
	}
	if n := again[1].RawGetString("elapsed"); n != glua.LNumber(3) {
		t.Fatalf("the running row's elapsed is %v, want 3", n)
	}
	moved := tools()
	moved[1].Detail, moved[1].Status = "workign", CallOK
	got := rows(moved, start)
	if got[0] != first[0] || got[1] == first[1] {
		t.Fatal("the wrong rows rebuilt")
	}
	if s := got[1].RawGetString("status"); s != glua.LString(CallOK) {
		t.Fatalf("the rebuilt row reads %v, want ok", s)
	}
	got = rows(append(moved, StreamTool{Name: "edit", Status: CallRun, Started: start}), start)
	if got[0] != first[0] || got[1] == first[1] || len(got) != 3 {
		t.Fatal("an appended tool moved the rows before it")
	}
}

// TestTranscriptTableCachesRows: a render reuses an unchanged row's subtable
// and rebuilds one whose fields moved - same length included - while a longer
// transcript reuses the prefix; a moved epoch, edit, revision or a shorter
// transcript rebuilds them all, so no stale row reaches a widget.
func TestTranscriptTableCachesRows(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	rows := func(epoch, edit int64, rev uint64, tr []ChatMessage) []*glua.LTable {
		var out []*glua.LTable
		_, err := h.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			tb := h.transcriptTable(L, epoch, edit, rev, tr)
			for i := 1; i <= tb.Len(); i++ {
				out = append(out, tb.RawGetInt(i).(*glua.LTable))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := func() []ChatMessage {
		return []ChatMessage{
			{Role: fantasy.MessageRoleUser, Text: "hi"},
			{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "read", Status: CallOK},
		}
	}
	first := rows(1, 0, 0, base())
	again := rows(1, 0, 0, base())
	for i := range first {
		if first[i] != again[i] {
			t.Fatalf("row %d rebuilt", i+1)
		}
	}
	cases := []struct {
		name          string
		epoch, edit   int64
		rev           uint64
		tr            func() []ChatMessage
		keptFromFirst int // rows keeping their table from the first render
	}{
		{"appended", 1, 0, 0, func() []ChatMessage {
			return append(base(), ChatMessage{Role: fantasy.MessageRoleUser, Text: "more"})
		}, 2},
		{"changed, stamps same", 1, 0, 0, func() []ChatMessage {
			tr := base()
			tr[1].Status = CallCancelled
			return tr
		}, 1},
		{"epoch moved", 2, 0, 0, base, 0},
		{"edit moved", 1, 1, 0, base, 0},
		{"revision moved", 1, 0, 1, base, 0},
		{"shrunk", 1, 0, 0, func() []ChatMessage { return base()[:1] }, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows(1, 0, 0, base()) // settle the cache at two rows
			prev := rows(1, 0, 0, base())
			got := rows(c.epoch, c.edit, c.rev, c.tr())
			for i := range got {
				switch {
				case i < c.keptFromFirst && got[i] != prev[i]:
					t.Fatalf("row %d rebuilt", i+1)
				case i >= c.keptFromFirst && i < len(prev) && got[i] == prev[i]:
					t.Fatalf("row %d kept its table", i+1)
				}
			}
			if c.name == "changed, stamps same" {
				if s := got[1].RawGetString("status"); s != glua.LString(CallCancelled) {
					t.Fatalf("the rebuilt row reads %v, want cancelled", s)
				}
			}
		})
	}
}
