package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// echoAgents stubs every subagent to answer "<prompt>!", recording the
// highest number of agents running at once. The parallel case's first
// branches (p1 to p4) wait for a second agent, so they overlap under any load.
func echoAgents(h *Host) *atomic.Int32 {
	var running, peak atomic.Int32
	two := make(chan struct{})
	closeTwo := sync.OnceFunc(func() { close(two) })
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			// the task ends where the scratchpad note begins
			if at := strings.Index(last, "Other agents share the scratchpad"); at >= 0 {
				last = strings.TrimRight(last[:at], "\n")
			}
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			if n >= 2 {
				closeTwo()
			}
			if len(last) == 2 && last[0] == 'p' {
				select {
				case <-two:
				case <-time.After(10 * time.Second): // branches run one at a time: the peak stays 1
				}
			}
			running.Add(-1)
			if last == "panic" {
				panic("agent boom")
			}
			if last == "fail" {
				return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: context.DeadlineExceeded}}
			}
			return textParts(last + "!")
		}}, nil
	}
	return &peak
}

// runFlow runs src in the foreground, waiting for its end; a ctx that ends
// first cancels the run, which is the stop path's shape - a call hands off.
func runFlow(ctx context.Context, h *Host, src string, args any) (string, error) {
	f, err := h.startFlow(ctx, flowSpec{src: src, args: args})
	if err != nil {
		return "", err
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		f.cancel()
		<-f.done
	}
	return f.result, f.err
}

// TestWorkflowCancelBoundedDrain: an agent that never notices the cancel
// must not hold the run open past the drain grace. The run checks that a
// cancel ends it; the bubble checks the bound.
func TestWorkflowCancelBoundedDrain(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	running := make(chan struct{}, 1)
	// the agent never returns on its own, so the run ending is the drain
	// giving up; wedged lets it end with the test
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
			running <- struct{}{}
			<-wedged
			return nil
		}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := runFlow(ctx, h, `zemi.workflow.agent("x")`, nil)
		done <- err
	}()
	// cancelled only once the agent runs: a cancel that beats it tests nothing
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("the run ended before its agent ran: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled workflow returned no error")
		}
	case <-time.After(time.Minute):
		t.Fatal("the run waited on an agent that ignores cancellation")
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		(&wfRun{running: 1}).drainAgents()
		if d := time.Since(start); d != _workflowDrainGrace {
			t.Fatalf("drained for %v, want %v", d, _workflowDrainGrace)
		}
	})
}

func TestWorkflowRun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	peak := echoAgents(h)
	for _, c := range []struct {
		name, script string
		args         any
		want         string // result, or the error substring when wantErr
		wantErr      bool
		raw          bool // script runs as written: no local wf prepended
	}{
		{name: "sequential chain", script: `local a = wf.agent("x") return wf.agent(a .. "y")`, want: "x!y!"},
		{
			name: "parallel branches keep order and chain independently",
			script: `
				local jobs = {}
				for i = 1, 4 do
					jobs[i] = function() return wf.agent(wf.agent("p" .. i)) end
				end
				local out = wf.parallel(jobs)
				return table.concat(out, ",")`,
			want: "p1!!,p2!!,p3!!,p4!!",
		},
		{
			name:   "args and zemi.json round-trip",
			script: `local t = zemi.json.decode(zemi.json.encode(wf.args)) return {n = t.n + 1}`,
			args:   map[string]any{"n": 1},
			want:   "{\n  \"n\": 2\n}",
		},
		{
			name:   "no args reads nil, so the builtins' guard holds",
			script: `if type(wf.args) == "string" then return wf.args end return "no args"`,
			want:   "no args",
		},
		{
			name:   "pipeline keeps item order and passes item and index",
			script: `return table.concat(wf.pipeline({"a", "b"}, function(x) return wf.agent(x) end, function(r, it, i) return r .. it .. i end), ",")`,
			want:   "a!a1,b!b2",
		},
		{
			name:   "a pipeline stage's nil ends that item quietly, its nil, err lands in errs",
			script: `local out, errs = wf.pipeline({"a", "b", "fail"}, function(x) if x ~= "b" then return wf.agent(x) end end, function(r) return r .. "+" end) return tostring(out[1]) .. " " .. tostring(out[2]) .. " " .. tostring(errs[2]) .. " " .. tostring(errs[3] ~= nil)`,
			want:   "a!+ nil nil true",
		},
		{
			name:   "failed branch reports through errs without a traceback",
			script: `local out, errs = wf.parallel({function() return wf.agent("ok") end, function() local r, e = wf.agent("fail") return r or error(e) end}) return out[1] .. " " .. tostring(errs ~= nil and errs[2] ~= nil and not errs[2]:find("traceback", 1, true))`,
			want:   "ok! true",
		},
		{
			name:   "a branch returning agent's nil, err reports through errs",
			script: `local out, errs = wf.parallel({function() return wf.agent("ok") end, function() return wf.agent("fail") end}) return out[1] .. " " .. tostring(errs ~= nil and errs[2] ~= nil)`,
			want:   "ok! true",
		},
		{name: "crashed agent", script: `local r, e = wf.agent("panic") return e`, want: "the agent crashed"},
		{name: "agent inside pcall refused", script: `local ok, err = pcall(wf.agent, "x") return tostring(ok) .. " " .. err`, want: "false"},
		{name: "pcall without a function", script: `return pcall()`, wantErr: true, want: "bad argument #1"},
		{name: "agent in a library callback", script: `local t = {3, 1, 2} table.sort(t, function(a, b) wf.agent("x") return a < b end) return "sorted"`, wantErr: true, want: "cannot wait"},
		{name: "many instant parallel calls", script: `local n = 0 for i = 1, 20000 do n = n + wf.parallel({function() return 1 end})[1] end return tostring(n)`, want: "20000"},
		{name: "parallel over the agent cap", script: `local t = {} for i = 1, 101 do t[i] = function() return 1 end end return wf.parallel(t)`, wantErr: true, want: "more than the"},
		{name: "unknown agent", script: `return wf.agent("x", {agent = "nope"})`, wantErr: true, want: "unknown agent"},
		{name: "a wrong-typed option raises naming it", script: `return wf.agent("x", {phase = 1})`, wantErr: true, want: "phase must be a string"},
		{name: "an unknown option names the ones there are", script: `return wf.agent("x", {agnt = "explore"})`, wantErr: true, want: `unknown option "agnt"; have agent, description, model, phase, schema`},
		{name: "bare agent points at zemi.workflow", script: `return agent("x")`, wantErr: true, want: "has no agent, write zemi.workflow.agent"},
		{name: "print is gone", script: `print("x")`, wantErr: true, want: "has no print, write zemi.workflow.log"},
		{name: "coroutine.yield points at the suspenders", script: `coroutine.yield(1)`, wantErr: true, want: "has no coroutine, write zemi.workflow.agent or zemi.workflow.parallel"},
		{name: "a misspelled member names the table", script: `return wf.agnt({})`, wantErr: true, want: "has no agnt; zemi.workflow has agent, parallel, pipeline, phase, log, args"},
		{name: "wf itself is not given", script: `return wf.agent("x")`, raw: true, wantErr: true, want: "has no wf, write zemi.workflow"},
		{name: "parallel functions as arguments name the shape", script: `return wf.parallel(function() return 1 end, function() return 1 end)`, wantErr: true, want: "parallel: pass the branches as one table: parallel({fn, ...})"},
		{name: "parallel raises at the script's line when it is on the stack", script: `local r = wf.parallel({function() return 1 end, "x"}) return type(r)`, wantErr: true, want: ".lua:2: parallel: item 2 is not a function"},
		{name: "pipeline stages as one table name the shape", script: `return wf.pipeline({"a"}, {function(a) return a end})`, wantErr: true, want: "pipeline: pass the stages as arguments, not one table: pipeline(items, stage, ...)"},
		{name: "pipeline with the items second names the shape", script: `return wf.pipeline(function(a) return a end, {"a"})`, wantErr: true, want: "pipeline: pass the items as one table first: pipeline(items, stage, ...)"},
		{name: "pipeline with a stage that is not a function names it", script: `return wf.pipeline({"a"}, function(a) return a end, "x")`, wantErr: true, want: "pipeline: stage 2 is not a function"},
		{name: "agent options as a string name the shape", script: `return wf.agent("x", "explore")`, wantErr: true, want: "agent: the argument must be a table, not a string"},
		{name: "agent prompt as a table names the shape", script: `return wf.agent({prompt = "x"})`, wantErr: true, want: "agent: the argument must be a string, not a table"},
		{name: "no other zemi namespace", script: `return zemi.fs.read("x")`, wantErr: true, want: "attempt to index"},
		{name: "syntax error", script: `return (`, wantErr: true, want: "workflow script"},
		{name: "a script error after an agent", script: `local x = wf.agent("quick") local t = nil return t.field`, wantErr: true, want: "attempt to index"},
		{name: "string.rep over the cap", script: `return ("x"):rep(1e13)`, wantErr: true, want: "byte cap"},
		{name: "string.rep under the cap", script: `return string.rep("ab", 2) .. string.rep("x", -1)`, want: "abab"},
		{name: "no file access", script: `return dofile("/etc/passwd")`, wantErr: true, want: "non-function"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := c.script
			if !c.raw {
				src = "local wf = zemi.workflow\n" + src
			}
			res, err := runFlow(t.Context(), h, src, c.args)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res, c.want) {
				t.Fatalf("result = %q, want %q", res, c.want)
			}
		})
	}
	if peak.Load() < 2 {
		t.Fatalf("parallel agents never overlapped (peak %d)", peak.Load())
	}
}

// TestWorkflowDescNamesNoBareCall: the tool description is the API a model
// writes the script against, so every call in it carries the wf. prefix the
// guard's names demand; a bare one is what a model copies verbatim.
func TestWorkflowDescNamesNoBareCall(t *testing.T) {
	t.Parallel()
	for name := range _misnamed {
		if bare := regexp.MustCompile(`(^|[^.\w])` + name + `\(`); bare.MatchString(_workflowToolDesc) {
			t.Errorf("workflowToolDesc calls %s() bare, not wf.%s()", name, name)
		}
	}
}

// TestWorkflowCallWarns: a foreground workflow call whose run finished with a
// failed agent inside answers under a mark that warns, so its row reads the
// run's own outcome rather than a check; a run that only succeeded keeps its
// check.
func TestWorkflowCallWarns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	tool := h.workflowTool(h.PipelineFor())
	call := func(script string) string {
		t.Helper()
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c1", Name: string(tools.ToolWorkflow), Input: `{"script":` + strconv.Quote("local wf = zemi.workflow\n"+script) + `}`})
		if err != nil {
			t.Fatal(err)
		}
		if resp.IsError {
			t.Fatalf("a run that finished answered an error: %q", resp.Content)
		}
		return resp.Content
	}
	warned := call(`local r, e = wf.agent("fail") return "the run went on"`)
	if got := toolStatus(fantasy.ToolResultOutputContentText{Text: warned}, false, tools.Outcome{Warn: true}); got != CallWarn {
		t.Fatalf("a call whose run warned reads %s: %q", got, warned)
	}
	clean := call(`wf.agent("ok") return "done"`)
	if got := toolStatus(fantasy.ToolResultOutputContentText{Text: clean}, false, tools.Outcome{}); got != CallOK {
		t.Fatalf("a call whose run finished reads %s: %q", got, clean)
	}
}

// gateModel holds every agent until release lets one answer "<prompt>!",
// or its context ends it; calls counts the calls by prompt. The prompt is
// the task's first line: the scratchpad line runAgent adds names the run.
type gateModel struct {
	scriptModel

	release chan struct{}
	running atomic.Int32
	mu      sync.Mutex
	calls   map[string]int
}

func newGate() *gateModel {
	m := &gateModel{release: make(chan struct{}), calls: map[string]int{}}
	m.script = func(_, last string, _ bool) []fantasy.StreamPart {
		prompt, _, _ := strings.Cut(last, "\n")
		m.mu.Lock()
		m.calls[prompt]++
		m.mu.Unlock()
		return textParts(prompt + "!")
	}
	return m
}

func (m *gateModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	resp, err := m.scriptModel.Stream(ctx, call)
	m.running.Add(1)
	defer m.running.Add(-1)
	select {
	case <-m.release:
		return resp, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *gateModel) count(prompt string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[prompt]
}

// TestWorkflowControl drives flowControl against agents the test holds. A
// tick is the scheduler's report after it handled an event, so a state
// seen there is one no later dispatch in that step changed.
func TestWorkflowControl(t *testing.T) {
	t.Parallel()
	type ctl struct {
		t     *testing.T
		h     *Host
		m     *gateModel
		f     *flowRun
		ticks chan flowView
	}
	// tick waits for the scheduler to report a state cond accepts
	tick := func(c ctl, cond func(flowView) bool) flowView {
		c.t.Helper()
		for {
			select {
			case v := <-c.ticks:
				if cond(v) {
					return v
				}
			case <-time.After(15 * time.Second):
				c.t.Fatal("the run never reached the state")
			}
		}
	}
	act := func(c ctl, a flowAction, n int, agent string) string {
		c.t.Helper()
		out, err := c.h.flowControl(c.f.id, a, n, agent)
		if err != nil {
			c.t.Fatal(err)
		}
		return out
	}
	running := func(n int32) func(c ctl) {
		return func(c ctl) {
			waitFor(c.t, func() error {
				if got := c.m.running.Load(); got != n {
					return fmt.Errorf("agents running %d, want %d", got, n)
				}
				return nil
			})
		}
	}
	for _, c := range []struct {
		name, script string
		limit        int
		drive        func(c ctl)
		want         string // the result, or the error's text
	}{
		{
			name:   "pause holds a queued agent that blocks reset; resume starts it with the flow's session as parent",
			script: `return wf.agent("a") .. wf.agent("b")`,
			drive: func(c ctl) {
				running(1)(c)
				act(c, _actPause, 0, "")
				c.m.release <- struct{}{}
				v := tick(c, func(v flowView) bool { return len(v.agents) == 2 })
				if v.agents[1].status != "queued" || v.running != 0 {
					c.t.Fatalf("paused run started %+v", v.agents[1])
				}
				if c.h.tasks.stillRunning(func(string) bool { return true }) == nil {
					c.t.Fatal("a paused workflow does not block a reset")
				}
				act(c, _actResume, 0, "")
				running(1)(c)
				if p := c.h.tasks.find(c.f.view().agents[1].id).parent; p != store.MainSessionID {
					c.t.Fatalf("the agent started after /new has parent %q", p)
				}
				c.m.release <- struct{}{}
			},
			want: "a!b!",
		},
		{
			name:   "raising the concurrency starts the queued agents",
			script: `return table.concat(wf.parallel({function() return wf.agent("a") end, function() return wf.agent("b") end, function() return wf.agent("c") end}))`,
			limit:  1,
			drive: func(c ctl) {
				running(1)(c)
				act(c, _actConcurrency, 3, "")
				running(3)(c)
				for range 3 {
					c.m.release <- struct{}{}
				}
			},
			want: "a!b!c!",
		},
		{
			name:   "stopping one agent hands the script nil and the reason",
			script: `local r, err = wf.agent("a") return tostring(r) .. " " .. err`,
			drive: func(c ctl) {
				running(1)(c)
				if err := c.h.stopRun(c.f.view().agents[0].id); err != nil {
					c.t.Fatal(err)
				}
			},
			want: "nil stopped by the user",
		},
		{
			name:   "stopping one agent hands the script nil and the reason",
			script: `local r, err = wf.agent("a") return tostring(r) .. " " .. err`,
			drive: func(c ctl) {
				running(1)(c)
				if err := c.h.stopRun(c.f.view().agents[0].id); err != nil {
					c.t.Fatal(err)
				}
			},
			want: "nil stopped by the user",
		},
		{
			name:   "stopping the run names it by its description",
			script: `local r = wf.agent("a") return r`,
			drive: func(c ctl) {
				running(1)(c)
				if got := act(c, _actStop, 0, ""); got != "Stopped "+c.f.description+" ("+c.f.id+")" {
					c.t.Fatalf("stop = %q", got)
				}
			},
			want: "context canceled",
		},
		{
			name:   "a restart runs the call again, the script sees only the second answer, and the row keeps the first attempt's spend",
			script: `return wf.agent("a")`,
			drive: func(c ctl) {
				running(1)(c)
				first := c.h.tasks.find(c.f.view().agents[0].id)
				first.mu.Lock()
				first.tokens = 110
				first.mu.Unlock()
				act(c, _actRestart, 0, first.id)
				waitFor(c.t, func() error {
					if n := c.m.count("a"); n != 2 {
						return fmt.Errorf("calls %d", n)
					}
					return nil
				})
				running(1)(c)
				if v := c.f.view(); v.tokens != 110 || v.agents[0].tokens != 110 {
					c.t.Fatalf("after the restart the run shows %d tokens, its row %d", v.tokens, v.agents[0].tokens)
				}
				c.m.release <- struct{}{}
			},
			want: "a!",
		},
		{
			name:   "a restart in a paused run reads as queued until resumed",
			script: `return wf.agent("a")`,
			drive: func(c ctl) {
				running(1)(c)
				act(c, _actPause, 0, "")
				act(c, _actRestart, 0, c.f.view().agents[0].id)
				tick(c, func(v flowView) bool { return v.agents[0].status == "queued" })
				act(c, _actResume, 0, "")
				running(1)(c)
				c.m.release <- struct{}{}
			},
			want: "a!",
		},
		{
			name:   "stop cancels the run and the agent it never started",
			script: `return table.concat(wf.parallel({function() return wf.agent("a") end, function() return wf.agent("b") end}))`,
			limit:  1,
			drive: func(c ctl) {
				running(1)(c)
				act(c, _actStop, 0, "")
				v := tick(c, func(v flowView) bool { return v.status == RunCancelled })
				if st := v.agents[1].status; st != RunCancelled {
					c.t.Fatalf("the agent a stopped run never started reads %q", st)
				}
			},
			want: "context canceled",
		},
		{
			name:   "an action on a finished run says it is not running",
			script: `return "x"`,
			drive: func(c ctl) {
				tick(c, func(v flowView) bool { return v.status == RunDone })
				if _, err := c.h.flowControl(c.f.id, _actPause, 0, ""); err == nil || !strings.Contains(err.Error(), "not running") {
					c.t.Fatalf("pause on a finished run: %v", err)
				}
			},
			want: "x",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			m := newGate()
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
			ticks, ended := make(chan flowView, 256), make(chan struct{})
			f, err := h.startFlow(t.Context(), flowSpec{
				src: "local wf = zemi.workflow\n" + c.script, concurrency: c.limit, background: true,
				progress: func(f *flowRun) {
					select {
					case ticks <- f.view():
					default: // a case that reads no ticks
					}
				},
				done: func(*flowRun) { close(ended) },
			})
			if err != nil {
				t.Fatal(err)
			}
			c.drive(ctl{t: t, h: h, m: m, f: f, ticks: ticks})
			<-ended
			got := f.result
			if f.err != nil {
				got = f.err.Error()
			}
			if got != c.want {
				t.Fatalf("result %q, want %q", got, c.want)
			}
		})
	}
}

// TestWorkflowRowCountsAnAgentAsItIsCalled: the foreground row counts an
// agent the moment the script calls it, so a call registered right after
// parallel() returns shows up while it waits, not only when it reports.
func TestWorkflowRowCountsAnAgentAsItIsCalled(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := newGate()
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	var mu sync.Mutex
	var reports []string
	f, err := h.startFlow(t.Context(), flowSpec{
		src: "local wf = zemi.workflow\n" +
			`table.concat(wf.parallel({function() return wf.agent("a") end, function() return wf.agent("b") end})) wf.agent("c") return "done"`,
		background: true,
		progress: func(f *flowRun) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, f.summary())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	busy := func(n int32) func() error {
		return func() error {
			if got := m.running.Load(); got != n {
				return fmt.Errorf("%d agents running, want %d", got, n)
			}
			return nil
		}
	}
	waitFor(t, busy(2))
	m.release <- struct{}{}
	m.release <- struct{}{}
	// the first two rows read done only after their streams returned, so
	// busy(1) is then the third call alone: a decrement races its row's done,
	// and a read before the drain has caught the last tick still predating it
	waitFor(t, func() error {
		v := f.view()
		if len(v.agents) < 2 {
			return fmt.Errorf("%d agents", len(v.agents))
		}
		for _, a := range v.agents[:2] {
			if a.status == RunRunning {
				return fmt.Errorf("agent %s still running", a.id)
			}
		}
		return nil
	})
	waitFor(t, busy(1)) // the third call, waiting on the model
	mu.Lock()
	last, all := reports[len(reports)-1], slices.Clone(reports)
	mu.Unlock()
	if !strings.Contains(last, "2/3 agents") {
		t.Fatalf("the row of a run that called 3 agents reads %q (reports: %q)", last, all)
	}
	m.release <- struct{}{}
	<-f.done
	if f.result != "done" {
		t.Fatalf("result %q", f.result)
	}
}

// TestWorkflowRerun runs a script, then reruns it from its journal and
// reruns that rerun, counting the model calls each makes. The model answers
// "<prompt>!<n>", n counting that prompt's calls, so an answer says which
// call made it.
func TestWorkflowRerun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	var mu sync.Mutex
	calls, total := map[string]int{}, 0
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			prompt, _, _ := strings.Cut(last, "\n")
			mu.Lock()
			defer mu.Unlock()
			calls[prompt]++
			total++
			if prompt == "fail" {
				return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: context.DeadlineExceeded}}
			}
			return textParts(fmt.Sprintf("%s!%d", prompt, calls[prompt]))
		}}, nil
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return total
	}
	pad := h.ScratchDir()
	const wf = "local wf = zemi.workflow\n"
	for _, c := range []struct {
		name, first, again string
		args               any
		change             func(t *testing.T, f *flowRun) // between the run and its rerun
		from               string                         // the rerun's from, else the run's id
		want               string                         // the rerun's result, or its error's text
		calls, later       int                            // model calls the rerun and its own rerun make
		status             RunStatus                      // how the first run ended, RunDone by default
	}{
		{name: "an unchanged script answers every call from the journal", first: `return wf.agent("a") .. wf.agent("b")`, want: "a!1b!1"},
		{
			name:  "an edited prompt runs only that agent",
			first: `return wf.agent("c") .. wf.agent("d")`,
			change: func(t *testing.T, f *flowRun) {
				src, _ := os.ReadFile(f.script)
				if err := os.WriteFile(f.script, bytes.ReplaceAll(src, []byte(`"d"`), []byte(`"e"`)), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "c!1e!1", calls: 1,
		},
		{name: "a script passed with the rerun replaces the file", first: `return wf.agent("f")`, again: `return wf.agent("f") .. wf.agent("g")`, want: "f!1g!1", calls: 1},
		{
			name:  "repeated identical calls take the answers in order, then run live",
			first: `return wf.agent("h") .. wf.agent("h")`, again: `return wf.agent("h") .. wf.agent("h") .. wf.agent("h")`,
			want: "h!1h!2h!3", calls: 1,
		},
		{name: "a failed agent runs again", first: `return tostring(wf.agent("fail"))`, want: "nil", calls: 1, later: 1, status: RunWarn},
		{name: "args default to the run's", first: `return wf.args .. wf.agent("i")`, args: "x", want: "xi!1"},
		{name: "an id that is no run id is refused", first: `return "j"`, from: "../../x", want: "not a workflow run id"},
		{name: "a missing journal names the file", first: `return "k"`, from: "0000000a", want: "workflow-0000000a.jsonl"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := h.startFlow(t.Context(), flowSpec{src: wf + c.first, args: c.args})
			if err != nil {
				t.Fatal(err)
			}
			<-f.done
			if f.err != nil {
				t.Fatal(f.err)
			}
			old, err := h.loadFlow(store.MainSessionID, f.id)
			want := cmp.Or(c.status, RunDone)
			if err != nil || old.result != f.result || len(old.agents) != len(f.agents) || old.status != want {
				t.Fatalf("loadFlow = %+v, %v", old, err)
			}
			for _, a := range f.view().agents {
				if !h.flowAgentIDs().Has(a.id) {
					t.Fatalf("flowAgentIDs misses %s", a.id)
				}
			}
			if c.change != nil {
				c.change(t, f)
			}
			again := ""
			if c.again != "" {
				again = wf + c.again
			}
			for i, want := range []int{c.calls, c.later} {
				before := count()
				from, src, err := cmp.Or(c.from, f.id), again, error(nil)
				if src == "" { // the tool's rerun reads the script the user approves
					src, err = flowSource(pad, from)
				}
				var g *flowRun
				if err == nil {
					g, err = h.startFlow(t.Context(), flowSpec{src: src, from: from})
				}
				if err != nil {
					if !strings.Contains(err.Error(), c.want) {
						t.Fatalf("rerun: %v, want %q", err, c.want)
					}
					return
				}
				<-g.done
				v := g.view()
				reused := 0
				for _, a := range v.agents {
					if a.reused {
						reused++
					}
				}
				if g.err != nil || g.result != c.want || count()-before != want || reused != len(v.agents)-want {
					t.Fatalf("rerun %d = %q, %v, %d calls, %d of %d reused; want %q, %d calls", i+1, g.result, g.err, count()-before, reused, len(v.agents), c.want, want)
				}
				// a rerun that replayed answers says so in its report, naming the
				// run the answers came from
				rep, ok := g.report()
				if !ok || strings.Contains(rep, "replayed from run") != (reused > 0) || reused > 0 && !strings.Contains(rep, "run "+from+"'s journal") {
					t.Fatalf("rerun %d report = %q, reused %d; want the replay marker to match", i+1, rep, reused)
				}
				f, again = g, ""
			}
		})
	}
	// a link planted at a new run's file name fails the start, and its
	// target keeps what it held
	victim := filepath.Join(t.TempDir(), "zshrc")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(pad, flowFile("0000000b", ".jsonl"))); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(pad, "0000000b", "return 1", journalLine{Workflow: "0000000b"}); err == nil {
		t.Fatal("a planted journal link was opened")
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("the link's target was written: %q", got)
	}
}

// TestWorkflowFailedReport: a failed run's report prices the rerun - the k of
// n agents a rerun answers at once - so a model that lost a long run near its
// end reruns instead of rewriting it from scratch.
func TestWorkflowFailedReport(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	for _, c := range []struct {
		name, script, want string
	}{
		{"a late failure names the agents a rerun replays", `local a = wf.agent("quick") local t = nil return t.field`, "1 of 1 agents finished, and a rerun answers those at once"},
		{"a failure before any agent keeps the rerun line", `local t = nil return t.field`, "with action rerun."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := h.startFlow(t.Context(), flowSpec{src: "local wf = zemi.workflow\n" + c.script})
			if err != nil {
				t.Fatal(err)
			}
			<-f.done
			if f.err == nil {
				t.Fatal("the run did not fail")
			}
			report, ok := f.report()
			if ok || !strings.Contains(report, c.want) {
				t.Fatalf("report = %q, want it failing with %q", report, c.want)
			}
		})
	}
}

// TestWorkflowBranchFailWarns: a parallel branch that failed on its own warns
// the run and is named in its report even when the script drops parallel's
// errs, where the failure would vanish; one a failed agent sank is the
// agent's failure alone, not counted twice.
func TestWorkflowBranchFailWarns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	for _, c := range []struct {
		name, script, want, notWant string
	}{
		{
			name:   "a branch that raises on its own is named",
			script: `local out = wf.parallel({function() return wf.agent("ok") end, function() error("branch boom") end}) return out[1]`,
			want:   "branch 2 failed: workflow-",
		},
		{
			name:    "a branch a failed agent sank is not named twice",
			script:  `local out = wf.parallel({function() return wf.agent("ok") end, function() return wf.agent("fail") end}) return out[1]`,
			want:    `agent "fail" failed:`,
			notWant: "branch 2 failed",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := h.startFlow(t.Context(), flowSpec{src: "local wf = zemi.workflow\n" + c.script})
			if err != nil {
				t.Fatal(err)
			}
			<-f.done
			if f.err != nil {
				t.Fatal(f.err)
			}
			if f.status != RunWarn {
				t.Fatalf("status = %s, want warn", f.status)
			}
			report, ok := f.report()
			if !ok {
				t.Fatalf("report reads failed: %q", report)
			}
			if !strings.Contains(report, c.want) {
				t.Fatalf("report = %q, want %q", report, c.want)
			}
			if c.notWant != "" && strings.Contains(report, c.notWant) {
				t.Fatalf("report = %q, want no %q", report, c.notWant)
			}
		})
	}
}

// TestWorkflowListGate: list answers with nothing but the schema's label
// fields set, and still refuses a call that names a source.
func TestWorkflowListGate(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	tool := h.workflowTool(h.PipelineFor())
	for _, c := range []struct {
		name, input, want string
	}{
		{"a habitual description does not kill the list", `{"action":"list","description":"List workflow runs"}`, "no workflow run"},
		{"a source still gets no action", `{"action":"list","script":"return 1"}`, "action acts on a run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, _ := tool.Run(t.Context(), fantasy.ToolCall{ID: "c", Name: string(tools.ToolWorkflow), Input: c.input})
			if !strings.Contains(resp.Content, c.want) {
				t.Fatalf("list = %q, want %q", resp.Content, c.want)
			}
		})
	}
}

// TestWorkflowRunEmptyResult: a script that returns nothing delivers nil to
// on_done and wait, not "" - zemi.lua promises string|nil.
func TestWorkflowRunEmptyResult(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `
		local h1 = zemi.workflow.run({ script = "return nil" }, function(res)
			zemi.state.set("res", res == nil and "nil" or tostring(res))
		end)
		assert(h1:wait() == nil, "wait = " .. tostring(h1:wait()))
		local h2 = zemi.workflow.run({ script = "return 'x'" }, function() end)
		assert(h2:wait() == "x", "wait = " .. tostring(h2:wait()))
	`)
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("res"); v != `"nil"` {
			return fmt.Errorf("on_done result = %v, want nil", v)
		}
		return nil
	})
}

// TestJournalStartedReadsBothForms: a journal written now names its start in
// RFC 3339, one an older zemi wrote in unix milliseconds; both read back.
func TestJournalStartedReadsBothForms(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct{ name, line string }{
		{"a journal written now names the instant", `{"started":"` + want.Format(time.RFC3339) + `"}`},
		{"an older one names unix milliseconds", `{"started":` + strconv.FormatInt(want.UnixMilli(), 10) + `}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var l journalLine
			if err := json.Unmarshal([]byte(c.line), &l); err != nil {
				t.Fatal(err)
			}
			if !l.Started.Equal(want) {
				t.Fatalf("started = %v, want %v", l.Started.Time, want)
			}
		})
	}
}

// TestStaleWorkflowWait: a wait on a run this process no longer holds reads
// the journal's end line - one without it died with the previous process,
// whose report was never delivered, and claiming it is in the conversation
// sends the model waiting for a message that will not come.
func TestStaleWorkflowWait(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	tool := h.workflowTool(h.PipelineFor())
	pad := h.ScratchDir()
	for _, c := range []struct {
		name, end, want string
	}{
		{"an ended run's report is where it was delivered", string(RunDone), "already ended (done)"},
		{"a run that died with the process lost its report", "", "report was lost"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := store.ShortID()
			j, err := openJournal(pad, id, "return 1", journalLine{Workflow: id, Started: journalStamp{time.Now()}})
			if err != nil {
				t.Fatal(err)
			}
			if c.end != "" {
				b, _ := json.Marshal(journalLine{Status: RunStatus(c.end), MS: 5})
				if _, err := j.Write(append(b, '\n')); err != nil {
					t.Fatal(err)
				}
			}
			j.Close()
			resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
				ID: "c", Name: string(tools.ToolWorkflow),
				Input: `{"run":"` + id + `","action":"wait"}`,
			})
			if !strings.Contains(resp.Content, c.want) {
				t.Fatalf("wait = %q, want %q", resp.Content, c.want)
			}
		})
	}
}

// TestWorkflowSchema: a schema call's report is the JSON object cut out of
// the reply, checked, asked for again at most twice, and a table in Lua.
func TestWorkflowSchema(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			switch first, _, _ := strings.Cut(last, "\n"); {
			case first == "fenced":
				return textParts("Here:\n```json\n{\"n\": 1}\n```")
			case strings.HasPrefix(first, "Your reply is not the JSON"):
				return textParts(`{"n": 1}`)
			default:
				return textParts(`{}`)
			}
		}}, nil
	}
	const n = `{type = "object", properties = {n = {type = "number"}}, required = {"n"}}`
	for _, c := range []struct{ name, script, want string }{
		{"a fenced reply is a table", `return wf.agent("fenced", {schema = ` + n + `}).n`, "1"},
		{"a mismatch is asked for again", `return wf.agent("retry", {schema = ` + n + `}).n`, "1"},
		{
			"a mismatch fails after two retries",
			`local r, err = wf.agent("never", {schema = {type = "object", properties = {m = {type = "string"}}, required = {"m"}}}) return tostring(r) .. " " .. err`,
			"nil the report is not the JSON asked for",
		},
		{"a schema that is no object schema raises", `return wf.agent("x", {schema = {type = "string"}})`, "schema must be a JSON Schema object"},
		{"a required field it does not declare raises", `return wf.agent("x", {schema = {type = "object", properties = {a = {}}, required = {"b"}}})`, "schema must be"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := runFlow(t.Context(), h, "local wf = zemi.workflow\n"+c.script, nil)
			if err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("= %q, want %q", got, c.want)
			}
		})
	}
}

// TestWorkflowToolInput: which fields go together, and what a rerun asks
// about.
func TestWorkflowToolInput(t *testing.T) {
	h, r := newTestHostDB(t)
	ui := &fakeUI{askAllowed: true}
	h.SetUIHooks(ui)
	// the ask-everything policy a plugin's veto makes
	askEverything(t, r)
	tool := h.workflowTool(h.PipelineFor())
	call := func(input string) (string, bool) {
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "workflow", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content, resp.IsError
	}
	out, failed := call(`{"script":"-- Count to two\nreturn 2"}`)
	_, idPart, _ := strings.Cut(out, "\n\n[workflow ")
	id, _, _ := strings.Cut(idPart, " ")
	if failed || !strings.HasPrefix(out, "2") || h.tasks.flow(id).description != "Count to two" {
		t.Fatalf("a run with no description: %q", out)
	}
	for _, c := range []struct {
		name, input, want string
		failed            bool
		preview           string // what the ask showed, when it asks
	}{
		{name: "two sources name the three", input: `{"script":"return 1","name":"review"}`, want: "exactly one of script, name or run", failed: true},
		{name: "an action needs its run", input: `{"script":"return 1","action":"stop"}`, want: "pass its id as run", failed: true},
		{name: "a script that cannot parse never asks", input: `{"script":"return ("}`, want: "workflow script:", failed: true},
		{name: "a run alone answers its status", input: `{"run":"` + id + `"}`, want: "workflow " + id + " Count to two: done"},
		{name: "a control on an ended run says so", input: `{"run":"` + id + `","action":"pause"}`, want: "is not running", failed: true},
		{name: "a rerun asks about the script file as it is", input: `{"run":"` + id + `","action":"rerun"}`, want: "3\n\n[workflow", preview: "-- Count to two\nreturn 3"},
		{name: "a script with the rerun replaces it", input: `{"run":"` + id + `","action":"rerun","script":"return 4"}`, want: "4\n\n[workflow", preview: "return 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(h.ScratchDir(), flowFile(id, ".lua")), []byte("-- Count to two\nreturn 3"), 0o600); err != nil {
				t.Fatal(err)
			}
			n := len(ui.asked)
			out, failed := call(c.input)
			if failed != c.failed || !strings.Contains(out, c.want) {
				t.Fatalf("= %q (error %v), want %q", out, failed, c.want)
			}
			if c.preview == "" && len(ui.asked) != n {
				t.Fatalf("asked %+v; only the script's ask is wanted", ui.asked[n:])
			}
			if c.preview != "" && (len(ui.asked) != n+1 || ui.asked[n].Arg != "script" || ui.asked[n].Preview != c.preview) {
				t.Fatalf("asked %+v, want the script %q", ui.asked[n:], c.preview)
			}
		})
	}
	// the user's rerun asks about the file as the model's does
	n := len(ui.asked)
	if src, err := h.approveRerun(id); err != nil || src != "-- Count to two\nreturn 3" || len(ui.asked) != n+1 || ui.asked[n].Preview != src {
		t.Fatalf("the user's rerun = %q, %v; asked %+v", src, err, ui.asked[n:])
	}
	params := h.workflowTool(h.PipelineFor()).Info().Parameters
	if enum := fmt.Sprint(at(t, params, "action", "enum")); enum != "[status list wait pause resume stop rerun]" {
		t.Fatalf("the TUI schema offers %s", enum)
	}
}

// TestWorkflowWait: a run the model started detached has no join but the wait
// action, which returns when the run ends with its status (never the report,
// which the same instant sends as a message of its own), and stops waiting on
// esc without stopping the run.
func TestWorkflowWait(t *testing.T) {
	for _, c := range []struct {
		name      string
		esc       bool
		speak     bool
		want      string
		stillLive bool
	}{
		{name: "the run ends", want: "Its report arrives as a message"},
		{name: "esc stops waiting, not the run", esc: true, want: "Stopped waiting", stillLive: true},
		{name: "the user's message takes the run", speak: true, want: "took the turn", stillLive: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			h.SetUIHooks(&fakeUI{})
			seedSession(t, h)
			gate := make(chan struct{})
			// the run's agent blocks until the test releases it, so the run is
			// live while the wait sits on it
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(system, _ string, _ bool) []fantasy.StreamPart {
					if strings.Contains(system, "a subagent of zemi") {
						<-gate
						return textParts("worked")
					}
					return textParts("ok") // the turn a report starts
				}}, nil
			}
			tool := h.workflowTool(h.PipelineFor())
			started := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
					ID: "c1", Name: string(tools.ToolWorkflow),
					Input: `{"script":"-- gate\nreturn zemi.workflow.agent('x')","background":true}`,
				})
				started <- resp
			}()
			f := waitLiveFlow(t, h)
			if resp := waitCall(t, started); !strings.HasPrefix(resp.Content, _bgWorkflowStarted) {
				t.Fatalf("a background start = %q", resp.Content)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waited := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := tool.Run(ctx, fantasy.ToolCall{
					ID: "c2", Name: string(tools.ToolWorkflow),
					Input: `{"run":"` + f.id + `","action":"wait"}`,
				})
				waited <- resp
			}()
			if c.esc {
				cancel()
			} else if c.speak {
				h.UserSpoke()
			} else {
				close(gate)
			}
			resp := waitCall(t, waited)
			if !strings.Contains(resp.Content, c.want) || strings.Contains(resp.Content, "worked") {
				t.Fatalf("wait = %q (error %v)", resp.Content, resp.IsError)
			}
			if f.isRunning() != c.stillLive {
				t.Fatalf("the run runs = %v, want %v", f.isRunning(), c.stillLive)
			}
			if c.stillLive {
				close(gate)
			}
			waitFor(t, func() error {
				if n := reportRows(h, f.id); n != 1 {
					return fmt.Errorf("%d report rows for %s, want exactly 1", n, f.id)
				}
				return nil
			})
		})
	}
}

// TestWorkflowWaitWakesOnAgent: a workflow's wait wakes when an agent run of
// the session ends, and the answer names the agent and the workflow that
// still runs - the two are never confused.
func TestWorkflowWaitWakesOnAgent(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	gate := make(chan struct{})
	fast := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			if !strings.Contains(system, "a subagent of zemi") || toolResult {
				return textParts("noted")
			}
			if strings.Contains(last, "fastwork") {
				<-fast
				return textParts("fast done")
			}
			<-gate
			return textParts("worked")
		}}, nil
	}
	tool := h.workflowTool(h.PipelineFor())
	started := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
			ID: "c1", Name: string(tools.ToolWorkflow),
			Input: `{"script":"-- gate\nreturn zemi.workflow.agent('flowwork')","background":true}`,
		})
		started <- resp
	}()
	f := waitLiveFlow(t, h)
	if resp := waitCall(t, started); !strings.HasPrefix(resp.Content, _bgWorkflowStarted) {
		t.Fatalf("a background start = %q", resp.Content)
	}
	def, _ := h.agentDef("explore")
	fastID, err := h.startBackgroundAgent(t.Context(), def, "dig fast", "fastwork")
	if err != nil {
		t.Fatal(err)
	}
	fastRun := h.tasks.find(fastID)
	waited := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
			ID: "c2", Name: string(tools.ToolWorkflow),
			Input: `{"run":"` + f.id + `","action":"wait"}`,
		})
		waited <- resp
	}()
	waitFor(t, func() error {
		fastRun.mu.Lock()
		defer fastRun.mu.Unlock()
		if len(fastRun.updates) == 0 {
			return errors.New("the wait never registered the agent")
		}
		return nil
	})
	close(fast)
	resp := waitCall(t, waited)
	if !strings.Contains(resp.Content, fastID) || !strings.Contains(resp.Content, f.id) ||
		!strings.Contains(resp.Content, "still runs") {
		t.Fatalf("wait = %q (error %v)", resp.Content, resp.IsError)
	}
	if !f.isRunning() {
		t.Fatal("the workflow did not go on")
	}
	close(gate)
	waitIdle(t, h)
	for _, id := range []string{f.id, fastID} {
		waitFor(t, func() error {
			if n := reportRows(h, id); n != 1 {
				return fmt.Errorf("%d report rows for %s, want 1", n, id)
			}
			return nil
		})
	}
}

// TestWaitAllJoinsFlows: wait=all joins a workflow the session is running,
// not only agent runs, and the sum names it.
func TestWaitAllJoinsFlows(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	gate := make(chan struct{})
	plain := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			if !strings.Contains(system, "a subagent of zemi") || toolResult {
				return textParts("noted")
			}
			if strings.Contains(last, "plainwork") {
				<-plain
				return textParts("plain done")
			}
			<-gate
			return textParts("worked")
		}}, nil
	}
	tool := h.workflowTool(h.PipelineFor())
	started := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
			ID: "c1", Name: string(tools.ToolWorkflow),
			Input: `{"script":"-- gate\nreturn zemi.workflow.agent('flowwork')","background":true}`,
		})
		started <- resp
	}()
	f := waitLiveFlow(t, h)
	if resp := waitCall(t, started); !strings.HasPrefix(resp.Content, _bgWorkflowStarted) {
		t.Fatalf("a background start = %q", resp.Content)
	}
	def, _ := h.agentDef("explore")
	plainID, err := h.startBackgroundAgent(t.Context(), def, "dig plain", "plainwork")
	if err != nil {
		t.Fatal(err)
	}
	plainRun := h.tasks.find(plainID)
	answers := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "w", Input: `{"wait":"all"}`})
		answers <- resp
	}()
	waitFor(t, func() error {
		plainRun.mu.Lock()
		defer plainRun.mu.Unlock()
		if len(plainRun.updates) == 0 {
			return errors.New("waitAll never waited on the agent")
		}
		return nil
	})
	close(gate)
	waitFor(t, func() error {
		if f.isRunning() {
			return errors.New("the workflow never ended")
		}
		return nil
	})
	select {
	case resp := <-answers:
		t.Fatalf("waitAll answered %q while the agent still runs", resp.Content)
	default:
	}
	close(plain)
	resp := waitCall(t, answers)
	if !strings.Contains(resp.Content, plainID) || !strings.Contains(resp.Content, f.id) ||
		strings.Contains(resp.Content, "No agents are running") {
		t.Fatalf("waitAll answered %q, want both lines", resp.Content)
	}
	waitIdle(t, h)
}

// TestWaitAllHandsOffFlowToUser: the user's message hands off a wait=all
// holding a workflow too - with the flow alone, where no agent run is left to
// detach, the join must still answer the user instead of holding.
func TestWaitAllHandsOffFlowToUser(t *testing.T) {
	for _, c := range []struct {
		name  string
		agent bool
	}{
		{name: "the flow alone"},
		{name: "the flow and an agent", agent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			h.SetUIHooks(&fakeUI{})
			seedSession(t, h)
			gate := make(chan struct{})
			agentGate := make(chan struct{})
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
					if !strings.Contains(system, "a subagent of zemi") || toolResult {
						return textParts("noted")
					}
					if strings.Contains(last, "plainwork") {
						<-agentGate
						return textParts("plain done")
					}
					<-gate
					return textParts("worked")
				}}, nil
			}
			tool := h.workflowTool(h.PipelineFor())
			started := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := tool.Run(t.Context(), fantasy.ToolCall{
					ID: "c1", Name: string(tools.ToolWorkflow),
					Input: `{"script":"-- gate\nreturn zemi.workflow.agent('flowwork')","background":true}`,
				})
				started <- resp
			}()
			f := waitLiveFlow(t, h)
			if resp := waitCall(t, started); !strings.HasPrefix(resp.Content, _bgWorkflowStarted) {
				t.Fatalf("a background start = %q", resp.Content)
			}
			var plainRun *agentRun
			if c.agent {
				def, _ := h.agentDef("explore")
				plainID, err := h.startBackgroundAgent(t.Context(), def, "dig plain", "plainwork")
				if err != nil {
					t.Fatal(err)
				}
				plainRun = h.tasks.find(plainID)
			}
			if !c.agent {
				// the spoke is level-triggered within the turn, so a close
				// before the wait starts still wakes it: the deterministic
				// park signal the flow alone cannot offer
				h.UserSpoke()
			}
			answers := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "w", Input: `{"wait":"all"}`})
				answers <- resp
			}()
			if c.agent {
				waitFor(t, func() error {
					plainRun.mu.Lock()
					defer plainRun.mu.Unlock()
					if len(plainRun.updates) == 0 {
						return errors.New("waitAll never waited on the agent")
					}
					return nil
				})
				h.UserSpoke()
			}
			resp := waitCall(t, answers)
			if !strings.Contains(resp.Content, "took the turn") || !strings.Contains(resp.Content, f.id) {
				t.Fatalf("waitAll answered %q, want the workflow handed off", resp.Content)
			}
			if c.agent && !strings.Contains(resp.Content, plainRun.id) {
				t.Fatalf("waitAll answered %q, want the agent named too", resp.Content)
			}
			if !f.isRunning() || (c.agent && !plainRun.isRunning()) {
				t.Fatal("a handed-off run did not go on")
			}
			close(gate)
			if c.agent {
				close(agentGate)
			}
			waitIdle(t, h)
			waitFor(t, func() error {
				if n := reportRows(h, f.id); n != 1 {
					return fmt.Errorf("%d report rows for %s, want exactly 1", n, f.id)
				}
				return nil
			})
		})
	}
}

// TestWorkflowCallHandsOff: a foreground workflow call hands its run off when
// the user's own message arrives or the turn is interrupted, so the turn reads
// that message instead of waiting: the call answers with the handoff text, the
// run stays live, and its report arrives as exactly one message. A run stops
// by cancel, which flowControl's stop holds, not through the call's ctx.
func TestWorkflowCallHandsOff(t *testing.T) {
	for _, c := range []struct {
		name          string
		act           func(*Host, context.CancelFunc)
		wantInterrupt bool
	}{
		{
			name: "the user's message hands the run off",
			act:  func(h *Host, _ context.CancelFunc) { h.UserSpoke() },
		},
		{
			name: "esc hands the run off",
			act:  func(_ *Host, cancel context.CancelFunc) { cancel() }, wantInterrupt: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			h.SetUIHooks(&fakeUI{})
			seedSession(t, h)
			gate := make(chan struct{})
			// the agent's call blocks until the test releases it, so the run is
			// live while the call waits
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(system, _ string, _ bool) []fantasy.StreamPart {
					if strings.Contains(system, "a subagent of zemi") {
						<-gate
						return textParts("worked")
					}
					return textParts("ok") // the turn a report starts
				}}, nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := h.workflowTool(h.PipelineFor()).Run(ctx,
					fantasy.ToolCall{ID: "1", Name: string(tools.ToolWorkflow), Input: `{"script":"-- gate\nreturn zemi.workflow.agent('x')"}`})
				calls <- resp
			}()
			f := waitLiveFlow(t, h)
			c.act(h, cancel)
			resp := waitCall(t, calls)
			want := _bgWorkflowStarted
			if c.wantInterrupt {
				want = "The turn was interrupted while workflow "
			}
			if !strings.HasPrefix(resp.Content, want) || toolStatus(out(resp.Content), false, tools.Outcome{Run: "f"}) != CallBG {
				t.Fatalf("handoff = %q (error %v), want the handoff text", resp.Content, resp.IsError)
			}
			if !f.isRunning() {
				t.Fatal("the run ended with the call that was handed off")
			}
			close(gate)
			waitFor(t, func() error {
				if f.isRunning() {
					return errors.New("the run never ended")
				}
				if n := reportRows(h, f.id); n != 1 {
					return fmt.Errorf("%d report rows for %s, want exactly 1", n, f.id)
				}
				return nil
			})
			waitIdle(t, h)
		})
	}
}

// TestSpentSignalDoesNotDetachCall: the typed message is spent once the turn
// reads it at a step boundary, so a call started after that drain waits for its
// run instead of handing it off.
func TestSpentSignalDoesNotDetachCall(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	h.agents.mu.Lock()
	h.agents.steering = []string{"why is it so slow?"}
	h.agents.mu.Unlock()
	if msgs := h.deliverSteering(); len(msgs) != 1 {
		t.Fatalf("the step boundary read %d messages, want the one queued", len(msgs))
	}
	select { // the spent signal must not wake a call started later in the turn
	case <-h.userSpoke():
		t.Fatal("the spent signal still wakes a later call")
	default:
	}
	gate := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, _ string, _ bool) []fantasy.StreamPart {
			if strings.Contains(system, "a subagent of zemi") {
				<-gate
				return textParts("worked")
			}
			return textParts("ok")
		}}, nil
	}
	calls := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := h.workflowTool(h.PipelineFor()).Run(t.Context(),
			fantasy.ToolCall{ID: "1", Name: string(tools.ToolWorkflow), Input: `{"script":"-- late\nreturn zemi.workflow.agent('y')"}`})
		calls <- resp
	}()
	f := waitLiveFlow(t, h)
	close(gate)
	resp := waitCall(t, calls)
	if resp.IsError || !strings.Contains(resp.Content, "[workflow "+f.id) || reportRows(h, f.id) != 0 {
		t.Fatalf("a call after the drain = %q (error %v), want its report inline and no message", resp.Content, resp.IsError)
	}
}

// TestEndedRunIsNotHandedOff: a run that already ended cannot be handed off, so
// its report stays the caller's - delivered exactly once, and never lost.
func TestEndedRunIsNotHandedOff(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	seedSession(t, h)
	f, err := h.startFlow(t.Context(), flowSpec{src: "return 1"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.done
	h.UserSpoke() // the message arrives after the run is over
	if f.detach() {
		t.Fatal("a run that ended was handed off after the fact")
	}
	report, ok := f.report()
	if !ok || !strings.HasPrefix(report, "1\n\n[workflow") {
		t.Fatalf("the ended run's report = %q, %v", report, ok)
	}
	if n := reportRows(h, f.id); n != 0 {
		t.Fatalf("%d report rows for a run whose caller read it, want none", n)
	}
}

// TestTurnReadsTheUsersMessageWhileARunWaits: the turn's step is blocked on a
// foreground workflow call, and the user's own message still reaches the next
// step instead of waiting for the run - the deadlock the handoff exists for.
func TestTurnReadsTheUsersMessageWhileARunWaits(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	gate := make(chan struct{})
	var mu sync.Mutex
	var read bool
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{
			record: func(call fantasy.Call) {
				for _, msg := range call.Prompt {
					for _, p := range msg.Content {
						if txt, ok := p.(fantasy.TextPart); ok && strings.Contains(txt.Text, "how long will it take?") {
							mu.Lock()
							read = true
							mu.Unlock()
						}
					}
				}
			},
			script: func(system, last string, toolResult bool) []fantasy.StreamPart {
				switch {
				case strings.Contains(system, "a subagent of zemi"):
					<-gate
					return textParts("worked")
				case strings.Contains(last, "<agent-report"): // the run's report arrives
					return textParts("noted")
				case !toolResult:
					return toolCallParts("c1", string(tools.ToolWorkflow), `{"script":"-- wait\nreturn zemi.workflow.agent('x')"}`)
				default:
					return textParts("read it")
				}
			},
		}, nil
	}
	seedSession(t, h)
	if err := h.AgentSend("start a workflow"); err != nil {
		t.Fatal(err)
	}
	f := waitLiveFlow(t, h)
	// the user types while the step waits on the run
	if err := h.AgentSendUser("how long will it take?"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if !f.isRunning() {
			return errors.New("the run ended with the message")
		}
		mu.Lock()
		defer mu.Unlock()
		if !read {
			return errors.New("the turn never read the message")
		}
		return nil
	})
	close(gate)
	waitIdle(t, h)
}

// waitLiveFlow waits for the one workflow run this session started to be live.
func waitLiveFlow(t *testing.T, h *Host) *flowRun {
	t.Helper()
	var f *flowRun
	waitFor(t, func() error {
		flows := h.tasks.flowsOf(h.AgentSessionID())
		if len(flows) != 1 {
			return fmt.Errorf("%d runs started, want one", len(flows))
		}
		if f = flows[0]; !f.isRunning() {
			return errors.New("the run ended before the call was answered")
		}
		return nil
	})
	return f
}

// waitCall waits for a tool call to answer, which a call waiting on a run that
// never ends would do never.
func waitCall(t *testing.T, calls <-chan fantasy.ToolResponse) fantasy.ToolResponse {
	t.Helper()
	select {
	case resp := <-calls:
		return resp
	case <-time.After(30 * time.Second):
		t.Fatal("the call did not answer")
		return fantasy.ToolResponse{}
	}
}

// reportRows counts the notice rows a run's report drew in the session.
func reportRows(h *Host, id string) int {
	return len(slices.DeleteFunc(slices.Clone(h.Session().Transcript), func(m ChatMessage) bool {
		return m.Kind != RowNotice || m.Run != id
	}))
}

func TestWorkflowBackgroundReports(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart { return textParts("ok") }}, nil
	}
	seedSession(t, h)
	resp, err := h.workflowTool(h.PipelineFor()).Run(t.Context(),
		fantasy.ToolCall{ID: "1", Name: "workflow", Input: `{"script":"return zemi.workflow.agent('x')","background":true,"description":"scan six parts"}`})
	if err != nil || toolStatus(out(resp.Content), false, tools.Outcome{Run: "f"}) != CallBG {
		t.Fatalf("background start = %+v, %v", resp, err)
	}
	id := strings.Fields(strings.TrimPrefix(resp.Content, _bgWorkflowStarted))[0]
	waitFor(t, func() error {
		if tr := h.Session().Transcript; !slices.ContainsFunc(tr, func(m ChatMessage) bool { return m.Kind == RowNotice && m.Run == id && m.Status == CallOK }) {
			return fmt.Errorf("no report row in %+v", tr)
		}
		return nil
	})
	waitIdle(t, h)

	// the model's own detached start is refused with the advice it can act on:
	// it can call the tool again without background, a user's or Lua's cannot
	h.Settings.SetQuiet("agent.max_background", 1)
	if err := h.tasks.addFlow(&flowRun{id: store.ShortID(), parent: h.AgentSessionID(), background: true, status: RunRunning}, _waitAdvice); err != nil {
		t.Fatal(err)
	}
	resp, err = h.workflowTool(h.PipelineFor()).Run(t.Context(),
		fantasy.ToolCall{ID: "2", Name: "workflow", Input: `{"script":"return 1","background":true}`})
	if err != nil || !strings.Contains(resp.Content, _foregroundAdvice) {
		t.Fatalf("a refused background start = %q, %v, want %q", resp.Content, err, _foregroundAdvice)
	}
}

// TestWorkflowToolListsRuns: the model asks what is running with action list,
// and a run the user started in /workflows is in the answer with the id it
// acts on, a session with none says so.
func TestWorkflowToolListsRuns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	release := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			<-release
			return textParts("ok")
		}}, nil
	}
	seedSession(t, h)
	list := func() string {
		resp, err := h.workflowTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "workflow", Input: `{"action":"list"}`})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return resp.Content
	}
	if got := list(); got != "(no workflow run to act on in this session)" {
		t.Fatalf("an empty session lists %q", got)
	}
	h.Dirs.ConfigHome = t.TempDir()
	dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil || os.WriteFile(filepath.Join(dir, "count.lua"), []byte(`return zemi.workflow.agent("x")`), 0o644) != nil {
		t.Fatal(err)
	}
	if err := h.workflowCommands()[0].GoFn(h, "run count", false); err != nil {
		t.Fatal(err)
	}
	f := h.tasks.flowList()[0]
	if got := list(); !strings.Contains(got, f.id+" "+f.description+": running") {
		t.Fatalf("the user's run is not listed with its id: %q", got)
	}
	close(release)
	waitIdle(t, h)
	waitFor(t, func() error {
		if got := list(); !strings.Contains(got, "done") || !strings.Contains(got, f.id) {
			return fmt.Errorf("the ended run is not listed as finished: %q", got)
		}
		return nil
	})
}

// TestWorkflowUserStart: /workflows run starts the saved workflow with no
// model call first, and its report reaches the model as a message. The run
// belongs to
// the session its report lands in, even when it opened no session first, and
// a start that cannot run opens none.
func TestWorkflowUserStart(t *testing.T) {
	for _, c := range []struct {
		name string
		ui   bool
		want []string // the transcript rows' texts, in order
	}{
		{"the TUI says the run started", true, []string{"Started workflow ", "count  ", "ok"}},
		{"without UI hooks it still runs detached and reports", false, []string{"count  ", "ok"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			if c.ui {
				h.SetUIHooks(&fakeUI{})
			}
			h.Dirs.ConfigHome = t.TempDir()
			dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
			if err := os.MkdirAll(dir, 0o755); err != nil || os.WriteFile(filepath.Join(dir, "count.lua"), []byte(`return zemi.workflow.agent("x")`), 0o644) != nil {
				t.Fatal(err)
			}
			h.modelFn = func() (fantasy.LanguageModel, error) { return nil, ErrNoModel }
			if err := h.workflowCommands()[0].GoFn(h, "run count", false); err == nil {
				t.Fatalf("a start with no model: %v", err)
			}
			// the report's turn carries what the command had attached, a loop's note
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
					note := "no note"
					if strings.Contains(last, _loopTag) {
						note = "ok"
					}
					return textParts(note)
				}}, nil
			}
			if err := h.workflowCommands()[0].GoFn(h, "run count"+agent.Attached("loop", "Run 1 of: /workflows run count"), false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() error {
				var got []string
				for _, m := range h.Session().Transcript {
					for _, w := range c.want[len(got):min(len(got)+1, len(c.want))] {
						if strings.HasPrefix(m.Text, w) {
							got = append(got, m.Text)
						}
					}
				}
				if len(got) != len(c.want) {
					return fmt.Errorf("rows %q, want %q", got, c.want)
				}
				return nil
			})
			waitIdle(t, h)
			if f, sid := h.tasks.flowList()[0], h.AgentSessionID(); sid == "" || f.parent != sid {
				t.Fatalf("the run belongs to session %q, the report went to %q", f.parent, sid)
			}
		})
	}
}

// TestWorkflowCancel: cancelling the context stops a script that is
// waiting on agents or spinning in Lua.
func TestWorkflowCancel(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runFlow(ctx, h, `zemi.workflow.agent("x") while true do end`, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled workflow returned no error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("workflow ignored cancellation")
	}
}

// TestWorkflowPanicEnds: a panic under the scheduler ends the run as failed,
// so it holds back no reset or queued report.
func TestWorkflowPanicEnds(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	f, err := h.startFlow(t.Context(), flowSpec{src: `return zemi.workflow.agent("x")`, progress: func(*flowRun) {
		panic("boom")
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-f.done
	if f.isRunning() || h.tasks.runningFor(f.parent) != 0 {
		t.Fatalf("a panicked run is still live: %+v", f)
	}
}

func TestWorkflowFileStartingWithDashesIsNotFrontmatter(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.ConfigHome = t.TempDir()
	dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "---\nreturn 1\n"
	if err := os.WriteFile(filepath.Join(dir, "dashes.lua"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := h.workflowSource("dashes"); err != nil || got != src {
		t.Fatalf("workflowSource = %q, %v; want the file as is", got, err)
	}
}

func TestSavedWorkflows(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.ConfigHome = t.TempDir()
	dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.lua"), []byte("-- Review the diff\n-- from three angles\nreturn 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// the user's file shadows the builtin one of the same name, the other
	// builtins arriving beside it
	list := h.workflows()
	i := slices.IndexFunc(list, func(w Workflow) bool { return w.Name == "review" })
	if i < 0 || list[i].Description != "Review the diff from three angles" || list[i].Source != lua.OriginUser {
		t.Fatalf("workflows = %+v", list)
	}
	if !slices.ContainsFunc(list, func(w Workflow) bool { return w.Name == "hunt" && w.Source == lua.OriginBuiltin }) {
		t.Fatalf("workflows = %+v, want the builtin hunt beside it", list)
	}
	if _, err := h.workflowSource("nope"); err == nil || !strings.Contains(err.Error(), "have review") {
		t.Fatalf("missing workflow err = %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openWorkflows(); err != nil {
		t.Fatal(err)
	}
	row := func(name string) OverlayItem {
		j := slices.IndexFunc(ui.opened[0].Items, func(it OverlayItem) bool { return it.Text == name })
		if j < 0 {
			t.Fatalf("no %s row in %v", name, ui.opened[0].Items)
		}
		return ui.opened[0].Items[j]
	}
	if ui.opened[0].Items[0].Text != "New workflow" {
		t.Fatalf("the list does not head with making one: %v", ui.opened[0].Items[0])
	}
	var acts []string
	for _, k := range row("review").Keys {
		acts = append(acts, k.Key+" "+k.Desc)
	}
	if want := []string{"r run", "a run with arguments", "o open in the editor"}; !slices.Equal(acts, want) {
		t.Fatalf("workflow keys: %q, want %q", acts, want)
	}

	// a run's script saves under workflows/, headed by its description and
	// named after it when no name is given, a name no other workflow has
	var runs []*flowRun
	for _, desc := range []string{"Sum up", "Review"} {
		f, err := h.startFlow(t.Context(), flowSpec{description: desc, src: "return 1"})
		if err != nil {
			t.Fatal(err)
		}
		<-f.done
		runs = append(runs, f)
	}
	f := runs[0]
	// what ran saves, whatever a command wrote to the pad's copy since
	if err := os.WriteFile(f.script, []byte("return 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		run        int
		name, want string
	}{
		{0, "", ""},
		{0, "sum-up", "exists: /workflows save "},
		{0, "Sum Up", "lowercase letters"},
		{0, strings.Repeat("a", 64), ""}, // the loader's name cap
		{0, strings.Repeat("a", 65), "up to 63"},
		{1, "", "a workflow review exists"},
		{1, "review", "a workflow review exists"}, // an explicit name too: it would shadow the builtin
	} {
		path, err := h.saveFlow(runs[c.run].id, c.name)
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("save as %q: %v, want %q", c.name, err, c.want)
			}
			continue
		}
		if got, _ := os.ReadFile(path); err != nil || string(got) != "-- Sum up\nreturn 1" {
			t.Fatalf("saved %q, %v", got, err)
		}
	}
	// a run rebuilt from its journal holds no script as it ran, so offers no Save
	loaded, err := h.loadFlow(store.MainSessionID, f.id)
	if err != nil {
		t.Fatal(err)
	}
	for run, want := range map[*flowRun]bool{f: true, loaded: false} {
		if got := slices.ContainsFunc(h.flowOverlay(run).Keys, func(k OverlayKey) bool { return k.Key == "w" }); got != want {
			t.Errorf("Save offered %v, want %v", got, want)
		}
	}
}

// TestWorkflowViewTakesTheMenusPlace: viewing a workflow's agent closes the
// run's view and the workflows list with it, so the transcript shows whole;
// esc restores them (the menu under the run view the back command reopens).
func TestWorkflowViewTakesTheMenusPlace(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	f, err := h.startFlow(t.Context(), flowSpec{src: `local wf = zemi.workflow
return wf.agent("look")`})
	if err != nil {
		t.Fatal(err)
	}
	<-f.done
	run := f.agents[0].run
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openWorkflows(); err != nil {
		t.Fatal(err)
	}
	if err := h.openFlow(f.id); err != nil {
		t.Fatal(err)
	}
	if err := h.workflowCommands()[0].GoFn(h, "view "+f.id+" "+run.id, false); err != nil {
		t.Fatal(err)
	}
	if h.viewedRun() != run {
		t.Fatalf("the chat shows %v, want the agent's run", h.viewedRun())
	}
	if !slices.Contains(ui.closed, "workflow:"+f.id) || !slices.Contains(ui.closed, "workflows") {
		t.Fatalf("closed %v, want the run's view and the list gone", ui.closed)
	}
}

// TestReviewTargetsACommit: a sha reviews what that commit introduced. Its
// merge base with HEAD is the commit itself, so reading one as a base ref
// would review everything since it plus the working tree.
func TestReviewTargetsACommit(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	echoAgents(h)
	src, err := h.workflowSource("review")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ args, want string }{
		{"abc1234", "git show abc1234"},
		{"main", "git diff --merge-base main"},
		{"", "git diff HEAD"},
	} {
		res, err := runFlow(t.Context(), h, src, c.args)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !strings.Contains(res, c.want) {
			t.Errorf("/review %q collected with %q", c.args, res)
		}
	}
}

// TestFlowViewIDs: an agent row keeps its id when a later call lands in an
// earlier phase, so a refresh never moves an open detail to another agent,
// and a queued one opens like a started one, so enter's hint stays put.
func TestFlowViewIDs(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	f := &flowRun{id: "0000000a", description: "d", status: RunRunning, limit: 1}
	add := func(desc, phase string) {
		f.agents = append(f.agents, &flowAgent{description: desc, phase: phase})
	}
	ids := func() map[string]any {
		m := map[string]any{}
		for _, row := range h.flowOverlay(f).Items {
			if row.Fields == nil {
				t.Fatalf("%s opens nothing", row.Text)
			}
			m[row.Text] = row.ID
		}
		return m
	}
	add("a1", "A")
	add("b1", "B")
	before := ids()
	add("a2", "A")
	for desc, id := range before {
		if got := ids()[desc]; got != id {
			t.Fatalf("%s's row id moved from %v to %v", desc, id, got)
		}
	}
}

// TestAgentView: the preview a run's row opens, the same in /agents and a
// workflow's run - its facts, its task, and what it answered once it has. A
// row with nothing to read sets the facts alone.
func TestAgentView(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	run := func(status RunStatus, tr ...ChatMessage) *agentRun {
		return &agentRun{id: "agent-1", agent: "explore", model: "m", status: status, tr: tr}
	}
	for _, c := range []struct {
		name        string
		row         agentRow
		run         *agentRun
		wantPrefix  string // the facts table leads with this
		wantFields  string
		wantDetail  string
		wantNoParts bool
	}{
		{
			"a queued one is its facts alone",
			agentRow{def: "explore", status: "queued"},
			nil,
			"[status", "[run ]", "", true,
		},
		{
			"a live one leads with the model it sends on",
			agentRow{def: "explore", status: RunRunning},
			run(RunRunning, ChatMessage{Role: "user", Text: "look around"}),
			"[model m] [status running", "", "look around", false,
		},
		{
			"a run with no model says nothing of one",
			agentRow{def: "explore", status: RunRunning},
			&agentRun{id: "agent-1", agent: "explore", status: RunRunning, tr: []ChatMessage{{Role: "user", Text: "look around"}}},
			"[status running", "", "look around", false,
		},
		{
			"a done one answers",
			agentRow{def: "explore", status: RunDone},
			run(RunDone, ChatMessage{Role: "user", Text: "look around"}, ChatMessage{Role: "assistant", Text: "two files changed"}),
			"[model m] [status done", "", "look around\n\ntwo files changed", false,
		},
		{
			"a failed one carries its error",
			agentRow{def: "explore", status: RunFailed},
			run(RunFailed, ChatMessage{Role: "user", Text: "look"}, ChatMessage{Kind: RowError, Text: "the provider failed"}),
			"[model m] [status failed", "[error the provider failed]", "look", false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fields, detail, parts := h.agentView(c.row, c.run)
			extra := dumpFields(fields)
			if !strings.HasPrefix(extra, c.wantPrefix) {
				t.Fatalf("fields %s, want a table led by %s", extra, c.wantPrefix)
			}
			if c.wantFields != "" && !strings.Contains(extra, c.wantFields) {
				t.Fatalf("fields %s, want %s", extra, c.wantFields)
			}
			if detail != c.wantDetail {
				t.Fatalf("detail %q, want %q", detail, c.wantDetail)
			}
			if c.wantNoParts && parts != nil || !c.wantNoParts && len(parts) == 0 {
				t.Fatalf("parts %+v", parts)
			}
			if len(parts) > 0 && parts[0].Fill != FillCanvas {
				t.Fatalf("the task sits on %v, want the canvas", parts[0].Fill)
			}
		})
	}
}

// TestFlowViewNamesItsError: a failed run's view carries why it failed, a
// warn one the branches that failed, as the agent view does; a run with
// neither has no error row.
func TestFlowViewNamesItsError(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for _, c := range []struct {
		name    string
		f       *flowRun
		want    string
		notWant string
	}{
		{
			"a failed one names its error",
			&flowRun{id: "0000000b", description: "d", status: RunFailed, limit: 1, err: errors.New("the script raised")},
			"[error the script raised]", "",
		},
		{
			"a warn one names its branch failures",
			&flowRun{id: "0000000c", description: "d", status: RunWarn, limit: 1, fails: []string{"branch 2 failed: boom", "branch 3 failed: bang"}},
			"[error branch 2 failed: boom; branch 3 failed: bang]", "",
		},
		{
			"a run with neither has no error row",
			&flowRun{id: "0000000d", description: "d", status: RunDone, limit: 1},
			"", "[error",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := dumpFields(h.flowOverlay(c.f).Fields)
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("fields %s, want %s", got, c.want)
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Fatalf("fields %s, want no %s", got, c.notWant)
			}
		})
	}
}

// TestWatchFlows: one ticker claims the views, and it lets go only once no
// run lives.
func TestWatchFlows(t *testing.T) {
	r := newTaskReg(config.NewStore())
	if !r.watch() || r.watch() {
		t.Fatal("watch claimed the ticker twice or not at all")
	}
	f := &flowRun{status: RunRunning}
	r.flows["x"] = f
	if r.unwatch() {
		t.Fatal("the ticker stopped while a run lives")
	}
	f.status = RunDone
	if !r.unwatch() || !r.watch() {
		t.Fatal("the ticker did not stop after the last run ended")
	}
}
