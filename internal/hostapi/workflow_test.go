package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/metafates/zemi/internal/perm"
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

// runFlow runs src in the foreground and returns its value.
func runFlow(ctx context.Context, h *Host, src string, args any) (string, error) {
	f, err := h.startFlow(ctx, flowSpec{src: src, args: args})
	if err != nil {
		return "", err
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
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
			running <- struct{}{}
			select {} // never returns, so the run ending is the drain giving up
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
		if d := time.Since(start); d != workflowDrainGrace {
			t.Fatalf("drained for %v, want %v", d, workflowDrainGrace)
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
		{name: "bare agent points at zemi.workflow", script: `return agent("x")`, wantErr: true, want: "agent is zemi.workflow.agent"},
		{name: "print is gone", script: `print("x")`, wantErr: true, want: "print is zemi.workflow.log"},
		{name: "no other zemi namespace", script: `return zemi.fs.read("x")`, wantErr: true, want: "attempt to index"},
		{name: "syntax error", script: `return (`, wantErr: true, want: "workflow script"},
		{name: "a script error after an agent", script: `local x = wf.agent("quick") local t = nil return t.field`, wantErr: true, want: "attempt to index"},
		{name: "string.rep over the cap", script: `return ("x"):rep(1e13)`, wantErr: true, want: "byte cap"},
		{name: "string.rep under the cap", script: `return string.rep("ab", 2) .. string.rep("x", -1)`, want: "abab"},
		{name: "no file access", script: `return dofile("/etc/passwd")`, wantErr: true, want: "non-function"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := runFlow(context.Background(), h, "local wf = zemi.workflow\n"+c.script, c.args)
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
			case <-time.After(3 * time.Second):
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
			name:   "pause holds a queued agent that blocks rewind; resume starts it with the flow's session as parent",
			script: `return wf.agent("a") .. wf.agent("b")`,
			drive: func(c ctl) {
				running(1)(c)
				act(c, actPause, 0, "")
				c.m.release <- struct{}{}
				v := tick(c, func(v flowView) bool { return len(v.agents) == 2 })
				if v.agents[1].status != "queued" || v.running != 0 {
					c.t.Fatalf("paused run started %+v", v.agents[1])
				}
				if c.h.tasks.stillRunning(func(string) bool { return true }) == nil {
					c.t.Fatal("a paused workflow does not block a rewind")
				}
				c.h.agents.mu.Lock()
				c.h.agents.sessionID = "s2"
				c.h.agents.mu.Unlock()
				act(c, actResume, 0, "")
				running(1)(c)
				if p := c.h.tasks.find(c.f.view().agents[1].id).parent; p != "s1" {
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
				act(c, actConcurrency, 3, "")
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
			name:   "a restart runs the call again, the script sees only the second answer, and the row keeps the first attempt's spend",
			script: `return wf.agent("a")`,
			drive: func(c ctl) {
				running(1)(c)
				first := c.h.tasks.find(c.f.view().agents[0].id)
				first.mu.Lock()
				first.tokens = 110
				first.mu.Unlock()
				act(c, actRestart, 0, first.id)
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
				act(c, actPause, 0, "")
				act(c, actRestart, 0, c.f.view().agents[0].id)
				tick(c, func(v flowView) bool { return v.agents[0].status == "queued" })
				act(c, actResume, 0, "")
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
				act(c, actStop, 0, "")
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
				if _, err := c.h.flowControl(c.f.id, actPause, 0, ""); err == nil || !strings.Contains(err.Error(), "not running") {
					c.t.Fatalf("pause on a finished run: %v", err)
				}
			},
			want: "x",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			h.agents.sessionID = "s1"
			m := newGate()
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
			ticks, ended := make(chan flowView, 256), make(chan struct{})
			f, err := h.startFlow(context.Background(), flowSpec{
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
			c.drive(ctl{t, h, m, f, ticks})
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

// TestWorkflowRerun runs a script, then reruns it from its journal and
// reruns that rerun, counting the model calls each makes. The model answers
// "<prompt>!<n>", n counting that prompt's calls, so an answer says which
// call made it.
func TestWorkflowRerun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
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
	pad := h.scratchFor("s1")
	link := func(name string) func(t *testing.T, f *flowRun) {
		return func(t *testing.T, f *flowRun) {
			path := filepath.Join(pad, flowFile(f.id, name))
			elsewhere := filepath.Join(t.TempDir(), name)
			if err := os.Rename(path, elsewhere); err != nil || os.Symlink(elsewhere, path) != nil {
				t.Fatal(err)
			}
		}
	}
	const wf = "local wf = zemi.workflow\n"
	for _, c := range []struct {
		name, first, again string
		args               any
		change             func(t *testing.T, f *flowRun) // between the run and its rerun
		from               string                         // the rerun's from, else the run's id
		want               string                         // the rerun's result, or its error's text
		calls, later       int                            // model calls the rerun and its own rerun make
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
		{name: "a failed agent runs again", first: `return tostring(wf.agent("fail"))`, want: "nil", calls: 1, later: 1},
		{name: "args default to the run's", first: `return wf.args .. wf.agent("i")`, args: "x", want: "xi!1"},
		{name: "an id that is no run id is refused", first: `return "j"`, from: "../../x", want: "not a workflow run id"},
		{name: "a missing journal names the file", first: `return "k"`, from: "0000000a", want: "workflow-0000000a.jsonl"},
		{name: "a journal planted as a link is refused", first: `return "l"`, change: link(".jsonl"), want: "a symlink"},
		{name: "a script planted as a link is refused", first: `return "m"`, change: link(".lua"), want: "a symlink"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := h.startFlow(context.Background(), flowSpec{src: wf + c.first, args: c.args})
			if err != nil || f.err != nil {
				t.Fatal(err, f.err)
			}
			if old, err := h.loadFlow("s1", f.id); err != nil || old.result != f.result || len(old.agents) != len(f.agents) || old.status != RunDone {
				t.Fatalf("loadFlow = %+v, %v", old, err)
			}
			for _, a := range f.view().agents {
				if !h.flowAgentIDs("s1")[a.id] {
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
					g, err = h.startFlow(context.Background(), flowSpec{src: src, from: from})
				}
				if err != nil {
					if !strings.Contains(err.Error(), c.want) {
						t.Fatalf("rerun: %v, want %q", err, c.want)
					}
					return
				}
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
			}
			return textParts(`{}`)
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
			got, err := runFlow(context.Background(), h, "local wf = zemi.workflow\n"+c.script, nil)
			if err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("= %q, want %q", got, c.want)
			}
		})
	}
}

// TestWorkflowToolInput: which fields go together, what a rerun asks
// about, and what headless leaves out.
func TestWorkflowToolInput(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	ui := &fakeUI{askAllowed: true}
	h.SetUIHooks(ui)
	h.Perm.Add(perm.Rule{Subject: "tool.workflow", Pattern: "*", Decision: perm.Ask})
	tool := h.workflowTool(h.PipelineFor())
	call := func(input string) (string, bool) {
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "workflow", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content, resp.IsError
	}
	out, failed := call(`{"script":"-- Count to two\nreturn 2"}`)
	id, _, _ := strings.Cut(strings.TrimPrefix(out, "[workflow "), " ")
	if failed || !strings.HasSuffix(out, "\n\n2") || h.tasks.flow(id).description != "Count to two" {
		t.Fatalf("a run with no description: %q", out)
	}
	for _, c := range []struct {
		name, input, want string
		failed            bool
		preview           string // what the ask showed, when it asks
	}{
		{name: "two sources name the three", input: `{"script":"return 1","name":"review"}`, want: "exactly one of script, name or run", failed: true},
		{name: "an action needs its run", input: `{"script":"return 1","action":"stop"}`, want: "pass its id as run", failed: true},
		{name: "a run alone answers its status", input: `{"run":"` + id + `"}`, want: "workflow " + id + " Count to two: done"},
		{name: "a control on an ended run says so", input: `{"run":"` + id + `","action":"pause"}`, want: "is not running", failed: true},
		{name: "a rerun asks about the script file as it is", input: `{"run":"` + id + `","action":"rerun"}`, want: "\n\n3", preview: "-- Count to two\nreturn 3"},
		{name: "a script with the rerun replaces it", input: `{"run":"` + id + `","action":"rerun","script":"return 4"}`, want: "\n\n4", preview: "return 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(h.scratchFor("s1"), flowFile(id, ".lua")), []byte("-- Count to two\nreturn 3"), 0o600); err != nil {
				t.Fatal(err)
			}
			n := len(ui.asked)
			out, failed := call(c.input)
			if failed != c.failed || !strings.Contains(out, c.want) {
				t.Fatalf("= %q (error %v), want %q", out, failed, c.want)
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
	h.SetUIHooks(nil)
	info := h.workflowTool(h.PipelineFor()).Info()
	if _, bg := info.Parameters["background"]; bg || fmt.Sprint(info.Parameters["action"].(map[string]any)["enum"]) != "[status rerun]" {
		t.Fatalf("headless schema: %v", info.Parameters)
	}
}

// TestWorkflowBackgroundReports: a background run answers at once, its row
// reads as still running, and its result reaches the model as a report.
func TestWorkflowBackgroundReports(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart { return textParts("ok") }}, nil
	}
	seedSession(t, h)
	resp, err := h.workflowTool(h.PipelineFor()).Run(context.Background(),
		fantasy.ToolCall{ID: "1", Name: "workflow", Input: `{"script":"return zemi.workflow.agent('x')","background":true,"description":"scan six parts"}`})
	if err != nil || toolStatus(tools.ToolWorkflow, !resp.IsError, resp.Content, false) != CallBG {
		t.Fatalf("background start = %+v, %v", resp, err)
	}
	id := strings.Fields(strings.TrimPrefix(resp.Content, bgWorkflowStarted))[0]
	waitFor(t, func() error {
		if tr := h.Session().Transcript; !slices.ContainsFunc(tr, func(m ChatMessage) bool { return m.Text == "Workflow "+id+" (scan six parts) finished" }) {
			return fmt.Errorf("no report row in %+v", tr)
		}
		return nil
	})
	waitIdle(t, h)

	// the model's own detached start is refused with the advice it can act on:
	// it can call the tool again without background, a user's or Lua's cannot
	h.Settings.SetQuiet("agent.max_background", 1)
	if err := h.tasks.addFlow(&flowRun{id: store.ShortID(), parent: h.AgentSessionID(), background: true, status: RunRunning}, waitAdvice); err != nil {
		t.Fatal(err)
	}
	resp, err = h.workflowTool(h.PipelineFor()).Run(context.Background(),
		fantasy.ToolCall{ID: "2", Name: "workflow", Input: `{"script":"return 1","background":true}`})
	if err != nil || !strings.Contains(resp.Content, foregroundAdvice) {
		t.Fatalf("a refused background start = %q, %v, want %q", resp.Content, err, foregroundAdvice)
	}
}

// TestWorkflowUserStart: /workflows run starts the saved workflow with no
// model call first, and its report reaches the model: detached in the TUI,
// on the claimed agent headless, which zemi run waits on. The run belongs to
// the session its report lands in, even when it opened no session first, and
// a start that cannot run opens none.
func TestWorkflowUserStart(t *testing.T) {
	for _, c := range []struct {
		name string
		ui   bool
		want []string // the transcript rows' texts, in order
	}{
		{"the TUI runs it detached and says so", true, []string{"Started workflow ", "Workflow ", "ok"}},
		{"headless it holds the agent until the report's turn ends", false, []string{"Workflow ", "ok"}},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			if err := h.workflowCommands()[0].GoFn(h, "run count"); err == nil || h.AgentSessionID() != "" {
				t.Fatalf("a start with no model: %v, session %q", err, h.AgentSessionID())
			}
			// the report's turn carries what the command had attached, a loop's note
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
					return textParts(map[bool]string{true: "ok", false: "no note"}[strings.Contains(last, loopTag)])
				}}, nil
			}
			if err := h.workflowCommands()[0].GoFn(h, "run count"+agent.Attached("loop", "Run 1 of: /workflows run count")); err != nil {
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
	h, _ := newTestHostDB(t)
	echoAgents(h)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
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
	case <-time.After(3 * time.Second):
		t.Fatal("workflow ignored cancellation")
	}
}

// TestWorkflowPanicEnds: a panic under the scheduler ends the run as failed,
// so it holds back no rewind or queued report.
func TestWorkflowPanicEnds(t *testing.T) {
	h, _ := newTestHostDB(t)
	echoAgents(h)
	var f *flowRun
	func() {
		defer func() { _ = recover() }()
		f, _ = h.startFlow(context.Background(), flowSpec{src: `return zemi.workflow.agent("x")`, progress: func(r *flowRun) {
			f = r
			panic("boom")
		}})
	}()
	if f == nil || f.isRunning() || h.tasks.runningFor(f.parent) != 0 {
		t.Fatalf("a panicked run is still live: %+v", f)
	}
}

func TestWorkflowFileStartingWithDashesIsNotFrontmatter(t *testing.T) {
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
	if !slices.ContainsFunc(list, func(w Workflow) bool { return w.Name == "research" && w.Source == lua.OriginBuiltin }) {
		t.Fatalf("workflows = %+v, want the builtin research beside it", list)
	}
	if _, err := h.workflowSource("nope"); err == nil || !strings.Contains(err.Error(), "have review") {
		t.Fatalf("missing workflow err = %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openWorkflows(); err != nil {
		t.Fatal(err)
	}
	row := func(name string) map[string]any {
		j := slices.IndexFunc(ui.opened[0].Items, func(it any) bool { return it.(map[string]any)["text"] == name })
		if j < 0 {
			t.Fatalf("no %s row in %v", name, ui.opened[0].Items)
		}
		return ui.opened[0].Items[j].(map[string]any)
	}
	if ui.opened[0].Items[0].(map[string]any)["text"] != "New workflow" {
		t.Fatalf("the list does not head with making one: %v", ui.opened[0].Items[0])
	}
	acts := fmt.Sprint(row("review")["actions"])
	if acts != "[map[hotkey:r run:workflows run review text:Run] map[fill:/workflows run review  hotkey:a text:Run with arguments] map[hotkey:o run:workflows open review text:Open in the editor]]" {
		t.Fatalf("workflow actions: %s", acts)
	}

	// a run's script saves under workflows/, headed by its description and
	// named after it when no name is given, a name no other workflow has
	h.agents.sessionID = "s1"
	var runs []*flowRun
	for _, desc := range []string{"Sum up", "Review"} {
		f, err := h.startFlow(context.Background(), flowSpec{description: desc, src: "return 1"})
		if err != nil {
			t.Fatal(err)
		}
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
		{1, "", "a workflow review exists"},
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
	loaded, err := h.loadFlow("s1", f.id)
	if err != nil {
		t.Fatal(err)
	}
	for run, want := range map[*flowRun]bool{f: true, loaded: false} {
		if got := slices.ContainsFunc(h.flowOverlay(run).Keys, func(k OverlayKey) bool { return k.Key == "s" }); got != want {
			t.Errorf("Save offered %v, want %v", got, want)
		}
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
		res, err := runFlow(context.Background(), h, src, c.args)
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
	h, _ := newTestHostDB(t)
	f := &flowRun{id: "0000000a", description: "d", status: RunRunning, limit: 1}
	add := func(desc, phase string) {
		f.agents = append(f.agents, &flowAgent{description: desc, phase: phase})
	}
	ids := func() map[string]any {
		m := map[string]any{}
		for _, it := range h.flowOverlay(f).Items {
			row, _ := ParseOverlayItem(it)
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
