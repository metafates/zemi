package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// echoAgents stubs every subagent to answer "<prompt>!" after a short
// delay, recording the highest number of agents running at once.
func echoAgents(h *Host) *atomic.Int32 {
	var running, peak atomic.Int32
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(30 * time.Millisecond)
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
// must not hold runWorkflow open past the drain grace.
func TestWorkflowCancelBoundedDrain(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			time.Sleep(30 * time.Second)
			return textParts(last)
		}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.runWorkflow(ctx, `agent("x")`, nil, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled workflow returned no error")
		}
	case <-time.After(workflowDrainGrace + 3*time.Second):
		t.Fatal("runWorkflow waited on an agent that ignores cancellation")
	}
}

func TestWorkflowRun(t *testing.T) {
	h, _ := newTestHostDB(t)
	peak := echoAgents(h)
	for _, c := range []struct {
		name, script string
		args         any
		want         string // result, or the error substring when wantErr
		wantErr      bool
	}{
		{name: "sequential chain", script: `local a = agent("x") return agent(a .. "y")`, want: "x!y!"},
		{
			name: "parallel branches keep order and chain independently",
			script: `
				local jobs = {}
				for i = 1, 4 do
					jobs[i] = function() return agent(agent("p" .. i)) end
				end
				local out = parallel(jobs)
				return table.concat(out, ",")`,
			want: "p1!!,p2!!,p3!!,p4!!",
		},
		{
			name:   "args and json",
			script: `local t = json.decode("noise " .. json.encode(args) .. " noise") return {n = t.n + 1}`,
			args:   map[string]any{"n": 1},
			want:   "{\n  \"n\": 2\n}",
		},
		{
			name:   "failed branch reports through errs",
			script: `local out, errs = parallel({function() return agent("ok") end, function() local r, e = agent("fail") return r or error(e) end}) return out[1] .. " " .. tostring(errs ~= nil and errs[2] ~= nil)`,
			want:   "ok! true",
		},
		{name: "crashed agent", script: `local r, e = agent("panic") return e`, want: "the agent crashed"},
		{name: "agent inside pcall refused", script: `local ok, err = pcall(agent, "x") return tostring(ok) .. " " .. err`, want: "false"},
		{name: "pcall without a function", script: `return pcall()`, wantErr: true, want: "bad argument #1"},
		{name: "agent in a library callback", script: `local t = {3, 1, 2} table.sort(t, function(a, b) agent("x") return a < b end) return "sorted"`, wantErr: true, want: "cannot wait"},
		{name: "many instant parallel calls", script: `local n = 0 for i = 1, 20000 do n = n + parallel({function() return 1 end})[1] end return tostring(n)`, want: "20000"},
		{name: "parallel over the agent cap", script: `local t = {} for i = 1, 101 do t[i] = function() return 1 end end return parallel(t)`, wantErr: true, want: "more than the"},
		{name: "unknown agent", script: `return agent("x", {agent = "nope"})`, wantErr: true, want: "unknown agent"},
		{name: "syntax error", script: `return (`, wantErr: true, want: "workflow script"},
		{name: "string.rep over the cap", script: `return ("x"):rep(1e13)`, wantErr: true, want: "byte cap"},
		{name: "string.rep under the cap", script: `return string.rep("ab", 2) .. string.rep("x", -1)`, want: "abab"},
		{name: "no file access", script: `return dofile("/etc/passwd")`, wantErr: true, want: "non-function"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := h.runWorkflow(context.Background(), c.script, c.args, nil)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want %q", err, c.want)
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
		_, err := h.runWorkflow(ctx, `agent("x") while true do end`, nil, nil)
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
}

func TestAgentsOverlayConfigDir(t *testing.T) {
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.Dirs.ConfigHome = t.TempDir()
	if err := h.openAgents(); err != nil || len(ui.opened) != 1 || !strings.Contains(fmt.Sprint(ui.opened[0].Items), TildePath(h.Dirs.ConfigHome)) {
		t.Fatalf("agents overlay = %v %+v", err, ui.opened)
	}
}
