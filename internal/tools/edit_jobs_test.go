//go:build !windows

package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestShellTimeoutBecomesJob(t *testing.T) {
	t.Parallel()
	p := &Pipeline{Jobs: &Jobs{}}
	res, _ := runShell(t.Context(), p, shellArgs{
		Command: "echo started; sleep 30", TimeoutMS: 400,
	})
	if res.IsError || !strings.Contains(res.Content, JobHandoff+"job-1") {
		t.Fatalf("timeout must hand off to a job: %+v", res)
	}
	if !strings.Contains(res.Content, "started") {
		t.Fatalf("output from before the timeout must survive: %q", res.Content)
	}
	if out, _ := runJobRead(t.Context(), p, jobReadArgs{ID: "job-1"}); !strings.Contains(out.Content, "running") {
		t.Fatalf("the job must still be running: %q", out.Content)
	}
	runJobKill(t.Context(), p, jobKillArgs{ID: "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the adopted command")
	}
}

// TestShellOrphanIsNotAnError: a command that leaves a child holding the
// pipe exited 0; the child is stopped and named, not reported as a failure.
func TestShellOrphanIsNotAnError(t *testing.T) {
	t.Parallel()
	res, _ := runShell(t.Context(), &Pipeline{}, shellArgs{Command: "sleep 30 &\necho started"})
	if res.IsError || !strings.HasPrefix(res.Content, "started\na background process it started was stopped") {
		t.Fatalf("orphan: %+v", res)
	}
}

// TestJobReadOfAFailedJobIsAnError: a read of a job that ended badly is an
// error response, so the model does not read the exit as done and the row is
// no green check; a clean exit and a job job_kill stopped stay success, as
// Job.Mark says.
func TestJobReadOfAFailedJobIsAnError(t *testing.T) {
	for _, c := range []struct {
		name, command string
		killed        bool
		wantErr       bool
	}{
		{"a clean exit", "echo done", false, false},
		{"a failed command", "printf boom; exit 2", false, true},
		{"a job the user killed", "sleep 30", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &Pipeline{Jobs: &Jobs{}, Pad: t.TempDir()}
			start, _ := runJobStart(t.Context(), p, jobStartArgs{Command: c.command})
			if start.IsError {
				t.Fatal(start.Content)
			}
			id := p.Jobs.List()[0].ID // the one job this pipeline started
			if c.killed {
				if res, _ := runJobKill(t.Context(), p, jobKillArgs{ID: id}); res.IsError {
					t.Fatal(res.Content)
				}
			}
			j, err := p.Jobs.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			waitJobDone(t, j)
			res, _ := runJobRead(t.Context(), p, jobReadArgs{ID: id})
			if res.IsError != c.wantErr {
				t.Fatalf("a read of %q is an error %v, want %v: %q", c.command, res.IsError, c.wantErr, res.Content)
			}
			if c.wantErr && (!strings.Contains(res.Content, "exited (2)") || !strings.Contains(res.Content, "boom")) {
				t.Fatalf("the exit state or the last output is missing: %q", res.Content)
			}
		})
	}
}

// TestShellProgress: the last output line reaches Progress while the command
// runs, and nothing clears it: the result replaces the row in one change.
func TestShellProgress(t *testing.T) {
	p, _ := testPipeline(t, true)
	ctx, stop := context.WithCancel(t.Context())
	var mu sync.Mutex
	var lines, tails []string
	p.Progress = func(id string, got Progress) {
		mu.Lock()
		defer mu.Unlock()
		lines, tails = append(lines, id+":"+got.Line), append(tails, got.Tail)
		stop() // the line came while the command ran
	}
	// one write, so no poll lands between the lines
	input := `{"command":"printf 'one\\ntwo\\n'; sleep 30"}`
	if _, err := toolNamed(t, "shell", p).Run(ctx, fantasy.ToolCall{ID: "t1", Name: "shell", Input: input}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// the tail is the whole output a view of the call reads, not its last line
	if len(lines) != 1 || lines[0] != "t1:two" || len(tails) != 1 || tails[0] != "one\ntwo" {
		t.Fatalf("progress lines = %q, tails = %q", lines, tails)
	}
}

// TestShellStops: an interrupt or a timeout ends a command whose children
// keep its output pipe open, instead of waiting on them forever.
func TestShellStops(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		input  shellArgs
		cancel bool
		want   string
	}{
		{"interrupt", shellArgs{Command: "sleep 30 | cat"}, true, "interrupted"},
		{"timeout", shellArgs{Command: "sleep 30 & wait", TimeoutMS: 300}, false, "timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.cancel {
				time.AfterFunc(200*time.Millisecond, cancel)
			}
			start := time.Now()
			res, err := runShell(ctx, &Pipeline{}, c.input)
			if err != nil || time.Since(start) > 5*time.Second || !strings.Contains(res.Content, c.want) {
				t.Fatalf("after %s: %v %q", time.Since(start), err, res.Content)
			}
		})
	}
}
