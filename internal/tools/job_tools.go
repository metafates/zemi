// job_tools.go ships the job_* built-ins: background processes the model
// can start, read, list and kill. Jobs share the Lua job discipline (DESIGN
// §6.5): the child env is the full parent environ plus the non-interactive
// vars, output lands in capped buffers, and the working directory is the
// project directory.

package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/proc"
)

// JobHandoff opens the shell result note for a command that outlived its
// timeout; the host stamps such a row "bg" by this phrase.
const JobHandoff = "moved to the background as "

// _maxKeptJobs is how many finished jobs a host keeps before the oldest go;
// a running job's id must stay resolvable for job_read, so only finished
// ones are evicted.
const _maxKeptJobs = 50

// _jobSettle is how long new job output must stay quiet before a job_read
// wait returns.
// NOTE: a build that pauses longer returns early and costs another read.
const _jobSettle = 2 * time.Second

// A waitless read of a quiet running job holds for these instead of making
// the model poll one roundtrip per quiet stretch: a build that bursts output
// rides to its exit, a silent server answers after the hold.
const (
	_jobQuietHold = 10 * time.Second
	_jobQuietCap  = 20 * time.Second
)

// waitJob holds until the job exits, its output has been quiet for quiet,
// or the ceiling passes or the turn's cancel fires.
func waitJob(ctx context.Context, j *Job, quiet, ceiling time.Duration) {
	deadline := time.NewTimer(ceiling)
	defer deadline.Stop()
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
			if seen == 0 || now.Sub(moved) < quiet {
				continue
			}
		case <-j.done:
		case <-ctx.Done():
		case <-deadline.C:
		}
		return
	}
}

// Job is one managed background process.
type Job struct {
	ID       string
	Name     string // the model's about for it, else the command line
	Cmd      string // the command line, which the job's view shows as bash
	Started  time.Time
	stdout   *proc.Buffer
	stderr   *proc.Buffer
	cmd      *exec.Cmd
	done     chan struct{}
	code     int
	err      string
	werr     error    // what Wait returned
	sent     [2]int64 // bytes of stdout/stderr already read, see since
	finished time.Time
	// announced: the exit's OnChange has run, so a sweep may evict; guarded
	// by Jobs.mu, which the exit goroutine holds when it sets it
	announced bool
	mu        sync.Mutex
}

func (j *Job) Running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// Elapsed is how long the job has run: to now while it runs, else to its end.
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.elapsed()
}

func (j *Job) elapsed() time.Duration {
	if j.Running() {
		return time.Since(j.Started)
	}
	return j.finished.Sub(j.Started)
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

// MarkKind is the state word a row asks its mark by: what Job.Mark answers
// and the key hostapi's Marks draws a glyph from.
type MarkKind string

const (
	MarkSpin    MarkKind = "spin" // drawn with the "spin" effect, which steps its glyph through Spinner
	MarkOK      MarkKind = "ok"
	MarkWarn    MarkKind = "warn"
	MarkErr     MarkKind = "err"
	MarkStop    MarkKind = "stop"
	MarkBG      MarkKind = "bg"
	MarkWait    MarkKind = "wait"
	MarkThought MarkKind = "thought"
	MarkNote    MarkKind = "note"
)

// Mark is Status as a Marks key: spin while it runs, stop once
// killed, ok after a clean exit 0, else err.
func (j *Job) Mark() MarkKind {
	switch {
	case j.Running():
		return MarkSpin
	case j.code < 0:
		return MarkStop
	case j.code == 0 && j.err == "":
		return MarkOK
	default:
		return MarkErr
	}
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
		default:
			return chunk{s[int64(len(s))-n:], 0}
		}
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
	mu       sync.Mutex
	seq      int
	jobs     map[string]*Job
	watching bool // the screens' one ticker runs
	// OnChange runs after a job starts or its exit is recorded: the screens
	// that list them have nothing else to redraw or re-time them.
	OnChange func()
}

// Start runs a shell line with proc.Shell in dir with env and returns the
// job id. Output is captured as it arrives, so job_read shows a server's
// lines while it runs. name is what the user sees it as, the model's about
// for it, falling back to the line.
func (m *Jobs) Start(dir, name, line string, env []string) (string, error) {
	if strings.TrimSpace(line) == "" {
		return "", errors.New("command is required")
	}
	cmd := proc.ShellCommand(line)
	cmd.Dir = dir
	cmd.Env = env
	j := &Job{
		Name: cmp.Or(name, line), Cmd: line, Started: time.Now(), cmd: cmd, done: make(chan struct{}),
		stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20},
	}
	cmd.Stdout, cmd.Stderr = j.stdout, j.stderr
	if err := proc.Start(cmd); err != nil {
		return "", err
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(cmd) }()
	return m.add(j, wait), nil
}

// Adopt registers a command that is already running, with the buffer it
// writes to, the byte count the caller already delivered from it, when it
// started and a channel carrying its Wait: a shell call past its timeout
// keeps going as a job instead of losing the work done so far. name is what
// the user sees it as (the shell call's about), line the command itself.
func (m *Jobs) Adopt(name, line string, cmd *exec.Cmd, out *proc.Buffer, sent int64, started time.Time, wait <-chan error) string {
	j := &Job{Name: cmp.Or(name, line), Cmd: line, Started: started, cmd: cmd, done: make(chan struct{}), stdout: out, stderr: &proc.Buffer{Max: 1 << 20}}
	j.sent[0] = sent
	return m.add(j, wait)
}

// add registers j under the next id and records its exit once wait delivers.
func (m *Jobs) add(j *Job, wait <-chan error) string {
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = map[string]*Job{}
	}
	m.seq++
	j.ID = "job-" + strconv.Itoa(m.seq)
	m.jobs[j.ID] = j
	m.mu.Unlock()
	if m.OnChange != nil {
		m.OnChange()
	}
	go func() {
		err := <-wait
		j.mu.Lock()
		j.werr = err
		j.finished = time.Now()
		switch exit, ok := errors.AsType[*exec.ExitError](err); {
		case ok:
			j.code = exit.ExitCode()
		case err != nil && !errors.Is(err, exec.ErrWaitDelay):
			j.err = err.Error()
		}
		j.mu.Unlock()
		close(j.done)
		// the exit announces while the job is still listed: a sibling's
		// sweep that saw it finished would evict it unannounced, and an
		// open view of it would never leave "running"
		if m.OnChange != nil {
			m.OnChange()
		}
		m.mu.Lock()
		j.announced = true
		m.evictLocked()
		m.mu.Unlock()
	}()
	return j.ID
}

// evictLocked drops the oldest announced-finished jobs past maxKeptJobs; a
// finished job whose exit has not been announced yet is its exit goroutine's
// to announce and evict. Callers hold m.mu.
func (m *Jobs) evictLocked() {
	finished := 0
	for _, j := range m.jobs {
		if j.announced {
			finished++
		}
	}
	for id := 1; finished > _maxKeptJobs && id <= m.seq; id++ {
		key := "job-" + strconv.Itoa(id)
		j, ok := m.jobs[key]
		if !ok || !j.announced {
			continue
		}
		delete(m.jobs, key)
		finished--
	}
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
		if j, ok := m.jobs["job-"+strconv.Itoa(i)]; ok {
			out = append(out, j)
		}
	}
	return out
}

// Running counts the jobs still running; a render reads it every frame.
func (m *Jobs) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningLocked()
}

// runningLocked is Running with m.mu held.
func (m *Jobs) runningLocked() int {
	n := 0
	for _, j := range m.jobs {
		if j.Running() {
			n++
		}
	}
	return n
}

// ClaimWatch gives the caller the one ticker that keeps the job screens
// current: false while another holds it or no job runs to refresh.
func (m *Jobs) ClaimWatch() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watching || m.runningLocked() == 0 {
		return false
	}
	m.watching = true
	return true
}

// ReleaseWatch gives the ticker back and reports whether no job runs now, so
// the caller stops; a start that came first claims a new one.
func (m *Jobs) ReleaseWatch() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watching = false
	return m.runningLocked() == 0
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
	proc.Kill(j.cmd)
	j.mu.Unlock()
	return nil
}

// KillAll stops every job still running and returns how many it stopped.
func (m *Jobs) KillAll() int {
	stopped := 0
	for _, j := range m.List() {
		if j.Running() && m.Kill(j.ID) == nil {
			stopped++
		}
	}
	return stopped
}

// jobStartArgs is the job_start tool's input and its schema source.
type jobStartArgs struct {
	About   string `json:"about,omitempty" desc:"what the process is in 3-6 words, shown to the user, e.g. Dev server."`
	Command string `json:"command"`
}

func runJobStart(ctx context.Context, p *Pipeline, in jobStartArgs) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	line := in.Command
	id, err := p.Jobs.Start(workdir(), in.About, line, p.cmdEnv())
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	p.outcome(ctx, Outcome{Job: id})
	return fantasy.NewTextResponse(fmt.Sprintf("started %s (%s)", id, line)), nil
}

// jobReadArgs is the job_read tool's input and its schema source.
type jobReadArgs struct {
	ID     string `json:"id"`
	WaitMS int    `json:"wait_ms,omitempty" desc:"wait until the job exits, or goes quiet for 2s after printing, at most this long (max 600000, default 0)"`
}

func runJobRead(ctx context.Context, p *Pipeline, in jobReadArgs) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	j, err := p.Jobs.Get(in.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// wait_ms blocks until the exit or until new output goes quiet, so a
	// chatty build costs one model step, not one per burst; the turn's
	// cancel still wins
	if in.WaitMS > 0 {
		waitJob(ctx, j, _jobSettle, time.Duration(min(in.WaitMS, 600_000))*time.Millisecond)
	} else if j.Running() {
		waitJob(ctx, j, _jobQuietHold, _jobQuietCap)
	}
	out, errOut, running, errText := j.since()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", j.ID, j.Status())
	if running {
		// the read already held through a quiet stretch: a job this long
		// wants one wait_ms, not another poll
		b.WriteString("still running: pass wait_ms to hold until it exits (the hold returns early when output pauses 2s)\n")
	}
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
	// a job that exited badly: the result is an error, so the model does not
	// read its exit as done and its row is no green check (Job.Mark says err)
	if j.Mark() == MarkErr {
		return fantasy.NewTextErrorResponse(b.String()), nil
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// jobListArgs is the job_list tool's input and its schema source.
type jobListArgs struct{}

func runJobList(ctx context.Context, p *Pipeline, _ jobListArgs) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	jobs := p.Jobs.List()
	if len(jobs) == 0 {
		return fantasy.NewTextResponse("(no jobs)"), nil
	}
	var b strings.Builder
	for _, j := range jobs {
		name := j.Name
		if j.Cmd != "" && j.Cmd != j.Name {
			name = name + " (" + j.Cmd + ")"
		}
		fmt.Fprintf(&b, "%s: %s  %s\n", j.ID, j.Status(), name)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// jobKillArgs is the job_kill tool's input and its schema source.
type jobKillArgs struct {
	ID string `json:"id"`
}

func runJobKill(ctx context.Context, p *Pipeline, in jobKillArgs) (fantasy.ToolResponse, error) {
	if p.ReadOnly != nil && p.ReadOnly() {
		// job_kill makes no gate request, so the gate's read-only deny cannot reach it
		return fantasy.NewTextErrorResponse(DeniedText(_readOnlyVerdict, string(ToolJobKill)+" "+in.ID)), nil
	}
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	id := in.ID
	if err := p.Jobs.Kill(id); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse("killed " + id), nil
}
