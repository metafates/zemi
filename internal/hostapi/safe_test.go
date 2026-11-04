package hostapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// TestPanicsDoNotKillZemi: a panicking host goroutine is recovered into
// /errors, and a turn that panics leaves the agent idle, not stuck active.
func TestPanicsDoNotKillZemi(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	done := make(chan struct{})
	h.GoSafe("test", func() {
		defer close(done)
		panic("boom")
	})
	<-done
	waitFor(t, func() error {
		if !h.HasUnseenErrors() {
			return errors.New("panic not recorded")
		}
		return nil
	})
	if !strings.Contains(h.errs.entries[0].msg, "boom") {
		t.Fatalf("panic not recorded: %+v", h.errs.entries)
	}

	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.mu.Unlock()
	func() {
		defer h.endTurnAfterPanic()
		panic("turn exploded")
	}()
	if h.AgentActive() {
		t.Fatal("agent still active after a turn panic")
	}
	// a panic before the turn starts releases the claim the send holds
	h.modelFn = func() (fantasy.LanguageModel, error) { panic("model exploded") }
	if err := h.AgentSend("hi"); err == nil || h.AgentActive() {
		t.Fatalf("send %v, agent active %v", err, h.AgentActive())
	}
}

// TestPanickedHandleWait: a handle whose work panicked ends its wait with
// the failure instead of an empty reply or the cap, and the panic still
// lands in /errors.
func TestPanickedHandleWait(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, src string }{
		{"model", `local v, err = zemi.model.ask({ prompt = "ping" }):wait() zemi.state.set("err", tostring(err))`},
		{"agent", `local v, err = zemi.agent.run({ prompt = "ping" }):wait() zemi.state.set("err", tostring(err))`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			h.modelFn = func() (fantasy.LanguageModel, error) { panic("provider exploded") }
			done := make(chan error, 1)
			go func() { done <- chunkAs(t.Context(), h, r, lua.OriginUser, c.src) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the wait burned its cap instead of reading the panic")
			}
			if v, _ := h.GlobalDB.GetKV("err"); !strings.Contains(v, "panicked") {
				t.Errorf("wait err = %s", v)
			}
			waitFor(t, func() error {
				if !h.HasUnseenErrors() {
					return errors.New("the panic was not recorded")
				}
				return nil
			})
		})
	}
}

// TestCloseJoinsTheGoroutinesItStarted: Close waits for a goroutine GoSafe
// started to end, and a wedged one costs it closeWait, never the shutdown.
func TestCloseJoinsTheGoroutinesItStarted(t *testing.T) {
	old := _closeWait
	t.Cleanup(func() { _closeWait = old })
	for _, c := range []struct {
		name   string
		wedge  bool
		joined bool
	}{
		{name: "a goroutine in flight ends before Close returns", joined: true},
		{name: "a wedged goroutine costs closeWait and no more", wedge: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir()})
			release, ended := make(chan struct{}), make(chan struct{})
			h.GoSafe("test", func() {
				defer close(ended)
				<-release
			})
			_closeWait = 100 * time.Millisecond
			if c.wedge {
				t.Cleanup(func() {
					close(release)
					<-ended
				})
			} else {
				go func() {
					time.Sleep(20 * time.Millisecond)
					close(release)
				}()
			}
			begin := time.Now()
			h.Close()
			waited := time.Since(begin)
			joined := true
			select {
			case <-ended:
			default:
				joined = false
			}
			if joined != c.joined {
				t.Fatalf("the goroutine was joined after Close = %v, want %v", joined, c.joined)
			}
			min := 20 * time.Millisecond
			if c.wedge {
				min = _closeWait // gave up at the cap, not before
			}
			if waited < min {
				t.Fatalf("Close returned after %s, before the goroutine settled", waited)
			}
		})
	}
}

// TestFailedTurnLeavesErrorRow: a turn that fails ends with an error row in
// the transcript (and the agent idle); an interrupt reads as interrupted.
// Reasoning mid-step settles the finished call and keeps the open one live,
// which the end then marks failed, or stopped after an interrupt.
func TestFailedTurnLeavesErrorRow(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	sa := &streamAdapter{h: h}
	for _, c := range []struct {
		err          error
		text, status string
		call         string
		steered      bool // the turn's words came from the queue
	}{
		{errors.New("dial tcp: no route to host"), "Dial tcp: no route to host", "", "err", false},
		{context.Canceled, "Interrupted\nwhile the read call ran", "cancelled", "stopped", false},
		{context.Canceled, "Interrupted\nwhile the read call ran\nthe queued message was not answered", "cancelled", "stopped", true},
	} {
		h.agents.mu.Lock()
		h.agents.steeredTurn = c.steered
		h.agents.mu.Unlock()
		sa.ToolCallStart("a", "glob", `{"pattern":"*"}`)
		sa.ToolCallEnd("a", "glob", out("x.go"), "")
		sa.ToolCallPending("b", "read")
		sa.ReasoningDelta("then b")
		if tr := h.Session().Transcript; tr[len(tr)-1].Status != "ok" || len(h.agents.tools) != 1 {
			t.Fatalf("%v: reasoning settled an open call: live %+v", c.err, h.agents.tools)
		}
		sa.TurnDone(c.err, fantasy.Usage{})
		tr := h.Session().Transcript
		last := tr[len(tr)-1]
		if last.Kind != "error" || last.Text != c.text || string(last.Status) != c.status {
			t.Fatalf("%v: last row = %+v", c.err, last)
		}
		if call := tr[len(tr)-2]; call.Tool != tools.ToolRead || string(call.Status) != c.call {
			t.Fatalf("%v: open call = %+v", c.err, call)
		}
	}
}

// TestBuiltinSkillsShipEnabled: zemi's self-knowledge skill is written to
// the data dir, discovered as builtin and enabled by default.
func TestBuiltinSkillsShipEnabled(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	h.syncSkills()
	s, ok := h.skillsReg.get("zemi")
	if !ok || s.Source != "builtin" || !h.skillsReg.isEnabled("zemi") {
		t.Errorf("zemi: found=%v source=%q enabled=%v", ok, s.Source, h.skillsReg.isEnabled("zemi"))
	}
	if mine, _ := h.skillsIndexPrompt(); !strings.Contains(mine, "- zemi: ") {
		t.Fatal("the builtin skill is missing from the prompt index")
	}
}
