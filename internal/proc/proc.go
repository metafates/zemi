// Package proc starts child processes so they can always be stopped. Each
// child gets its own session: it has no controlling terminal, so nothing it
// runs can read the TUI's keys or wait on a password prompt, and it leads a
// process group that cancelling kills whole (sh -c pipelines, dev servers
// and anything backgrounded with & included). Pipes still held by orphans
// stop blocking Wait after a short delay, what is left of a group dies when
// its command returns, and KillAll ends every group when zemi exits.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// waitDelay bounds how long Wait waits on pipes an orphaned grandchild
// keeps open after the command itself exited or was killed.
const waitDelay = 2 * time.Second

// NonInteractiveEnv keeps common tools from waiting on a person: no git
// credential prompts, pagers or editors, and no colors or progress bars
// spending the model's tokens.
var NonInteractiveEnv = []string{
	"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GH_PAGER=cat", "PAGER=cat", "GIT_EDITOR=true",
	"PIP_NO_INPUT=1", "DEBIAN_FRONTEND=noninteractive", "NO_COLOR=1", "TERM=dumb",
}

var (
	mu   sync.Mutex
	live = map[int]bool{}
)

// Command is exec.Command in its own session.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	setSession(cmd)
	cmd.WaitDelay = waitDelay
	return cmd
}

// CommandContext is exec.CommandContext whose cancellation kills the whole
// process group.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setSession(cmd)
	cmd.Cancel = func() error {
		killGroup(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = waitDelay
	return cmd
}

// Start starts cmd and remembers its group for KillAll.
func Start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	mu.Lock()
	live[cmd.Process.Pid] = true
	mu.Unlock()
	return nil
}

// Wait waits for cmd, then kills whatever its group left running.
// NOTE: the group id is the leader's pid; were it reused by an unrelated
// new session leader in the moment between exit and kill, that session
// would be hit. Linux and macOS allocate pids sequentially, so this needs a
// full wrap-around.
func Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid)
		mu.Lock()
		delete(live, cmd.Process.Pid)
		mu.Unlock()
	}
	return err
}

// Run is Start then Wait.
func Run(cmd *exec.Cmd) error {
	if err := Start(cmd); err != nil {
		return err
	}
	return Wait(cmd)
}

// Kill stops cmd's whole process group.
func Kill(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		killGroup(cmd.Process.Pid)
	}
}

// KillAll stops every group started through Start that has not been waited
// for: running commands, jobs and servers.
func KillAll() {
	mu.Lock()
	pids := make([]int, 0, len(live))
	for pid := range live {
		pids = append(pids, pid)
	}
	mu.Unlock()
	for _, pid := range pids {
		killGroup(pid)
	}
}

// Buffer keeps the first and last Max/2 bytes of what is written, so a
// command that floods output cannot exhaust memory and the end of a failing
// build still shows.
type Buffer struct {
	Max int

	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int64
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	half := max(b.Max/2, 1)
	rest := p
	if room := half - len(b.head); room > 0 {
		n := min(room, len(rest))
		b.head = append(b.head, rest[:n]...)
		rest = rest[n:]
	}
	if len(rest) == 0 {
		return len(p), nil
	}
	b.tail = append(b.tail, rest...)
	if over := len(b.tail) - half; over > 0 {
		b.dropped += int64(over)
		b.tail = append(b.tail[:0:0], b.tail[over:]...)
	}
	return len(p), nil
}

// Written counts every byte ever written, including what the middle drop
// removed. A reader that kept a previous count knows how much is new even
// though the buffer's length barely moves once it saturates.
func (b *Buffer) Written() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written()
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.str()
}

// Snapshot returns the contents and the write count together. Read one
// after the other, a write landing between them counts as delivered
// without being returned, and those bytes are never read again.
func (b *Buffer) Snapshot() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.str(), b.written()
}

func (b *Buffer) str() string {
	if b.dropped == 0 {
		return string(b.head) + string(b.tail)
	}
	var s bytes.Buffer
	s.Write(b.head)
	fmt.Fprintf(&s, "\n... (%d bytes of output skipped) ...\n", b.dropped)
	s.Write(b.tail)
	return s.String()
}

func (b *Buffer) written() int64 { return b.dropped + int64(len(b.head)+len(b.tail)) }
