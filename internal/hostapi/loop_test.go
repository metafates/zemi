package hostapi

import (
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

// loopTest drives one /loop on a fake clock: the model answers every run
// with reply, and each step moves the clock and checks what the loop did.
type loopTest struct {
	t     *testing.T
	h     *Host
	clock *fakeClock
	runs  atomic.Int32
	start time.Time
}

// newLoopTest starts a loop on args whose runs answer reply: "fail" fails
// the run, "hang" waits for an interrupt.
func newLoopTest(t *testing.T, args, reply string) *loopTest {
	t.Helper()
	h, clock := newLoopHost(t)
	lt := &loopTest{t: t, h: h, clock: clock}
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			switch {
			case strings.HasSuffix(last, "hang"):
				return nil
			case !strings.Contains(last, "\n<loop>\nRun "):
				return textParts("ok")
			}
			lt.runs.Add(1)
			switch reply {
			case "fail":
				return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("boom")}}
			case "hang":
				return nil
			default:
				return textParts(reply)
			}
		}}, nil
	}
	if _, err := h.Commands.Add(&Command{Name: "note", NArgs: NArgsAny, Desc: "d", Origin: lua.OriginUser, GoFn: func(h *Host, args string, confirm bool) error {
		return h.AgentSend("/note" + args)
	}}); err != nil {
		t.Fatal(err)
	}
	lt.start = clock.Now()
	if err := h.SetLoop(args); err != nil {
		t.Fatal(err)
	}
	return lt
}

// step is what one move of the clock must show: the runs the model saw,
// whether a turn is held, and when the loop fires next (0 once it stopped).
type step struct {
	after time.Duration
	runs  int
	next  time.Duration
	held  bool
}

// advance fires the timers due within s.after, moves the clock past them and
// checks s.
func (lt *loopTest) advance(s step) {
	lt.t.Helper()
	until := lt.clock.Now().Add(s.after)
	for f, ok := lt.clock.due(until); ok; f, ok = lt.clock.due(until) {
		f()
		// the fire armed the next timer or stopped the loop
		waitFor(lt.t, func() error {
			if _, on := lt.h.Loop(); on && lt.clock.armed() == 0 {
				return errors.New("the fire has not run")
			}
			return nil
		})
	}
	lt.clock.set(until)
	waitFor(lt.t, func() error {
		if n := int(lt.runs.Load()); n != s.runs || lt.h.AgentActive() != s.held {
			return fmt.Errorf("%d runs, active %v", n, lt.h.AgentActive())
		}
		return nil
	})
	l, on := lt.h.Loop()
	if on != (s.next > 0) {
		// the interrupt's stop runs beside waitIdle's release: wait for the
		// settled loop, never assert the moving state
		waitForWithin(lt.t, 10*time.Second, func() error {
			l, on = lt.h.Loop()
			if on != (s.next > 0) {
				return fmt.Errorf("loop on %v, want %v", on, s.next > 0)
			}
			return nil
		})
	}
	if on {
		if next := l.Next.Sub(lt.start); next != s.next {
			lt.t.Fatalf("loop next at +%s; want +%s", next, s.next)
		}
	}
}

// wantNotices asserts the loop's rows in the transcript, in order.
func (lt *loopTest) wantNotices(want ...string) {
	lt.t.Helper()
	waitFor(lt.t, func() error {
		var notices []string
		for _, m := range lt.h.Session().Transcript {
			if m.Kind == RowNotice && (strings.HasPrefix(m.Text, "loop  Run ") || strings.HasPrefix(m.Text, "Loop ")) {
				notices = append(notices, m.Text)
			}
		}
		if !slices.Equal(notices, want) {
			return fmt.Errorf("notices = %q, want %q", notices, want)
		}
		return nil
	})
}

// wantEnded asserts the why the loop stopped with.
func (lt *loopTest) wantEnded(want string) {
	lt.t.Helper()
	waitFor(lt.t, func() error {
		if got := lt.h.LoopEndedWhy(); got != want {
			return fmt.Errorf("loop ended why = %q, want %q", got, want)
		}
		return nil
	})
}

// TestLoopFixed: a fixed interval runs now and again on its interval.
func TestLoopFixed(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check the deploy", "rolling")
	lt.advance(step{runs: 1, next: 5 * time.Minute})
	lt.advance(step{after: 5 * time.Minute, runs: 2, next: 10 * time.Minute})
	lt.advance(step{after: 5 * time.Minute, runs: 3, next: 15 * time.Minute})
	lt.wantNotices("loop  Run 1: check the deploy", "loop  Run 2: check the deploy", "loop  Run 3: check the deploy")
}

// TestLoopPaced: the model's next line paces the loop.
func TestLoopPaced(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "check it", "waiting\n`next: 7m`")
	lt.advance(step{runs: 1, next: 7 * time.Minute})
	lt.advance(step{after: 6 * time.Minute, runs: 1, next: 7 * time.Minute})
	lt.advance(step{after: time.Minute, runs: 2, next: 14 * time.Minute})
}

// TestLoopPacedFallback: a paced run with no next line falls back.
func TestLoopPacedFallback(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "check it", "hm")
	lt.advance(step{runs: 1, next: loopFallback})
}

// TestLoopPacedDelayCap: a paced delay past an hour is an hour.
func TestLoopPacedDelayCap(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "check it", "next: 2h")
	lt.advance(step{runs: 1, next: time.Hour})
}

// TestLoopDone: next: done ends it.
func TestLoopDone(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "check it", "all good\nnext: done")
	lt.advance(step{runs: 1})
	lt.wantNotices("loop  Run 1: check it", "Loop done after 1 run")
	lt.wantEnded("Loop done after 1 run")
}

// TestLoopFailed: a failed run stops it.
func TestLoopFailed(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check it", "fail")
	lt.advance(step{runs: 1})
	lt.wantNotices("loop  Run 1: check it", "Loop stopped: boom (1 run)")
	lt.wantEnded("Loop stopped: boom (1 run)")
}

// TestLoopInterrupted: an interrupted run stops it.
func TestLoopInterrupted(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check it", "hang")
	lt.advance(step{runs: 1, next: 5 * time.Minute, held: true})
	lt.h.AgentCancel()
	lt.advance(step{runs: 1})
	lt.wantNotices("loop  Run 1: check it", "Loop stopped: interrupted (1 run)")
	lt.wantEnded("Loop stopped: interrupted (1 run)")
}

// TestLoopReplacedInterrupted: an interrupt stops the loop that replaced the
// run's.
func TestLoopReplacedInterrupted(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check a", "hang")
	lt.advance(step{runs: 1, next: 5 * time.Minute, held: true})
	if err := lt.h.SetLoop("5m check b"); err != nil {
		t.Fatal(err)
	}
	if got := lt.h.LoopEndedWhy(); got != "" {
		t.Fatalf("the replaced loop kept the old why: %s", got)
	}
	waitFor(t, func() error {
		if lt.clock.armed() == 0 {
			return errors.New("b has not tried to run")
		}
		return nil
	})
	lt.h.AgentCancel()
	waitIdle(t, lt.h)
	lt.advance(step{runs: 1})
	lt.wantNotices("loop  Run 1: check a", "Loop stopped: interrupted (0 runs)")
}

// TestLoopInterruptedTurn: a turn nobody typed into, interrupted, stops it.
func TestLoopInterruptedTurn(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check it", "ok")
	lt.advance(step{runs: 1, next: 5 * time.Minute})
	if err := lt.h.AgentSend(agent.AttachMarker + "\nhang"); err != nil {
		t.Fatal(err)
	}
	lt.advance(step{runs: 1, next: 5 * time.Minute, held: true})
	lt.h.AgentCancel()
	lt.advance(step{runs: 1})
	lt.wantNotices("loop  Run 1: check it", "Loop stopped: interrupted (1 run)")
}

// TestLoopKeepsGoalRuns: a loop run leaves the goal's count of turns alone.
func TestLoopKeepsGoalRuns(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check it", "ok")
	lt.advance(step{runs: 1, next: 5 * time.Minute})
	lt.h.agents.mu.Lock()
	lt.h.agents.goalRuns = 7
	lt.h.agents.mu.Unlock()
	lt.advance(step{after: 5 * time.Minute, runs: 2, next: 10 * time.Minute})
	lt.h.agents.mu.Lock()
	goalRuns := lt.h.agents.goalRuns
	lt.h.agents.mu.Unlock()
	if goalRuns != 7 {
		t.Fatalf("a loop run reset the goal's count to %d", goalRuns)
	}
}

// TestLoopBusyAgent: a busy agent delays a run, keeping its time.
func TestLoopBusyAgent(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m check it", "ok")
	lt.advance(step{runs: 1, next: 5 * time.Minute})
	if err := lt.h.AgentSend("hang"); err != nil {
		t.Fatal(err)
	}
	lt.advance(step{after: 5 * time.Minute, runs: 1, next: 5 * time.Minute, held: true})
	lt.h.AgentCancel()
	waitIdle(t, lt.h)
	lt.advance(step{after: loopRetry, runs: 2, next: 5*time.Minute + loopRetry + 5*time.Minute})
}

// TestLoopExpires: a day's loop stops after 72 hours.
func TestLoopExpires(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "1d check it", "ok")
	lt.advance(step{runs: 1, next: 24 * time.Hour})
	lt.advance(step{after: 24 * time.Hour, runs: 2, next: 48 * time.Hour})
	lt.advance(step{after: 24 * time.Hour, runs: 3, next: 72 * time.Hour})
	lt.advance(step{after: 24 * time.Hour, runs: 3})
	lt.wantNotices("loop  Run 1: check it", "loop  Run 2: check it", "loop  Run 3: check it", "Loop stopped after 72h (3 runs)")
}

// TestLoopCommand: a command runs with the note attached.
func TestLoopCommand(t *testing.T) {
	t.Parallel()
	lt := newLoopTest(t, "5m /note it", "ok")
	lt.advance(step{runs: 1, next: 5 * time.Minute})
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
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	h.SetUIHooks(&fakeUI{})
	return h, clock
}

// TestSetLoopRefuses: what /loop refuses before anything runs.
func TestSetLoopRefuses(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	for _, c := range []struct{ args, want string }{
		{"30s check", "30s is under the 1m floor; a loop runs every 1m to 24h"},
		{"48h check", "48h is over the 24h ceiling; a longer wait belongs to cron"},
		{"2d check", "48h is over the 24h ceiling"},
		{"5m", "/loop 5m <prompt>"},
		{"5m /loop 1m x", "cannot run /loop"},
		{"/goal the tests pass", "cannot run /goal"},
		{"stop", "no loop to stop"},
	} {
		if err := h.SetLoop(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("/loop %s: %v, want %q", c.args, err, c.want)
		}
	}
	// a bare /loop answers in a box that stays, not a toast
	if err := h.SetLoop(""); err != nil {
		t.Errorf("/loop : %v, want the empty overlay", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(ui.opened) != 1 || ui.opened[0].Empty == "" {
		t.Fatalf("/loop with no loop opened %+v, want the empty-state overlay", ui.opened)
	}
}

// TestLoopEveryAlias: "/loop every 5m x" is "/loop 5m x"; an interval under
// the floor still refuses instead of silently model-pacing the line.
func TestLoopEveryAlias(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	if err := h.SetLoop("every 5m count rivers"); err != nil {
		t.Fatal(err)
	}
	l, on := h.Loop()
	if !on || l.Every != 5*time.Minute || l.Prompt != "count rivers" {
		t.Fatalf("every 5m armed %+v", l)
	}
	if err := h.SetLoop("every 30s check"); err == nil || !strings.Contains(err.Error(), "every 1m to 24h") {
		t.Errorf("/loop every 30s: %v, want the floor refusal", err)
	}
}

// TestLoopBrokenDuration: a first word that reads as a duration but parses
// as none ("5x") is refused, not taken as a prompt a silent loop runs.
func TestLoopBrokenDuration(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	if err := h.SetLoop("5x do something"); err == nil || !strings.Contains(err.Error(), "is not a duration") {
		t.Errorf("/loop 5x do something: %v, want the duration refusal", err)
	}
	if _, on := h.Loop(); on {
		t.Error("the broken duration started a loop")
	}
}

// TestLoopStopNamesItsRuns: the manual stop's notice counts runs, like
// every other stop path.
func TestLoopStopNamesItsRuns(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	h.agents.mu.Lock()
	h.claim() // hold the agent: the first fire retries on a clock that never ticks, so no run lands before the stop
	h.agents.mu.Unlock()
	if err := h.SetLoop("5m count rivers"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetLoop("stop"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if got := h.LoopEndedWhy(); got != "Loop stopped (0 runs)  0 tokens" {
			return fmt.Errorf("ended why = %q", got)
		}
		return nil
	})
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
