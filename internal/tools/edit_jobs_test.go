//go:build !windows

package tools

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/proc"
)

// TestJobReadAfterBufferDrop: a job that floods its buffer keeps delivering
// new output. The buffer drops its middle, so its length stops growing and
// a length-based offset would report "no new output" forever.
func TestJobReadAfterBufferDrop(t *testing.T) {
	j := &Job{stdout: &proc.Buffer{Max: 64}, stderr: &proc.Buffer{Max: 64}, done: make(chan struct{})}
	close(j.done)
	j.stdout.Write([]byte(strings.Repeat("a", 40)))
	if out, _, _, _ := j.since(); out.text != strings.Repeat("a", 40) {
		t.Fatalf("first read: %+v", out)
	}
	if out, _, _, _ := j.since(); out.text != "" {
		t.Fatalf("nothing written since: %+v", out)
	}
	// more arrived than the buffer holds, so it returns what is left of it,
	// skip marker and all, rather than reporting nothing new
	j.stdout.Write([]byte(strings.Repeat("b", 200)))
	out, _, _, _ := j.since()
	if !strings.HasSuffix(out.text, "bb") || !strings.Contains(out.text, "skipped") || out.lost <= 0 {
		t.Fatalf("new output after the drop: %+v", out)
	}
	if out, _, _, _ := j.since(); out.text != "" {
		t.Fatalf("nothing written since the drop: %+v", out)
	}
}

func TestShellTimeoutBecomesJob(t *testing.T) {
	t.Parallel()
	p := &Pipeline{Jobs: NewJobs()}
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

// TestVerifyStops: the tool that decides the task is done ends the turn on
// exit 0 only - a failed run and a handoff to a job both continue the loop.
func TestVerifyStops(t *testing.T) {
	t.Parallel()
	p := &Pipeline{Jobs: NewJobs()}
	for _, c := range []struct {
		name, command string
		stop, isErr   bool
	}{
		{"exit 0", "echo green", true, false},
		{"exit 1", "echo red >&2; exit 1", false, true},
		{"handoff", "echo started; sleep 30", false, false},
	} {
		res, err := runVerify(t.Context(), p, verifyArgs{Command: c.command, TimeoutMS: 400})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.StopTurn != c.stop || res.IsError != c.isErr {
			t.Fatalf("%s: stop %v isErr %v: %q", c.name, res.StopTurn, res.IsError, res.Content)
		}
	}
	runJobKill(t.Context(), p, jobKillArgs{ID: "job-1"})
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

// TestJobStreamsOutput: job_read shows a long-running process's output
// while it runs, and a shell line works without args.
func TestJobStreamsOutput(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runJobStart(t.Context(), p, jobStartArgs{Command: "echo ready && sleep 30"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := runJobRead(t.Context(), p, jobReadArgs{ID: "job-1"})
		if strings.Contains(out.Content, "ready") && strings.Contains(out.Content, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, _ := runJobRead(t.Context(), p, jobReadArgs{ID: "job-1"}); strings.Contains(out.Content, "ready") {
		t.Fatalf("a second read must not repeat delivered output: %q", out.Content)
	}
	runJobKill(t.Context(), p, jobKillArgs{ID: "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the shell and its sleep")
	}
}

// TestJobReadSpillsTheCut: a read that cuts the middle saves the whole
// chunk, which since() has already moved past.
func TestJobReadSpillsTheCut(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{Jobs: NewJobs(), Pad: dir}
	if res, _ := runJobStart(t.Context(), p, jobStartArgs{Command: "seq 1 4000"}); res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := runJobRead(t.Context(), p, jobReadArgs{ID: "job-1"})
		if _, file, ok := strings.Cut(out.Content, "(output saved to "); ok {
			file, _, _ = strings.Cut(file, ")")
			b, err := os.ReadFile(file)
			if err != nil || !strings.Contains(string(b), "\n2000\n") {
				t.Fatalf("spill %s: %v", file, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no spill: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
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
			p := &Pipeline{Jobs: NewJobs(), Pad: t.TempDir()}
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
