//go:build !windows

package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sandbox"
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

// TestJobReadSandboxHint: a sandboxed job's denial read while it ran still
// gets the hint once, after it exits, and the sandbox: false retry is offered
// only where someone can answer the ask.
func TestJobReadSandboxHint(t *testing.T) {
	for _, param := range []bool{true, false} {
		// job_read's own pipeline is unconfined: the job keeps its policy
		p := &Pipeline{Jobs: NewJobs(), SandboxParam: param}
		pol := &sandbox.Policy{Write: []string{"/pad/s1"}}
		out := &proc.Buffer{Max: 1 << 20}
		wait := make(chan error, 1)
		id := p.Jobs.Adopt("touch x", nil, out, 0, wait, pol, func() string { return "\nguard note" })
		out.Write([]byte("touch: /elsewhere/x: Operation not permitted\n"))
		read := func() string {
			res, _ := runJobRead(t.Context(), p, map[string]any{"id": id})
			return res.Content
		}
		if got := read(); !strings.Contains(got, "not permitted") {
			t.Fatalf("while running: %q", got)
		}
		wait <- errors.New("exit status 1")
		j, _ := p.Jobs.Get(id)
		<-j.done
		got := read()
		if !strings.Contains(got, "\nSandbox: commands write only in /pad/s1") ||
			strings.Contains(got, "sandbox: false") != param || !strings.Contains(got, "guard note") {
			t.Errorf("param %v: after exit %q", param, got)
		}
		if got := read(); strings.Contains(got, "Sandbox:") || strings.Contains(got, "guard note") {
			t.Errorf("param %v: the hint came twice: %q", param, got)
		}
	}
	// a job job_kill stopped died from zemi's signal, not the sandbox's
	p := &Pipeline{Jobs: NewJobs(), SandboxParam: true}
	c := proc.Command("sleep", "100")
	if err := proc.Start(c); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(c) }()
	id := p.Jobs.Adopt("sleep 100", c, &proc.Buffer{Max: 1 << 20}, 0, wait, &sandbox.Policy{}, func() string { return "" })
	runJobKill(t.Context(), p, map[string]any{"id": id})
	j, _ := p.Jobs.Get(id)
	<-j.done
	if res, _ := runJobRead(t.Context(), p, map[string]any{"id": id}); strings.Contains(res.Content, "Sandbox:") {
		t.Errorf("a killed job got the hint: %q", res.Content)
	}
}

// TestEscapesOnlyWhereOffered: shell and job_start name network and sandbox
// only when their schema carries them.
func TestEscapesOnlyWhereOffered(t *testing.T) {
	for _, param := range []bool{false, true} {
		for _, tool := range All(&Pipeline{SandboxParam: param}) {
			info := tool.Info()
			_, has := info.Parameters["sandbox"]
			if about, ok := info.Parameters["about"].(map[string]any); ok && strings.Contains(about["description"].(string), "sandbox") != has {
				t.Errorf("%s with the param %v: about %q", info.Name, param, about["description"])
			}
		}
	}
}

// TestSandboxHintFollowsPolicy: a denial gets one line naming the limit the
// denied path hit and the next step, offering only the escapes the run has:
// none without a UI, no sandbox: false for a read-only agent.
func TestSandboxHintFollowsPolicy(t *testing.T) {
	denied := errors.New("exit status 1")
	killed := exec.Command("sh", "-c", "exit 134").Run()
	network := "curl: (6) Could not resolve host: example.com"
	if !sandbox.LikelyDenied(network, denied) {
		t.Skip("no denial pattern on this platform")
	}
	b, _ := sandbox.Probe()
	proj := perm.RealPath(t.TempDir())
	t.Chdir(proj)
	t.Setenv("PATH", filepath.Join(proj, ".venv", "bin")+string(filepath.ListSeparator)+os.Getenv("PATH"))
	const escapes = ": network: true adds the network, sandbox: false drops every limit; both need approval."
	for _, c := range []struct {
		name, out              string
		plan, noParam, network bool
		noPad, skip, killed    bool
		gitOpen                bool
		want                   string
	}{
		{
			"planner writes the project", "touch: PROJ/a: Operation not permitted", true, false, false, false, false, false, false,
			"the project is read-only to this agent; describe the change instead of making it.",
		},
		{
			"network already given", network, false, false, true, false, false, false, false,
			"it had the network already; sandbox: false drops every limit (needs approval).",
		},
		{"network already given while planning", network, true, false, true, false, false, false, false, "it had the network already."},
		{
			"no network", network, false, false, false, false, false, false, false,
			"no network here: call it again with network: true, which keeps the rest of the sandbox (needs approval).",
		},
		{
			"hidden", "ls: /h/zemi/x: Operation not permitted", false, false, false, false, false, false, false,
			"/h/zemi is hidden from commands: read it with the read, glob and grep tools, or call it again with sandbox: false (needs approval).",
		},
		{
			"hidden credentials", "cat: /h/.ssh/id: Operation not permitted", false, false, false, false, false, false, false,
			"/h/.ssh holds credentials and is hidden from commands; if the command needs them, call it again with sandbox: false (needs approval).",
		},
		{
			"secret", "cat: PROJ/.env: Operation not permitted", false, false, false, false, !b.HidesSecretFiles, false, false,
			"secret-shaped files (.env, *.pem, *.key) are unreadable to commands, in $TMPDIR too: if the file holds no secret, give it another name; if the command needs a real one, call it again with sandbox: false (needs approval).",
		},
		{
			".agents", "mkdir: .agents/workflows: Operation not permitted", false, false, false, false, false, false, false,
			".agents is read-only to commands: create and edit files there with the write and edit tools.",
		},
		{
			".git", "fatal: Unable to create 'PROJ/.git/index.lock': Operation not permitted", false, false, false, false, false, false, false,
			".git is read-only to commands: git commands that change it ask first when they run on their own; otherwise call it again with sandbox: false (needs approval).",
		},
		{
			"a code part of an open .git", "error: could not lock config file PROJ/.git/config: Operation not permitted", false, false, false, false, false, false, true,
			".git/config is read-only to commands: git would run what it names; call it again with sandbox: false (needs approval).",
		},
		{
			"a PATH dir", "pip: PROJ/.venv/bin/x: Operation not permitted", false, false, false, false, false, false, false,
			".venv/bin is read-only to commands: zemi runs programs from it unconfined; call it again with sandbox: false (needs approval).",
		},
		{
			"outside the roots with no pad", "touch: /elsewhere/x: Operation not permitted", false, false, false, true, false, false, false,
			"commands write only in the project and $GOCACHE: write there, or call it again with sandbox: false (needs approval).",
		},
		{
			"outside the roots", "touch: /elsewhere/x: Operation not permitted", false, false, false, false, false, false, false,
			"commands write only in the project, $TMPDIR and $GOCACHE: write there, or call it again with sandbox: false (needs approval).",
		},
		{
			"outside the roots under -p", "touch: /elsewhere/x: Operation not permitted", false, true, false, false, false, false, false,
			"commands write only in the project, $TMPDIR and $GOCACHE: write there.",
		},
		{
			"killed by a signal", "Abort trap: 6", false, false, false, false, false, true, false,
			"it died from a signal: if it needs a system service (a browser, a GUI app, docker), call it again with sandbox: false (needs approval); a crash of its own repeats there.",
		},
		{
			"killed by a signal under -p", "Abort trap: 6", false, true, false, false, false, true, false,
			"it died from a signal: if it needs a system service (a browser, a GUI app, docker), it cannot run here: say what you could not check.",
		},
		{
			"nothing classified", "open x: operation not permitted", false, false, false, false, false, false, false,
			"a limit may have stopped it (Sandbox in the system prompt lists them)" + escapes,
		},
		{
			"nothing classified while planning", "open x: operation not permitted", true, false, false, false, false, false, false,
			"a limit may have stopped it (Sandbox in the system prompt lists them): network: true adds the network (needs approval).",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				t.Skip("the backend hides no secret-shaped files")
			}
			p := &Pipeline{SandboxParam: !c.noParam, Plan: func() bool { return c.plan }, Pad: "/pad/s1", Secret: func(path string) bool { return filepath.Base(path) == ".ssh" }}
			pol := &sandbox.Policy{
				Network: c.network,
				Write:   []string{proj, "/pad/s1", "/cache/go-build"},
				Protect: []string{filepath.Join(proj, ".agents"), filepath.Join(proj, ".git"), filepath.Join(proj, ".venv", "bin")},
				Hide:    []string{"/h/zemi", "/h/.ssh"},
				Env:     map[string]string{"TMPDIR": "/pad/s1", "TMP": "/pad/s1", "GOCACHE": "/cache/go-build"},
			}
			if c.gitOpen {
				pol.Git = filepath.Join(proj, ".git")
			}
			if c.noPad {
				p.Pad, pol.Write = "", slices.Delete(pol.Write, 1, 2)
			}
			err := denied
			if c.killed {
				err = killed
			}
			if hint := p.sandboxHint(pol, strings.ReplaceAll(c.out, "PROJ", proj), err); hint != "\nSandbox: "+c.want {
				t.Errorf("got %q\nwant %q", hint, "\nSandbox: "+c.want)
			}
		})
	}
}

// TestShellTimeoutBecomesJob: a command still running at its timeout keeps
// going as a job, so a slow build is not thrown away half done.
func TestShellTimeoutBecomesJob(t *testing.T) {
	t.Parallel()
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runShell(t.Context(), p, map[string]any{
		"command": "echo started; sleep 30", "timeout_ms": float64(400),
	})
	if res.IsError || !strings.Contains(res.Content, JobHandoff+"job-1") {
		t.Fatalf("timeout must hand off to a job: %+v", res)
	}
	if !strings.Contains(res.Content, "started") {
		t.Fatalf("output from before the timeout must survive: %q", res.Content)
	}
	if out, _ := runJobRead(t.Context(), p, map[string]any{"id": "job-1"}); !strings.Contains(out.Content, "running") {
		t.Fatalf("the job must still be running: %q", out.Content)
	}
	runJobKill(t.Context(), p, map[string]any{"id": "job-1"})
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
	res, _ := runShell(t.Context(), &Pipeline{}, map[string]any{"command": "sleep 30 &\necho started"})
	if res.IsError || !strings.HasPrefix(res.Content, "started\na background process it started was stopped") {
		t.Fatalf("orphan: %+v", res)
	}
}

// TestJobStreamsOutput: job_read shows a long-running process's output
// while it runs, and a shell line works without args.
func TestJobStreamsOutput(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runJobStart(t.Context(), p, map[string]any{"command": "echo ready && sleep 30"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := runJobRead(t.Context(), p, map[string]any{"id": "job-1"})
		if strings.Contains(out.Content, "ready") && strings.Contains(out.Content, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, _ := runJobRead(t.Context(), p, map[string]any{"id": "job-1"}); strings.Contains(out.Content, "ready") {
		t.Fatalf("a second read must not repeat delivered output: %q", out.Content)
	}
	runJobKill(t.Context(), p, map[string]any{"id": "job-1"})
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
	if res, _ := runJobStart(t.Context(), p, map[string]any{"command": "seq 1 4000"}); res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := runJobRead(t.Context(), p, map[string]any{"id": "job-1"})
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

// TestFileToolsSymlinkRace: a directory flipped between a real one and a
// symlink out of the jail while write, edit and read run never lets one of
// them act outside.
func TestFileToolsSymlinkRace(t *testing.T) {
	t.Parallel()
	proj, outside := outsideTemp(t), outsideTemp(t)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600)
	s := new(perm.Store)
	s.Default = func(subject, arg string) perm.Decision {
		if strings.HasPrefix(perm.RealPath(arg), proj+string(filepath.Separator)) {
			return perm.Allow
		}
		return perm.Deny
	}
	byName := map[string]fantasy.AgentTool{}
	for _, tl := range All(&Pipeline{Store: s}) {
		byName[tl.Info().Name] = tl
	}
	run := func(name string, in map[string]any) string {
		b, _ := json.Marshal(in, json.Deterministic(true))
		res, _ := byName[name].Run(t.Context(), fantasy.ToolCall{ID: "1", Name: name, Input: string(b)})
		return res.Content
	}
	d := filepath.Join(proj, "d")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.RemoveAll(d)
			os.Mkdir(d, 0o755)
			os.WriteFile(filepath.Join(d, "secret.txt"), []byte("mine"), 0o644)
			os.RemoveAll(d)
			os.Symlink(outside, d)
		}
	}()
	leaked := false
	for i := range 2000 {
		run("write", map[string]any{"path": filepath.Join(d, fmt.Sprintf("race%d.txt", i)), "content": "x"})
		run("edit", map[string]any{"path": filepath.Join(d, "secret.txt"), "old_string": "secret", "new_string": "owned"})
		leaked = leaked || strings.Contains(run("read", map[string]any{"path": filepath.Join(d, "secret.txt"), "limit": 0}), "\tsecret")
	}
	close(stop)
	<-done
	entries, _ := os.ReadDir(outside)
	got, _ := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if len(entries) != 1 || string(got) != "secret" || leaked {
		t.Fatalf("outside the jail: %d entries, secret %q, read leaked %v", len(entries), got, leaked)
	}
}

// TestShellResultText pins what the model reads back: binary is named, not
// sent, and a failure ends in its exit status.
func TestShellResultText(t *testing.T) {
	t.Parallel()
	_, noBackend := sandbox.Probe()
	for _, tc := range []struct {
		cmd, want string
		confined  bool
	}{
		{`printf 'a\0b'`, "(binary output, 3 bytes)", false},
		{"echo hi; exit 3", "hi\nexit status 3", false},
		// the shell's report of a crash a pipe hid from the exit status
		{"echo 'Abort trap: 6'", "Abort trap: 6\nSandbox: it died from a signal: if it needs a system service (a browser, a GUI app, docker), it cannot run here: say what you could not check.", true},
	} {
		p := &Pipeline{}
		if tc.confined {
			if noBackend != nil {
				continue
			}
			p.Sandboxed = &sandbox.Policy{}
		}
		res, _ := runShell(t.Context(), p, map[string]any{"command": tc.cmd})
		if res.Content != tc.want {
			t.Errorf("%s: %q, want %q", tc.cmd, res.Content, tc.want)
		}
	}
}

// TestShellProgress: the last output line reaches Progress while the command
// runs, and nothing clears it: the result replaces the row in one change.
func TestShellProgress(t *testing.T) {
	p, _ := testPipeline(t, true)
	ctx, stop := context.WithCancel(t.Context())
	var mu sync.Mutex
	var lines []string
	p.Progress = func(id, line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, id+":"+line)
		stop() // the line came while the command ran
	}
	// one write, so no poll lands between the lines
	input := `{"command":"printf 'one\\ntwo\\n'; sleep 30"}`
	if _, err := toolNamed(t, "shell", p).Run(ctx, fantasy.ToolCall{ID: "t1", Name: "shell", Input: input}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "t1:two" {
		t.Fatalf("progress lines = %q", lines)
	}
}

// TestShellStops: an interrupt or a timeout ends a command whose children
// keep its output pipe open, instead of waiting on them forever.
func TestShellStops(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		input  map[string]any
		cancel bool
		want   string
	}{
		{"interrupt", map[string]any{"command": "sleep 30 | cat"}, true, "interrupted"},
		{"timeout", map[string]any{"command": "sleep 30 & wait", "timeout_ms": float64(300)}, false, "timed out"},
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
