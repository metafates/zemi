package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
)

// TestRunView: what zemi run prints on stderr for a run, frame by frame.
func TestRunView(t *testing.T) {
	user := hostapi.ChatMessage{Role: "user", Text: "why does TestFlushOrder fail?"}
	thought := hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowReasoning, Text: "hmm", Stat: &hostapi.TurnStat{ThoughtMS: 2400}}
	failed := hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowTool, Tool: "bash", Text: "go test ./internal/hostapi", Label: "Run the flush tests", Status: hostapi.CallErr, Output: "--- FAIL: TestFlushOrder\nexit status 1"}
	reply := hostapi.ChatMessage{Role: "assistant", Text: "It fails because the reply lands first."}
	running := hostapi.StreamSnapshot{Active: true, Elapsed: 6 * time.Second, Tools: []hostapi.StreamTool{{ID: "t1", Name: "bash", Input: "go test ./internal/hostapi", Label: "Run the flush tests", Status: hostapi.CallRun}}}
	for _, c := range []struct {
		name    string
		live    bool
		profile colorprofile.Profile
		frames  []hostapi.RunFrame
	}{
		{"a plain log prints settled rows once", false, colorprofile.NoTTY, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, thought}, Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Pending: []hostapi.ChatMessage{failed}, Stream: running},
			{Settled: []hostapi.ChatMessage{failed, reply}},
		}},
		{"the live tail redraws in place and commits above itself", true, colorprofile.NoTTY, []hostapi.RunFrame{
			{Pending: []hostapi.ChatMessage{user}, Stream: running},
			{Settled: []hostapi.ChatMessage{user}, Stream: running},
			{Stream: running}, // unchanged: nothing written
			{Settled: []hostapi.ChatMessage{thought, failed, reply}},
		}},
		{"NO_COLOR keeps bold and drops color", false, colorprofile.ASCII, []hostapi.RunFrame{
			{Settled: []hostapi.ChatMessage{user, failed}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := bootAgentApp(t, 100, 30)
			db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			h.DB = db
			const sid = "5c0e9d1f8a4b4e2f9a3c7d6e1b2f4a8c"
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
			v.Start(h)
			for _, fr := range c.frames {
				v.Show(fr, false)
			}
			v.Close()
			golden.RequireEqual(t, out.String())
		})
	}
}
