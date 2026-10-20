package ui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestRunView: what zemi run prints on stderr for a run, frame by frame.
func TestRunView(t *testing.T) {
	user := hostapi.ChatMessage{Role: "user", Text: "why does TestFlushOrder fail?"}
	thought := hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowReasoning, Text: "hmm", Stat: &hostapi.TurnStat{ThoughtMS: 2400}}
	failed := hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowTool, Tool: "shell", Text: "go test ./internal/hostapi", Label: "Run the flush tests", Status: hostapi.CallErr, Output: "--- FAIL: TestFlushOrder\nexit status 1"}
	reply := hostapi.ChatMessage{Role: "assistant", Text: "It fails because the reply lands first."}
	edit := hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowTool, Tool: "edit", Text: "a.go", Label: "edit a", Status: hostapi.CallOK, Diff: "--- a.go\n+++ a.go\n@@ -1 +1 @@\n-old\n+new"}
	running := hostapi.StreamSnapshot{Active: true, Elapsed: 6 * time.Second, Tools: []hostapi.StreamTool{{ID: "t1", Name: "shell", Input: "go test ./internal/hostapi", Label: "Run the flush tests", Status: hostapi.CallRun}}}
	longUser := hostapi.ChatMessage{Role: "user", Text: strings.Repeat("please look around the repository and report back ", 10)}
	// tailsOf reads each tail the view drew, between its show-cursor and
	// sync-update ends.
	tails := func(t *testing.T, out string) []string {
		t.Helper()
		var got []string
		for part := range strings.SplitSeq(out, "\x1b[?25l") {
			if tail, _, ok := strings.Cut(part, "\x1b[?2026l"); ok && part != "" {
				got = append(got, tail)
			}
		}
		return got
	}
	// rowWidths checks the printed chat rows: the working rule spans the
	// terminal and its bar rows are the only ones with a rule run in them.
	rowWidths := func(t *testing.T, out string, limit int) {
		t.Helper()
		for row := range strings.SplitSeq(ansi.Strip(out), "\n") {
			if strings.Contains(row, "───") {
				continue
			}
			if w := ansi.StringWidth(row); w > limit {
				t.Errorf("a printed row is %d cells, want at most %d: %q", w, limit, row)
			}
		}
	}
	for _, c := range []struct {
		name      string
		live      bool
		profile   colorprofile.Profile
		frames    []hostapi.RunFrame
		diag      bool // the actor writes a diagnostic as the first Show starts
		dead      bool // a chat widget whose render raises fails the view
		reasoning bool // --reasoning
		w         int  // the terminal's width; 0: the 100 a live tail gets
		advance   time.Duration
		check     func(t *testing.T, out string)
	}{
		{"a plain log prints settled rows once", false, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, thought}, Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Settled: []hostapi.ChatMessage{failed, reply}},
		}, false, false, false, 0, 0, nil},
		{"the live tail redraws in place and commits above itself", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Settled: []hostapi.ChatMessage{user}, Stream: running},
			{Stream: running}, // unchanged: nothing written
			{Settled: []hostapi.ChatMessage{thought, failed, reply}},
		}, false, false, false, 0, 0, nil},
		{"NO_COLOR keeps bold and drops color", false, colorprofile.ASCII, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, failed}},
		}, false, false, false, 0, 0, nil},
		{"a write from the actor never waits on a render", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Settled: []hostapi.ChatMessage{user}, Stream: running},
		}, true, false, false, 0, 0, nil},
		{"TrueColor prints a diff row's tint with its fg", false, colorprofile.TrueColor, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{edit}},
		}, false, false, true, 0, 0, func(t *testing.T, out string) {
			for _, want := range []string{"38;2;", ";48;2;"} {
				if !strings.Contains(out, want) {
					t.Errorf("the diff's rows print no %q pair:\n%s", want, ansi.Strip(out))
				}
			}
		}},
		{"the tail's spinner and the snail step with the frame", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
		}, false, false, false, 0, 300 * time.Millisecond, func(t *testing.T, out string) {
			got := tails(t, out)
			if len(got) < 2 {
				t.Fatalf("no tail redrawn in place:\n%q", out)
			}
			if !strings.Contains(got[0], hostapi.Spinner[0]) || !strings.Contains(got[1], hostapi.Spinner[6]) {
				t.Errorf("the tail's spinner did not step: %q -> %q", got[0], got[1])
			}
			if !strings.Contains(got[0], "@··") || !strings.Contains(got[1], "@.·") {
				t.Errorf("the snail's eyes did not step: %q -> %q", got[0], got[1])
			}
		}},
		{"chat rows draw at the stage width", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{longUser}},
		}, false, false, false, 160, 0, func(t *testing.T, out string) {
			rowWidths(t, out, 100)
		}},
		{"chat rows fit a narrow terminal", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{longUser}},
		}, false, false, false, 80, 0, func(t *testing.T, out string) {
			rowWidths(t, out, 80)
		}},
		{"a dead view still prints the resume hint", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user}},
		}, false, true, false, 0, 0, func(t *testing.T, out string) {
			if !strings.Contains(ansi.Strip(out), "zemi: run view:") || !strings.Contains(ansi.Strip(out), "resume") {
				t.Errorf("a dead view lost its resume hint:\n%s", ansi.Strip(out))
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, h := bootAgentApp(t, 100, 30)
			db := testStore(t, h)
			const sid = "7hq2m4xk"
			if err := db.CreateSession(sid, "", "t", ""); err != nil {
				t.Fatal(err)
			}
			if err := h.ResumeSession(sid); err != nil {
				t.Fatal(err)
			}
			if c.dead {
				if err := h.RT.ExecuteChunk(t.Context(),
					`zemi.ui.widget.add({ id = "boom", region = "chat", render = function() error("boom") end })`,
					"boom.lua", lua.OriginUser); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			at := time.UnixMilli(0)
			v := &RunView{w: &out, profile: c.profile, now: func() time.Time { return at }}
			if c.live {
				w := c.w
				if w == 0 {
					w = 100
				}
				v.size = func() (int, int) { return w, 30 }
			}
			if c.diag {
				// the actor holds the write while Show goes on to render
				v.size = sync.OnceValues(func() (int, int) {
					writing := make(chan struct{})
					go h.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(*glua.LState) ([]any, error) {
						close(writing)
						fmt.Fprintln(v, "zemi: warning: tick")
						return nil, nil
					})
					<-writing
					return 100, 30
				})
			}
			v.Start(h, c.reasoning)
			for i, fr := range c.frames {
				if c.advance > 0 && i > 0 {
					at = at.Add(c.advance)
				}
				v.Show(fr, false)
			}
			v.Close()
			v.Close() // the run loop calls it again through its defer
			if c.check != nil {
				c.check(t, out.String())
			}
			golden.RequireEqual(t, out.String())
		})
	}
}
