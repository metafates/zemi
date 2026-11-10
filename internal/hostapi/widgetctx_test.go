package hostapi

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// TestViewedRunIsTheLiveOne: while an agent run is shown the ctx reports that
// run's own state, not the session's, so the chat draws its last activity run
// live, its thinking tail included, and the working rule clocks it; the
// session's stream, queue and todos stay out of it.
func TestViewedRunIsTheLiveOne(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{}) // ViewAgent opens a screen
	probe := func() string {
		t.Helper()
		st := h.ScreenState(60, 1, UIState{})
		active := "idle"
		if st.AgentActive {
			active = "active"
		}
		slow := ""
		if st.Agent.Elapsed > 60*time.Second {
			slow = "slow"
		}
		thinking := ""
		if st.Agent.Thinking {
			thinking = "thinking"
		}
		return fmt.Sprintf("%s|%s|%d|%d|%d|%s|%s|%s", active, st.Agent.Text, len(st.Agent.Tools),
			len(st.Agent.Steering), len(st.Todos()), slow, st.Agent.Reasoning, thinking)
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
	probe := func() string {
		t.Helper()
		return strconv.FormatUint(h.ScreenState(60, 1, UIState{}).ViewRevision, 10)
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

// TestConvertSegments: the two returns that unambiguously mean one body line
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
		{"strings", []any{"a", "b"}, "!body of strings and lines"},
		{"single segment", seg("a"), "!an overlay body must be a list of lines, got a single segment"},
		{"number line", []any{42.0}, "!an overlay line must be a list of segments, got a number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, err := convertSegments(c.in)
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
