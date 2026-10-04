package ui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
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
	running := hostapi.StreamSnapshot{Active: true, Elapsed: 6 * time.Second, Tools: []hostapi.StreamTool{{ID: "t1", Name: "shell", Input: "go test ./internal/hostapi", Label: "Run the flush tests", Status: hostapi.CallRun}}}
	for _, c := range []struct {
		name    string
		live    bool
		profile colorprofile.Profile
		frames  []hostapi.RunFrame
		diag    bool // the actor writes a diagnostic as the first Show starts
	}{
		{"a plain log prints settled rows once", false, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, thought}, Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Settled: []hostapi.ChatMessage{failed, reply}},
		}, false},
		{"the live tail redraws in place and commits above itself", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Settled: []hostapi.ChatMessage{user}, Stream: running},
			{Stream: running}, // unchanged: nothing written
			{Settled: []hostapi.ChatMessage{thought, failed, reply}},
		}, false},
		{"NO_COLOR keeps bold and drops color", false, colorprofile.ASCII, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, failed}},
		}, false},
		{"a write from the actor never waits on a render", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Settled: []hostapi.ChatMessage{user}, Stream: running},
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := bootAgentApp(t, 100, 30)
			db := testStore(t, h)
			const sid = "7hq2m4xk"
			if err := db.CreateSession(sid, "", "t", ""); err != nil {
				t.Fatal(err)
			}
			if err := h.ResumeSession(sid); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			v := &RunView{w: &out, profile: c.profile, now: func() time.Time { return time.UnixMilli(0) }}
			if c.live {
				v.size = func() (int, int) { return 100, 30 }
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
			v.Start(h, false)
			for _, fr := range c.frames {
				v.Show(fr, false)
			}
			v.Close()
			golden.RequireEqual(t, out.String())
		})
	}
}
