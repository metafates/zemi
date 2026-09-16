//go:build !windows

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/proc"
)

func TestEditRefusesAmbiguousMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("x = 1\nx = 1\n"), 0o644)
	p := &Pipeline{}
	for _, c := range []struct {
		input map[string]any
		want  string
		file  string
	}{
		{map[string]any{"path": path, "old_string": "x = 1", "new_string": "x = 2"}, "matches 2 places", "x = 1\nx = 1\n"},
		{map[string]any{"path": path, "old_string": "", "new_string": "y"}, "empty", "x = 1\nx = 1\n"},
		{map[string]any{"path": path, "old_string": "x = 1", "new_string": "x = 2", "replace_all": true}, "2 replacements", "x = 2\nx = 2\n"},
	} {
		res, _ := runEdit(context.Background(), p, c.input)
		got, _ := os.ReadFile(path)
		if !strings.Contains(res.Content, c.want) || string(got) != c.file {
			t.Fatalf("%v: %q, file %q", c.input, res.Content, got)
		}
	}
}

// TestJobReadAfterBufferDrop: a job that floods its buffer keeps delivering
// new output. The buffer drops its middle, so its length stops growing and
// a length-based offset would report "no new output" forever.
func TestJobReadAfterBufferDrop(t *testing.T) {
	j := &Job{stdout: &proc.Buffer{Max: 64}, stderr: &proc.Buffer{Max: 64}, done: make(chan struct{})}
	close(j.done)
	j.stdout.Write([]byte(strings.Repeat("a", 40)))
	if out, _, _, _ := j.since(); out != strings.Repeat("a", 40) {
		t.Fatalf("first read: %q", out)
	}
	if out, _, _, _ := j.since(); out != "" {
		t.Fatalf("nothing written since: %q", out)
	}
	// more arrived than the buffer holds, so it returns what is left of it,
	// skip marker and all, rather than reporting nothing new
	j.stdout.Write([]byte(strings.Repeat("b", 200)))
	out, _, _, _ := j.since()
	if !strings.HasSuffix(out, "bb") || !strings.Contains(out, "skipped") {
		t.Fatalf("new output after the drop: %q", out)
	}
	if out, _, _, _ := j.since(); out != "" {
		t.Fatalf("nothing written since the drop: %q", out)
	}
}

// TestJobReadSandboxHint: a sandboxed job's denial read while it ran still
// gets the hint once it exits, and the sandbox: false retry is offered only
// where someone can answer the ask.
func TestJobReadSandboxHint(t *testing.T) {
	for _, param := range []bool{true, false} {
		p := &Pipeline{Jobs: NewJobs(), SandboxParam: param}
		out := &proc.Buffer{Max: 1 << 20}
		wait := make(chan error, 1)
		id := p.Jobs.Adopt("touch x", nil, out, 0, wait, func() string { return "\nguard note" })
		out.Write([]byte("touch: x: Operation not permitted\n"))
		read := func() string {
			res, _ := runJobRead(context.Background(), p, map[string]any{"id": id})
			return res.Content
		}
		if got := read(); !strings.Contains(got, "not permitted") {
			t.Fatalf("while running: %q", got)
		}
		wait <- errors.New("exit status 1")
		j, _ := p.Jobs.Get(id)
		<-j.done
		got := read()
		if !strings.Contains(got, "The sandbox may have blocked this") || strings.Contains(got, "sandbox: false") != param || !strings.Contains(got, "guard note") {
			t.Errorf("param %v: after exit %q", param, got)
		}
	}
}

// TestBashTimeoutBecomesJob: a command still running at its timeout keeps
// going as a job, so a slow build is not thrown away half done.
func TestBashTimeoutBecomesJob(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runBash(context.Background(), p, map[string]any{
		"command": "echo started; sleep 30", "timeout_ms": float64(400)})
	if res.IsError || !strings.Contains(res.Content, "moved to the background as job-1") {
		t.Fatalf("timeout must hand off to a job: %+v", res)
	}
	if !strings.Contains(res.Content, "started") {
		t.Fatalf("output from before the timeout must survive: %q", res.Content)
	}
	if out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"}); !strings.Contains(out.Content, "running") {
		t.Fatalf("the job must still be running: %q", out.Content)
	}
	runJobKill(context.Background(), p, map[string]any{"id": "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the adopted command")
	}
}

// TestJobStreamsOutput: job_read shows a long-running process's output
// while it runs, and a shell line works without args.
func TestJobStreamsOutput(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runJobStart(context.Background(), p, map[string]any{"command": "echo ready && sleep 30"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"})
		if strings.Contains(out.Content, "ready") && strings.Contains(out.Content, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"}); strings.Contains(out.Content, "ready") {
		t.Fatalf("a second read must not repeat delivered output: %q", out.Content)
	}
	runJobKill(context.Background(), p, map[string]any{"id": "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the shell and its sleep")
	}
}
