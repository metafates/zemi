package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
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

// TestWorkflowCancelBoundedDrain: an agent that never notices the cancel
// must not hold runWorkflow open past the drain grace. The run checks that a
// cancel ends it; the bubble checks the bound.
func TestWorkflowCancelBoundedDrain(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	running := make(chan struct{}, 1)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
			running <- struct{}{}
			select {} // never returns, so runWorkflow returning is the drain giving up
		}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := h.runWorkflow(ctx, `zemi.workflow.agent("x")`, nil, nil)
		done <- err
	}()
	// cancelled only once the agent runs: a cancel that beats it tests nothing
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("runWorkflow returned before its agent ran: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled workflow returned no error")
		}
	case <-time.After(time.Minute):
		t.Fatal("runWorkflow waited on an agent that ignores cancellation")
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
		ran          string // an agent line a failed run keeps
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
		{name: "a failed script names the agents that ran", script: `local x = wf.agent("quick") local t = nil return t.field`, wantErr: true, want: "attempt to index", ran: "general done: quick"},
		{name: "string.rep over the cap", script: `return ("x"):rep(1e13)`, wantErr: true, want: "byte cap"},
		{name: "string.rep under the cap", script: `return string.rep("ab", 2) .. string.rep("x", -1)`, want: "abab"},
		{name: "no file access", script: `return dofile("/etc/passwd")`, wantErr: true, want: "non-function"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := h.runWorkflow(context.Background(), "local wf = zemi.workflow\n"+c.script, c.args, nil)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				// each line leads with an id send_to continues
				for _, l := range res.Agents {
					if h.tasks.find(strings.Fields(l)[0]) == nil || c.ran == "" || !strings.Contains(l, c.ran) {
						t.Fatalf("agents = %q, want %q", res.Agents, c.ran)
					}
				}
				if c.ran != "" && len(res.Agents) == 0 {
					t.Fatalf("no agents listed, want %q", c.ran)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res.Value, c.want) {
				t.Fatalf("result = %q, want %q", res.Value, c.want)
			}
		})
	}
	if peak.Load() < 2 {
		t.Fatalf("parallel agents never overlapped (peak %d)", peak.Load())
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
		_, err := h.runWorkflow(ctx, `zemi.workflow.agent("x") while true do end`, nil, nil)
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
	list := h.workflows()
	if len(list) != 1 || list[0].Name != "review" || list[0].Description != "Review the diff from three angles" {
		t.Fatalf("workflows = %+v", list)
	}
	if _, err := h.workflowSource("nope"); err == nil || !strings.Contains(err.Error(), "have review") {
		t.Fatalf("missing workflow err = %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openWorkflows(); err != nil {
		t.Fatal(err)
	}
	acts := fmt.Sprint(ui.opened[0].Items[0].(map[string]any)["actions"])
	if acts != "[map[hotkey:r run:workflow review text:Run] map[fill:/workflow review  hotkey:a text:Run with arguments]]" {
		t.Fatalf("workflow actions: %s", acts)
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
		res, err := h.runWorkflow(context.Background(), src, c.args, nil)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !strings.Contains(res.Value, c.want) {
			t.Errorf("/review %q collected with %q", c.args, res.Value)
		}
	}
}
