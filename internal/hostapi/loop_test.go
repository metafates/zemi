package hostapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// TestLoop drives /loop on a fake clock: each step runs its action, moves
// the clock and waits for what fired; then it checks how many runs the
// model saw, whether the loop is still on and when it fires next.
func TestLoop(t *testing.T) {
	type step struct {
		do    func(h *Host)
		after time.Duration
		runs  int
		next  time.Duration // from the start; 0 once the loop stopped
		held  bool          // a turn is left running
	}
	for _, c := range []struct {
		name    string
		args    string
		reply   string // every loop run's reply; "fail" fails it, "hang" waits for an interrupt
		steps   []step
		notices []string
	}{
		{name: "fixed runs now and on its interval", args: "5m check the deploy", reply: "rolling", steps: []step{
			{runs: 1, next: 5 * time.Minute}, {after: 5 * time.Minute, runs: 2, next: 10 * time.Minute}, {after: 5 * time.Minute, runs: 3, next: 15 * time.Minute},
		}, notices: []string{"Loop run 1: check the deploy", "Loop run 2: check the deploy", "Loop run 3: check the deploy"}},
		{name: "the model paces it", args: "check it", reply: "waiting\n`next: 7m`", steps: []step{
			{runs: 1, next: 7 * time.Minute}, {after: 6 * time.Minute, runs: 1, next: 7 * time.Minute}, {after: time.Minute, runs: 2, next: 14 * time.Minute},
		}},
		{name: "a paced run with no next line falls back", args: "check it", reply: "hm", steps: []step{{runs: 1, next: loopFallback}}},
		{name: "a paced delay past an hour is an hour", args: "check it", reply: "next: 2h", steps: []step{{runs: 1, next: time.Hour}}},
		{
			name: "next: done ends it", args: "check it", reply: "all good\nnext: done", steps: []step{{runs: 1}},
			notices: []string{"Loop run 1: check it", "Loop done after 1 run"},
		},
		{
			name: "a failed run stops it", args: "5m check it", reply: "fail", steps: []step{{runs: 1}},
			notices: []string{"Loop run 1: check it", "Loop stopped: boom (1 run)"},
		},
		{name: "an interrupted run stops it", args: "5m check it", reply: "hang", steps: []step{{runs: 1, next: 5 * time.Minute, held: true}, {do: func(h *Host) {
			h.AgentCancel()
		}, runs: 1}}, notices: []string{"Loop run 1: check it", "Loop stopped: interrupted (1 run)"}},
		{name: "an interrupt stops the loop that replaced the run's", args: "5m check a", reply: "hang", steps: []step{
			{runs: 1, next: 5 * time.Minute, held: true},
			{do: func(h *Host) {
				if err := h.SetLoop("5m check b"); err != nil {
					panic(err)
				}
				waitFor(t, func() error {
					if h.RT.Clock().(*fakeClock).armed() == 0 {
						return errors.New("b has not tried to run")
					}
					return nil
				})
				h.AgentCancel()
				waitIdle(t, h)
			}, runs: 1},
		}, notices: []string{"Loop run 1: check a", "Loop stopped: interrupted (0 runs)"}},
		{name: "an interrupted turn nobody typed stops it", args: "5m check it", reply: "ok", steps: []step{
			{runs: 1, next: 5 * time.Minute},
			{do: func(h *Host) {
				if err := h.AgentSend(agent.AttachMarker + "\nhang"); err != nil {
					panic(err)
				}
			}, runs: 1, next: 5 * time.Minute, held: true},
			{do: func(h *Host) { h.AgentCancel() }, runs: 1},
		}, notices: []string{"Loop run 1: check it", "Loop stopped: interrupted (1 run)"}},
		{name: "a run leaves the goal's count of turns", args: "5m check it", reply: "ok", steps: []step{
			{runs: 1, next: 5 * time.Minute},
			{do: func(h *Host) {
				h.agents.mu.Lock()
				h.agents.goalRuns = 7
				h.agents.mu.Unlock()
			}, after: 5 * time.Minute, runs: 2, next: 10 * time.Minute},
			{do: func(h *Host) {
				h.agents.mu.Lock()
				defer h.agents.mu.Unlock()
				if h.agents.goalRuns != 7 {
					panic(fmt.Sprintf("a loop run reset the goal's count to %d", h.agents.goalRuns))
				}
			}, runs: 2, next: 10 * time.Minute},
		}},
		{name: "a busy agent delays a run, keeping its time", args: "5m check it", reply: "ok", steps: []step{
			{runs: 1, next: 5 * time.Minute},
			{do: func(h *Host) {
				if err := h.AgentSend("hang"); err != nil {
					panic(err)
				}
			}, after: 5 * time.Minute, runs: 1, next: 5 * time.Minute, held: true},
			{do: func(h *Host) { h.AgentCancel(); waitIdle(t, h) }, after: loopRetry, runs: 2, next: 5*time.Minute + loopRetry + 5*time.Minute},
		}},
		{name: "it expires", args: "1d check it", reply: "ok", steps: []step{
			{runs: 1, next: 24 * time.Hour}, {after: 24 * time.Hour, runs: 2, next: 48 * time.Hour}, {after: 24 * time.Hour, runs: 3, next: 72 * time.Hour}, {after: 24 * time.Hour, runs: 3},
		}, notices: []string{"Loop run 1: check it", "Loop run 2: check it", "Loop run 3: check it", "Loop stopped after 72h (3 runs)"}},
		{name: "a new session stops it quietly", args: "5m check it", reply: "ok", steps: []step{{runs: 1, next: 5 * time.Minute}, {do: func(h *Host) {
			if err := h.NewSession(); err != nil {
				panic(err)
			}
		}, runs: 1}}},
		{name: "a command runs with the note attached", args: "5m /note it", reply: "ok", steps: []step{{runs: 1, next: 5 * time.Minute}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, clock := newLoopHost(t)
			var runs atomic.Int32
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
					switch {
					case strings.HasSuffix(last, "hang"):
						return nil
					case !strings.Contains(last, "\n<loop>\nRun "):
						return textParts("ok")
					}
					runs.Add(1)
					switch c.reply {
					case "fail":
						return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("boom")}}
					case "hang":
						return nil
					}
					return textParts(c.reply)
				}}, nil
			}
			if _, err := h.Commands.Add(&Command{Name: "note", NArgs: NArgsAny, Desc: "d", Origin: lua.OriginUser, GoFn: func(h *Host, args string) error {
				return h.AgentSend("/note" + args)
			}}); err != nil {
				t.Fatal(err)
			}
			start := clock.Now()
			if err := h.SetLoop(c.args); err != nil {
				t.Fatal(err)
			}
			for i, s := range c.steps {
				if s.do != nil {
					s.do(h)
				}
				until := clock.Now().Add(s.after)
				for f, ok := clock.due(until); ok; f, ok = clock.due(until) {
					f()
					// the fire armed the next timer or stopped the loop
					waitFor(t, func() error {
						if _, on := h.Loop(); on && clock.armed() == 0 {
							return errors.New("the fire has not run")
						}
						return nil
					})
				}
				clock.set(until)
				waitFor(t, func() error {
					if n := int(runs.Load()); n != s.runs || h.AgentActive() != s.held {
						return fmt.Errorf("step %d: %d runs, active %v", i, n, h.AgentActive())
					}
					return nil
				})
				l, on := h.Loop()
				if next := l.Next.Sub(start); on != (s.next > 0) || on && next != s.next {
					t.Fatalf("step %d: loop on %v, next at +%s; want +%s", i, on, next, s.next)
				}
			}
			var notices []string
			for _, m := range h.Session().Transcript {
				if m.Kind == RowNotice && strings.HasPrefix(m.Text, "Loop ") {
					notices = append(notices, m.Text)
				}
			}
			if c.notices != nil && !slices.Equal(notices, c.notices) {
				t.Errorf("notices = %q, want %q", notices, c.notices)
			}
		})
	}
}

// armed counts the timers waiting on the clock.
func (c *fakeClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func newLoopHost(t *testing.T) (*Host, *fakeClock) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	clock := &fakeClock{now: testEpoch}
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, Clock: clock})
	h := NewHost(r, log, config.NewStore(), config.Dirs{StateHome: t.TempDir(), ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	h.SetUIHooks(&fakeUI{})
	return h, clock
}

// TestSetLoopRefuses: what /loop refuses before anything runs.
func TestSetLoopRefuses(t *testing.T) {
	h, _ := newLoopHost(t)
	for _, c := range []struct{ args, want string }{
		{"30s check", "every 1m to 24h"},
		{"48h check", "every 1m to 24h; a longer wait belongs to cron"},
		{"2d check", "every 1m to 24h"},
		{"5m", "/loop 5m <prompt>"},
		{"5m /loop 1m x", "cannot run /loop"},
		{"/goal the tests pass", "cannot run /goal"},
		{"stop", "no loop to stop"},
		{"", "no loop;"},
	} {
		if err := h.SetLoop(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("/loop %s: %v, want %q", c.args, err, c.want)
		}
	}
}

func TestParseNext(t *testing.T) {
	for _, c := range []struct {
		reply    string
		d        time.Duration
		done, ok bool
	}{
		{"still going\nnext: 5m", 5 * time.Minute, false, true},
		{"`next: 30s`", loopMin, false, true},
		{"NEXT: done", 0, true, true},
		{"next: soon", 0, false, false},
		{"no line", 0, false, false},
		{"next: 5m\nand then some", 0, false, false},
	} {
		d, done, ok := parseNext(c.reply)
		if d != c.d || done != c.done || ok != c.ok {
			t.Errorf("%q: %s %v %v", c.reply, d, done, ok)
		}
	}
}
