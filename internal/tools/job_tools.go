// job_tools.go ships the job_* built-ins: background processes the model
// can start, read, list and kill. Jobs share the Lua job discipline (DESIGN
// §6.5): the child env is the parent minus secret-shaped names, output
// lands in capped buffers, and the working directory is the project
// directory.

package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sandbox"
)

// JobHandoff opens the shell result note for a command that outlived its
// timeout; the chat widget keeps such a row by this phrase.
const JobHandoff = "moved to the background as "

// jobSettle is how long new job output must stay quiet before a job_read
// wait returns.
// NOTE: a build that pauses longer returns early and costs another read.
const jobSettle = 2 * time.Second

// Job is one managed background process.
type Job struct {
	ID     string
	Name   string
	stdout *proc.Buffer
	stderr *proc.Buffer
	cmd    *exec.Cmd
	done   chan struct{}
	code   int
	err    string
	werr   error // what Wait returned
	// policy confines the job, nil when unconfined; its failure may need
	// the sandbox hint, which names the policy's writable dirs
	policy *sandbox.Policy
	after  func() string // runs at exit (Pipeline.Guard)
	note   string        // what after returned
	told   bool          // the exit's hint and note were read
	killed bool          // Jobs.Kill ended it, so its signal is no sandbox's
	sent   [2]int64      // bytes of stdout/stderr already read, see since
	mu     sync.Mutex
}

func (j *Job) Running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// unread counts the bytes written since the last read without taking them.
func (j *Job) unread() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, out := j.stdout.Snapshot()
	_, errOut := j.stderr.Snapshot()
	return out - j.sent[0] + errOut - j.sent[1]
}

// Status renders one list line.
func (j *Job) Status() string {
	state := "running"
	if !j.Running() {
		// ExitCode is -1 when a signal ended the process: /agents kill, an
		// interrupt or the OS, never a status the command chose
		state = fmt.Sprintf("exited (%d)", j.code)
		if j.code < 0 {
			state = "killed"
		}
		if j.err != "" {
			state += " " + j.err
		}
	}
	return state
}

// Mark is Status as a hostapi.Marks key: spin while it runs, stop once
// killed, ok after a clean exit 0, else err.
func (j *Job) Mark() string {
	switch {
	case j.Running():
		return "spin"
	case j.code < 0:
		return "stop"
	case j.code == 0 && j.err == "":
		return "ok"
	}
	return "err"
}

// Output is what the job's buffers still hold, stdout then stderr.
func (j *Job) Output() string {
	so, _ := j.stdout.Snapshot()
	se, _ := j.stderr.Snapshot()
	return so + se
}

// since returns the output written after the previous read and the exit
// state: a job that is polled while it runs would otherwise resend its
// whole buffer every time.
func (j *Job) since() (out, errOut chunk, running bool, errText string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Count bytes ever written, not buffer length: a saturated buffer drops
	// its middle, so its length stops growing while output keeps arriving.
	// Whatever is new sits at the end of what the buffer still holds.
	rest := func(b *proc.Buffer, sent *int64) chunk {
		s, written := b.Snapshot()
		n := written - *sent
		*sent = written
		switch {
		case n <= 0:
			return chunk{}
		case n >= int64(len(s)):
			// all of it is new, and past Max the buffer dropped the middle
			return chunk{s, n - int64(b.Max)}
		}
		return chunk{s[int64(len(s))-n:], 0}
	}
	return rest(j.stdout, &j.sent[0]), rest(j.stderr, &j.sent[1]), j.Running(), j.err
}

// chunk is output a job_read has not returned yet; lost counts what the
// buffer dropped before it was read.
type chunk struct {
	text string
	lost int64
}

// Jobs owns every model-started job for one host.
type Jobs struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
	// OnExit runs after a job's exit is recorded: the statusline's job
	// count has nothing else to redraw it.
	OnExit func()
}

func NewJobs() *Jobs {
	return &Jobs{jobs: map[string]*Job{}}
}

// Start runs a shell line with proc.Shell in dir with env, confined by pol
// unless it is nil, and runs after once it exits; returns the
// job id. Output is captured as it arrives, so job_read shows a server's
// lines while it runs.
func (m *Jobs) Start(dir, line string, env []string, pol *sandbox.Policy, after func() string) (string, error) {
	if strings.TrimSpace(line) == "" {
		return "", errors.New("command is required")
	}
	cmd := proc.ShellCommand(line)
	cmd.Dir = dir
	cmd.Env = env
	j := &Job{
		Name: line, cmd: cmd, done: make(chan struct{}), policy: pol,
		stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20}, after: after,
	}
	cmd.Stdout, cmd.Stderr = j.stdout, j.stderr
	fail := func(err error) (string, error) {
		after()
		return "", err
	}
	if pol != nil {
		if err := sandbox.Wrap(cmd, *pol); err != nil {
			return fail(fmt.Errorf("the sandbox failed, so the command did not run: %w", err))
		}
	}
	if err := proc.Start(cmd); err != nil {
		return fail(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(cmd) }()
	return m.add(j, wait), nil
}

// Adopt registers a command that is already running, with the buffer it
// writes to, the byte count the caller already delivered from it, a channel
// carrying its Wait, its policy (nil when unconfined) and what runs at its
// exit: a shell call past its timeout keeps going as a job instead of losing
// the work done so far.
func (m *Jobs) Adopt(name string, cmd *exec.Cmd, out *proc.Buffer, sent int64, wait <-chan error, pol *sandbox.Policy, after func() string) string {
	j := &Job{Name: name, cmd: cmd, done: make(chan struct{}), stdout: out, stderr: &proc.Buffer{Max: 1 << 20}, policy: pol, after: after}
	j.sent[0] = sent
	return m.add(j, wait)
}

// add registers j under the next id and records its exit once wait delivers.
func (m *Jobs) add(j *Job, wait <-chan error) string {
	m.mu.Lock()
	m.seq++
	j.ID = fmt.Sprintf("job-%d", m.seq)
	m.jobs[j.ID] = j
	m.mu.Unlock()
	go func() {
		err := <-wait
		note := j.after()
		j.mu.Lock()
		j.werr, j.note = err, note
		switch exit, ok := errors.AsType[*exec.ExitError](err); {
		case ok:
			j.code = exit.ExitCode()
		case err != nil && !errors.Is(err, exec.ErrWaitDelay):
			j.err = err.Error()
		}
		j.mu.Unlock()
		close(j.done)
		if m.OnExit != nil {
			m.OnExit()
		}
	}()
	return j.ID
}

func (m *Jobs) Get(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("no such job %q", id)
	}
	return j, nil
}

// List returns every job in start order.
func (m *Jobs) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for i := 1; i <= m.seq; i++ {
		if j, ok := m.jobs[fmt.Sprintf("job-%d", i)]; ok {
			out = append(out, j)
		}
	}
	return out
}

// Running counts the jobs still running; a render reads it every frame.
func (m *Jobs) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		if j.Running() {
			n++
		}
	}
	return n
}

func (m *Jobs) Kill(id string) error {
	j, err := m.Get(id)
	if err != nil {
		return err
	}
	if !j.Running() {
		return fmt.Errorf("%s already exited", id)
	}
	j.mu.Lock()
	j.killed = true
	j.mu.Unlock()
	proc.Kill(j.cmd)
	return nil
}

func runJobStart(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	line := str(input["command"])
	env, err := p.childEnv()
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	id, err := p.Jobs.Start(workdir(), line, env, p.Sandboxed, p.guard())
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("started %s (%s)", id, line)), nil
}

func runJobRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	j, err := p.Jobs.Get(str(input["id"]))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// wait_ms blocks until the exit or until new output goes quiet, so a
	// chatty build costs one model step, not one per burst; the turn's
	// cancel still wins
	if v, ok := input["wait_ms"].(float64); ok && v > 0 {
		deadline := time.After(time.Duration(min(v, 600_000)) * time.Millisecond)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		seen, moved := j.unread(), time.Now()
		for j.Running() {
			select {
			case now := <-tick.C:
				if n := j.unread(); n != seen {
					seen, moved = n, now
					continue
				}
				if seen == 0 || now.Sub(moved) < jobSettle {
					continue
				}
			case <-j.done:
			case <-ctx.Done():
			case <-deadline:
			}
			break
		}
	}
	out, errOut, running, errText := j.since()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", j.ID, j.Status())
	if errText != "" {
		fmt.Fprintf(&b, "error: %s\n", errText)
	}
	// since() has already moved past what it returns, so a cut chunk is
	// gone unless CmdOut spills it
	// labels only when both streams have text: an adopted shell job's
	// output is merged into stdout
	both := out.text != "" && errOut.text != ""
	for _, c := range []struct {
		label string
		chunk
	}{{"[stdout]\n", out}, {"[stderr]\n", errOut}} {
		if c.text == "" {
			continue
		}
		if both {
			b.WriteString(c.label)
		}
		b.WriteString(p.cmdOut("job", c.text, c.lost) + "\n")
	}
	if running && out.text == "" && errOut.text == "" {
		b.WriteString("(no new output)")
	}
	if !running && j.policy != nil {
		j.mu.Lock()
		werr, note, told, killed := j.werr, j.note, j.told, j.killed
		j.told = true
		j.mu.Unlock()
		if !told && !killed {
			// the whole retained tail: a denial may have been read while it ran
			note = p.sandboxHint(j.policy, j.Output(), werr) + note
		}
		if !told {
			b.WriteString(note)
		}
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runJobList(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	jobs := p.Jobs.List()
	if len(jobs) == 0 {
		return fantasy.NewTextResponse("(no jobs)"), nil
	}
	var b strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&b, "%s: %s - %s\n", j.ID, j.Status(), j.Name)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runJobKill(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	id := str(input["id"])
	if err := p.Jobs.Kill(id); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse("killed " + id), nil
}
