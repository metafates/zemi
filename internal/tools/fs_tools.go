package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
)

// progress shows the command's last output line under its row while it
// runs, polled every 300 ms and sent only when it changed (each send
// redraws); the returned func stops the poll and clears the line, so a
// call handed on as a job keeps no stale one.
func (p *Pipeline) progress(ctx context.Context, buf *proc.Buffer) func() {
	id, _ := ctx.Value(callIDKey{}).(string)
	if p == nil || p.Progress == nil || id == "" {
		return func() {}
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		last := ""
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			raw, _ := buf.Snapshot()
			line := ""
			for l := range strings.SplitSeq(strings.TrimRight(ansi.Strip(raw), "\n"), "\n") {
				if strings.TrimSpace(l) != "" {
					line = l
				}
			}
			if line != last {
				last = line
				p.Progress(id, line)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		p.Progress(id, "")
	}
}

// runBash executes the command in the session directory with a timeout
// (default 60s, capped at 10min). Every call is a fresh process.
func runBash(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	cmd := str(input["command"])
	if strings.TrimSpace(cmd) == "" {
		return fantasy.NewTextErrorResponse("empty command"), nil
	}
	timeout := 60 * time.Second
	if v, ok := input["timeout_ms"].(float64); ok && v > 0 {
		// capped as a float: a huge value would overflow the Duration
		timeout = time.Duration(min(v, 600_000)) * time.Millisecond
	}
	// its own session and process group: cancelling or the timeout stops
	// pipelines and backgrounded children too, and nothing reads the TUI's tty.
	// Not bound to the turn's context: a command that outlives the timeout
	// becomes a job, which outlives the turn.
	c := proc.Command("sh", "-c", cmd)
	c.Dir = workdir()
	// secret-shaped variables stay out of the child, as they do for jobs:
	// the model reads whatever the command prints
	env, err := p.Store.ChildEnv(nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	c.Env = append(env, proc.NonInteractiveEnv...)
	// one buffer keeps stdout and stderr in the order they were written
	buf := &proc.Buffer{Max: 1 << 20}
	c.Stdout, c.Stderr = buf, buf
	if wrap := p.wrap(); wrap != nil {
		if err := wrap(c); err != nil {
			return fantasy.NewTextErrorResponse("the sandbox failed, so the command did not run: " + err.Error()), nil
		}
	}
	after := p.guard()
	finish := func() string {
		if after == nil {
			return ""
		}
		return after()
	}
	changes := func() (string, string, []string) { return "", "", nil }
	if p != nil && p.Changes != nil {
		changes = p.Changes(c.Dir)
	}
	changed := func(res fantasy.ToolResponse) (fantasy.ToolResponse, error) {
		note, diff, paths := changes()
		p.changed = paths
		if note != "" {
			res.Content += "\n" + note
		}
		res.Metadata = diff
		return res, nil
	}
	if err = proc.Start(c); err != nil {
		finish()
		return changed(fantasy.NewTextErrorResponse(err.Error()))
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(c) }()
	stopProgress := p.progress(ctx, buf)
	timedOut := false
	select {
	case err = <-wait:
	case <-ctx.Done():
		proc.Kill(c)
		<-wait
		err = ctx.Err()
	case <-time.After(timeout):
		timedOut = true
	}
	stopProgress()
	// the count goes with the text: job_read resumes exactly after it
	raw, written := buf.Snapshot()
	out := cmdText(raw)
	var b strings.Builder
	// compare the text, not lengths: the cut marker can outgrow a small cut
	if cut := TruncOut(out, cmdMaxOut); cut != out {
		b.WriteString(cut + p.spill("bash", out))
	} else {
		b.WriteString(out)
	}
	if timedOut {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// the work so far is not thrown away: it keeps running as a job
		if p != nil && p.Jobs != nil {
			id := p.Jobs.Adopt(cmd, c, buf, written, wait, after)
			fmt.Fprintf(&b, "still running after %s, so it %s%s: read the rest with job_read (wait_ms blocks until it prints or exits), stop it with job_kill.", timeout, JobHandoff, id)
			return changed(fantasy.NewTextResponse(b.String()))
		}
		proc.Kill(c)
		<-wait
		b.WriteString("timed out after " + timeout.String() + " and was stopped. Servers, watch modes and other commands that do not exit belong in job_start.")
		b.WriteString(finish())
		return changed(fantasy.ToolResponse{Type: "text", Content: b.String(), IsError: true})
	}
	note := finish()
	// the shell exited 0; a child it left behind held the pipe until
	// WaitDelay, and proc.Wait killed it
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
		note = "\na background process it started was stopped when the command returned; a server or watcher belongs in job_start" + note
	}
	if err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if ctx.Err() != nil {
			b.WriteString("stopped: the user interrupted the command")
		} else {
			b.WriteString(err.Error())
			if p != nil && p.Sandboxed != nil {
				b.WriteString(p.sandboxHint(raw, err))
			}
		}
		b.WriteString(note)
		return changed(fantasy.ToolResponse{Type: "text", Content: b.String(), IsError: true})
	}
	if b.Len() == 0 {
		b.WriteString("(no output)")
	}
	b.WriteString(note)
	return changed(fantasy.NewTextResponse(b.String()))
}

// cmdText is command output as the model reads it: like read and fetch,
// binary is named instead of sent as control bytes the model cannot use.
func cmdText(raw string) string {
	if strings.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0 {
		return fmt.Sprintf("(binary output, %d bytes)", len(raw))
	}
	return strings.TrimRight(ansi.Strip(raw), "\n")
}

// cmdMaxOut is what a command's output may take in a tool result; the rest
// is spilled to the scratchpad.
const cmdMaxOut = 10_000

// Spill saves output too long for a result under dir and names the file,
// so the cut part stays one read away; "" when it cannot be written.
func Spill(dir, name, s string) string {
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	f, err := os.CreateTemp(dir, name+"-*.txt")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		return ""
	}
	return "\n(full output: " + f.Name() + ")"
}

func (p *Pipeline) spill(name, s string) string {
	if p == nil || p.ScratchDir == nil {
		return ""
	}
	return Spill(p.ScratchDir(), name, s)
}

const (
	readMaxLines   = 2000
	readMaxBytes   = 50_000
	readMaxLineLen = 2000
	readMaxImage   = 5 << 20
	editMaxBytes   = 10 << 20
)

// runRead reads a file with optional 1-based line offset and limit. A page
// stops at readMaxLines or readMaxBytes and names the offset to continue
// from. It streams: the peak cost is the page, not the file, so a huge log
// pages like any other file instead of ending the process.
func runRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	f, fi, err := OpenRegular(path)
	switch {
	case fi != nil && fi.IsDir():
		return listDir(p, path), nil
	case err != nil:
		return fantasy.NewTextErrorResponse(p.notFound(path, err)), nil
	}
	defer f.Close()
	// every outcome of opening a file counts as seen, a page included: the
	// edit guard is about what changed under the model, an exact old_string
	// is its own proof, and a refused binary is all the model can know of a
	// file that write replaces whole
	p.markRead(path, fi)
	rel := workRel(path)
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(8000)
	switch mt := http.DetectContentType(head); {
	case mt == "image/png" || mt == "image/jpeg" || mt == "image/gif" || mt == "image/webp":
		if fi.Size() > readMaxImage {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("%s is a %d byte image, over the %d byte limit", rel, fi.Size(), readMaxImage)), nil
		}
		if p.Images != nil && !p.Images() {
			return fantasy.NewTextErrorResponse(rel + " is an image and the current model does not take images; go on without it, or ask the user to describe it or switch model"), nil
		}
		data, err := io.ReadAll(br)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewImageResponse(data, mt), nil
	case bytes.IndexByte(head, 0) >= 0:
		return fantasy.NewTextErrorResponse(rel + " is a binary file"), nil
	}
	start := 0
	if v, ok := input["offset"].(float64); ok && v > 1 {
		start = int(v) - 1
	}
	limit := readMaxLines
	if v, ok := input["limit"].(float64); ok && v > 0 {
		limit = min(int(v), limit)
	}
	var b strings.Builder
	n, kept := 0, 0
	for ; ; n++ {
		line, err := readLine(br, readMaxLineLen)
		if err != nil {
			break
		}
		if n < start {
			continue
		}
		row := fmt.Sprintf("%6d\t%s\n", n+1, line)
		if kept == limit || kept > 0 && b.Len()+len(row) > readMaxBytes {
			fmt.Fprintf(&b, "... (more lines; continue with offset %d)\n", n+1)
			break
		}
		b.WriteString(row)
		kept++
	}
	if kept == 0 {
		if start > 0 {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("offset %d is past the end of %s (%d lines)", start+1, rel, n)), nil
		}
		return fantasy.NewTextResponse("(empty file)"), nil
	}
	if p != nil && p.Instructions != nil {
		b.WriteString(p.Instructions(path))
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// readLine returns the next line without its ending, keeping at most max
// bytes and naming the cut: one endless line costs max bytes, not the file.
func readLine(br *bufio.Reader, max int) (string, error) {
	var line []byte
	cut := false
	for {
		chunk, err := br.ReadSlice('\n')
		if room := max - len(line); len(chunk) > room {
			chunk, cut = chunk[:room], true
		}
		line = append(line, chunk...)
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && len(line) == 0 && !cut:
			return "", err
		}
		s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if cut {
			s = strings.ToValidUTF8(s, "") + "... (line cut)"
		}
		return s, nil
	}
}

// listDir lists a directory one level deep, sorted, directories ending in
// "/". Like glob and grep it hides entries a deny or ask read rule covers,
// and secret-shaped names.
func listDir(p *Pipeline, path string) fantasy.ToolResponse {
	f, err := openGated(path, os.O_RDONLY, 0)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error())
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error())
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	ruledOut, shown := p.ruledOut(), []string{}
	for i, e := range entries {
		if len(shown) == globMaxFiles {
			shown = append(shown, fmt.Sprintf("... (up to %d more; narrow it with glob)", len(entries)-i))
			break
		}
		if name := e.Name(); !perm.PathIsSecret(name) && !ruledOut(filepath.Join(path, name)) {
			if e.IsDir() {
				name += "/"
			}
			shown = append(shown, name)
		}
	}
	if len(shown) == 0 {
		return fantasy.NewTextResponse("(empty directory)")
	}
	return fantasy.NewTextResponse(strings.Join(shown, "\n"))
}

// OpenRegular opens a file for reading through openGated. fi is set once
// the file is open, so a directory or a FIFO is reported without reading
// it; the caller closes f.
func OpenRegular(path string) (f *os.File, fi os.FileInfo, err error) {
	// nonblocking: opening a FIFO must not wait for a writer
	f, err = openGated(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		// a FIFO blocks and a device can be endless
		f.Close()
		return nil, fi, cmp.Or(err, error(&os.PathError{Op: "read", Path: path, Err: errors.New("not a regular file")}))
	}
	return f, fi, nil
}

// readGated reads a whole file, for the tools that need all of it (edit,
// multiedit). A file over editMaxBytes is refused rather than allocated:
// the edit would have to hold it twice over.
func readGated(path string) (data []byte, fi os.FileInfo, err error) {
	f, fi, err := OpenRegular(path)
	if err != nil {
		return nil, fi, err
	}
	defer f.Close()
	if fi.Size() > editMaxBytes {
		return nil, fi, fmt.Errorf("%s is %d bytes, too large to edit; narrow the change or split the file", workRel(path), fi.Size())
	}
	data, err = io.ReadAll(f)
	return data, fi, err
}

// writeGated replaces a file's content through openGated.
func writeGated(path string, data []byte, mode os.FileMode) error {
	f, err := openGated(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return cmp.Or(err, f.Close())
}

// markRead records the modification time the model saw path at; fi is
// taken before the read, so a change during it reads as stale.
func (p *Pipeline) markRead(path string, fi os.FileInfo) {
	if p != nil && p.Seen != nil && fi != nil {
		p.Seen.Store(absPath(path), fi.ModTime())
	}
}

// stale refuses changing a file the model has not read, or one that changed
// since (a command it ran, the user's editor, a formatter, another agent):
// writing over it would silently drop those changes. bash never updates
// Seen, so the model re-reads what a command rewrote. "" when the view is
// current.
func (p *Pipeline) stale(path string, fi os.FileInfo) string {
	rel := workRel(path)
	// before the read log: a directory listing never marks it seen, so the
	// "read it first" advice would loop
	if fi.IsDir() {
		return rel + " is a directory"
	}
	if p == nil || p.Seen == nil {
		return ""
	}
	t, ok := p.Seen.Load(absPath(path))
	switch {
	case !ok:
		return "read " + rel + " with the read tool first: this session has not read it, and cat or grep in bash does not count. Then repeat this edit."
	case !t.(time.Time).Equal(fi.ModTime()):
		return rel + " changed since you read it (a command you ran, the user's editor, a formatter or another agent). Read it again and redo the edit against the current content."
	}
	return ""
}

var fileLocks sync.Map // absolute path -> *sync.Mutex

// lockFile serializes read-modify-write of one file across concurrent
// agents and returns the unlock.
func lockFile(path string) func() {
	m, _ := fileLocks.LoadOrStore(absPath(path), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// callOrder keeps the write tools' calls on one file in the order the
// model made them: fantasy runs a parallel step's calls as goroutines, and
// the second of two edits to one file, run first, finds text the first was
// to write. Ids arrive from CallStart, which fantasy fires for every call of
// a step before it dispatches any; Run waits until its id leads its file.
// The maps are shared by every runner, so nothing here clears more than the
// ids a caller names.
var callOrder = struct {
	mu    sync.Mutex
	cond  *sync.Cond
	path  map[string]string   // call id -> file key
	ahead map[string][]string // file key -> call ids, in call order
}{path: map[string]string{}, ahead: map[string][]string{}}

// CallStart records a write tool's call as the model made it and reports
// whether it did; other tools are ignored. The agent runner calls it from
// fantasy's OnToolCall and owes a CallDone for every recorded id.
func CallStart(id, tool, input string) bool {
	if tool != ToolWrite && tool != ToolEdit && tool != ToolMultiedit {
		return false
	}
	var in struct{ Path string }
	if json.Unmarshal([]byte(input), &in) != nil || in.Path == "" {
		return false
	}
	key := absPath(in.Path)
	callOrder.mu.Lock()
	callOrder.path[id] = key
	callOrder.ahead[key] = append(callOrder.ahead[key], id)
	callOrder.mu.Unlock()
	return true
}

// CallDone releases the calls, so nothing waits on them: a call fantasy
// answered without running it (invalid input, a tool the profile dropped)
// and, at the end of a turn, whatever a step recorded and never dispatched.
// Ids Run already released are no-ops.
func CallDone(ids ...string) {
	c := &callOrder
	c.mu.Lock()
	for _, id := range ids {
		if key, ok := c.path[id]; ok {
			delete(c.path, id)
			c.ahead[key] = slices.DeleteFunc(c.ahead[key], func(x string) bool { return x == id })
		}
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

// turn blocks until call id leads the calls on its file, was never
// recorded, or ctx ends, and returns the release the caller runs once its
// write landed.
func turn(ctx context.Context, id string) func() {
	c := &callOrder
	// taking mu first means the waiter is inside Wait when the broadcast lands
	defer context.AfterFunc(ctx, func() { c.mu.Lock(); c.mu.Unlock(); c.cond.Broadcast() })()
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.path[id]
	for ok && c.ahead[key][0] != id && ctx.Err() == nil {
		c.cond.Wait()
		key, ok = c.path[id]
	}
	return func() { CallDone(id) }
}

func init() { callOrder.cond = sync.NewCond(&callOrder.mu) }

// absPath is the key both the read log and the file locks use, the same
// form fs rules match: an alias and its target, or main.go and Main.go
// where the filesystem folds case, take the same lock.
func absPath(path string) string { return perm.CanonPath(path) }

// runWrite creates or overwrites a file (parents created).
func runWrite(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	content := str(input["content"])
	defer lockFile(path)()
	if fi, err := os.Stat(path); err == nil {
		if msg := p.stale(path, fi); msg != "" {
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}
	p.checkpoint(path)
	if err := writeGated(path, []byte(content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("wrote %s (%d bytes)", workRel(path), len(content)) + p.wrote(path)), nil
}

func (p *Pipeline) afterWrite(path string) string {
	if p.AfterWrite == nil {
		return ""
	}
	return p.AfterWrite(path)
}

// wrote runs the hook before recording the file as seen: the hook may
// rewrite it, and the next edit must not find it stale.
func (p *Pipeline) wrote(path string) string {
	note := p.afterWrite(path)
	fi, _ := os.Stat(path)
	p.markRead(path, fi)
	return note
}

func (p *Pipeline) checkpoint(path string) {
	if p.Checkpoint != nil {
		p.Checkpoint(path)
	}
}

func runEdit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	return editFile(p, str(input["path"]), []any{input})
}

func runMultiedit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	edits, ok := input["edits"].([]any)
	if !ok || len(edits) == 0 {
		return fantasy.NewTextErrorResponse("edits must be a non-empty array"), nil
	}
	return editFile(p, str(input["path"]), edits)
}

// editFile applies edits to a file under its lock, all or nothing. Matching
// runs on LF text and a CRLF file is written back with CRLF.
func editFile(p *Pipeline, path string, edits []any) (fantasy.ToolResponse, error) {
	defer lockFile(path)()
	fi, err := os.Stat(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(p.notFound(path, err)), nil
	}
	if msg := p.stale(path, fi); msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	data, _, err := readGated(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error()), nil
	}
	rel := workRel(path)
	out, n, tolerant, msg := applyEdits(lf(string(data)), edits, rel)
	if msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// a mixed file takes its majority ending, so a stray line of the other
	// kind never rewrites every line
	if n := bytes.Count(data, []byte("\r\n")); 2*n > bytes.Count(data, []byte("\n")) {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	p.checkpoint(path)
	if err := writeGated(path, []byte(out), fi.Mode().Perm()); err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error()), nil
	}
	res := fmt.Sprintf("edited %s (%d replacement%s)", rel, n, plural(n))
	if tolerant.count > 0 {
		// the model never re-reads after a success, so what its text became
		// under the file's indentation has to be in the result
		res += "; matched ignoring whitespace and moved new_string to the file's indentation. Written as:" + numbered(strings.Split(out, "\n"), tolerant.at, tolerant.count)
	}
	return fantasy.NewTextResponse(res + p.wrote(path)), nil
}

// numbered renders lines[at:at+n] as read does, at most 30 of them.
func numbered(lines []string, at, n int) string {
	var b strings.Builder
	for j := at; j < min(at+n, at+30, len(lines)); j++ {
		fmt.Fprintf(&b, "\n%6d\t%s", j+1, lines[j])
	}
	return b.String()
}

// notFound words a path error; for a missing file it names up to three
// siblings a typo could have meant (app.go for apps.go or ap.go), leaving
// out secret-shaped and ruled-out names as listDir does.
func (p *Pipeline) notFound(path string, err error) string {
	msg := relErr(err).Error()
	if !errors.Is(err, os.ErrNotExist) {
		return msg
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	// a dotfile's stem is its whole name: an empty one would be two edits
	// from any short name, and dotfiles sort first
	stemOf := func(name string) string {
		if ext := filepath.Ext(name); ext != name {
			return strings.TrimSuffix(name, ext)
		}
		return name
	}
	stem := stemOf(strings.ToLower(filepath.Base(path)))
	ruledOut, dir := p.ruledOut(), filepath.Dir(path)
	var near []string
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		s := stemOf(name)
		if len(near) == 3 || e.IsDir() || perm.PathIsSecret(name) || ruledOut(filepath.Join(dir, e.Name())) {
			continue
		}
		if s == stem || len(stem) >= 3 && (strings.Contains(s, stem) || strings.Contains(stem, s)) || editDistance(s, stem) <= 2 {
			near = append(near, e.Name())
		}
	}
	if len(near) > 0 {
		msg += " (did you mean " + strings.Join(near, ", ") + "?)"
	}
	return msg
}

// editDistance is the Levenshtein distance between two short names.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := []int{i}
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur = append(cur, min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost))
		}
		prev = cur
	}
	return prev[len(b)]
}

// relErr shortens the path in an os error the way the results do.
func relErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		pe.Path = workRel(pe.Path)
	}
	return err
}

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// region is where a whitespace-tolerant edit put its new lines: the last
// such edit's, in the text as it is once every edit applied.
// NOTE: an exact edit above it in a later multiedit step shifts the lines.
type region struct{ at, count int }

// applyEdits applies edits in order to LF text. msg explains the first edit
// that does not apply, and then nothing changes.
func applyEdits(src string, edits []any, where string) (out string, n int, tolerant region, msg string) {
	for i, e := range edits {
		em, _ := e.(map[string]any)
		old, new := lf(str(em["old_string"])), lf(str(em["new_string"]))
		all, _ := em["replace_all"].(bool)
		res, c := applyEdit(src, old, new, all)
		if c == 0 && old != "" {
			var at int
			if res, c, at = applyTolerant(src, old, new, all); c > 0 {
				tolerant = region{at, strings.Count(strings.Trim(new, "\n"), "\n") + 1}
			}
		}
		m := editError(src, old, c, all, where)
		if old != "" && old == new {
			m = "old_string and new_string are the same; nothing to change"
		}
		if m != "" {
			if len(edits) > 1 {
				m = fmt.Sprintf("edit %d (nothing was written): %s", i+1, m)
			}
			return src, 0, region{}, m
		}
		src, n = res, n+c
	}
	return src, n, tolerant, ""
}

// applyEdit replaces old with new in s and returns the match count. With
// several matches and !all nothing changes: guessing which one was meant
// edits the wrong place silently.
func applyEdit(s, old, new string, all bool) (string, int) {
	n := strings.Count(s, old)
	if n == 0 || (n > 1 && !all) {
		return s, n
	}
	return strings.ReplaceAll(s, old, new), n
}

// editError explains a failed match, or returns "" when the edit applied.
func editError(src, old string, n int, all bool, where string) string {
	switch {
	case old == "":
		return "old_string is empty; use write to create a file"
	case n > 1 && !all:
		return fmt.Sprintf("old_string matches %d places in %s; add surrounding lines to make it unique, or set replace_all", n, where)
	case n > 0:
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimLeft(old, "\n"), "\n")
	if first = strings.TrimSpace(first); first != "" {
		lines := strings.Split(src, "\n")
		at, hits := 0, 0
		for i, l := range lines {
			if strings.TrimSpace(l) == first {
				if hits == 0 {
					at = i
				}
				hits++
			}
		}
		// the file's lines only when the hit is unique: a common first line
		// like "}" would show the wrong block
		if hits > 1 {
			return fmt.Sprintf("old_string not found in %s; its first line matches line %d, so a later line differs: read from there and copy the text exactly", where, at+1)
		}
		if hits == 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "old_string not found in %s; its first line matches line %d, so a later line differs. The file there reads (copy it without the line numbers):", where, at+1)
			n := min(strings.Count(strings.Trim(old, "\n"), "\n")+1, 30)
			for j := at; j < min(at+n, len(lines)); j++ {
				fmt.Fprintf(&b, "\n%6d\t%s", j+1, lines[j])
			}
			return b.String()
		}
	}
	return "old_string not found in " + where + "; read the file again and copy the text exactly"
}

// applyTolerant matches whole lines ignoring whitespace runs, and moves
// new_string to the file's indentation: the indent old_string used on each
// matched line maps to the indent the file has there. first is the line
// the first match's new text starts on.
func applyTolerant(src, old, new string, all bool) (out string, count, first int) {
	simp := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	lead := func(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }
	srcLines := strings.Split(src, "\n")
	oldLines := strings.Split(strings.Trim(old, "\n"), "\n")
	var newLines []string
	if t := strings.Trim(new, "\n"); t != "" {
		newLines = strings.Split(t, "\n")
	}
	m := len(oldLines)
	var b []string
	at := 0
	for i := 0; i+m <= len(srcLines); i++ {
		if !slices.EqualFunc(srcLines[i:i+m], oldLines, func(s, o string) bool { return simp(s) == simp(o) }) {
			continue
		}
		indent := map[string]string{}
		for j, o := range oldLines {
			if strings.TrimSpace(o) != "" {
				indent[lead(o)] = lead(srcLines[i+j])
			}
		}
		if count == 0 {
			first = i
		}
		b = append(b, srcLines[at:i]...)
		for _, l := range newLines {
			from := ""
			for o := range indent {
				if len(o) > len(from) && strings.HasPrefix(l, o) {
					from = o
				}
			}
			to, ok := indent[from]
			if !ok {
				to = from
			}
			b = append(b, to+l[len(from):])
		}
		count++
		at = i + m
		i += m - 1
	}
	if count == 0 || (count > 1 && !all) {
		return src, count, 0
	}
	return strings.Join(append(b, srcLines[at:]...), "\n"), count, first
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const (
	globMaxFiles   = 200
	grepMaxMatches = 100
)

// globMaxScan is where a glob stops collecting; a var so a test can hit it
var globMaxScan = 20_000

// runGlob lists files under root (default session dir) matching pattern,
// newest first: recently changed files are the likely targets.
func runGlob(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern, root := str(input["pattern"]), searchRoot(input)
	var rels []string
	collect := func(rel string) bool {
		rels = append(rels, rel)
		return len(rels) < globMaxScan
	}
	if !ripgrep(ctx, root, collect, "--files", "--glob", pattern) {
		rels = rels[:0]
		match, err := glob.Match(pattern)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		err = walk(ctx, root, func(path, rel string) error {
			if match(rel) && !collect(rel) {
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	if len(rels) == 0 {
		return fantasy.NewTextResponse("(no matches)"), nil
	}
	mod := make(map[string]time.Time, len(rels))
	for i, rel := range rels {
		rels[i] = filepath.ToSlash(filepath.Join(root, rel))
		if fi, err := os.Stat(rels[i]); err == nil {
			mod[rels[i]] = fi.ModTime()
		}
	}
	slices.SortStableFunc(rels, func(a, b string) int { return mod[b].Compare(mod[a]) })
	// a check resolves symlinks, so only as many hits are checked as are shown
	ruledOut, shown := p.ruledOut(), rels[:0]
	for i, rel := range rels {
		if len(shown) == globMaxFiles {
			// the scan stopped at globMaxScan, so the rest is unknown; below
			// it "up to" holds, since ruledOut may hide some of the remainder
			if len(rels) == globMaxScan {
				shown = append(shown, fmt.Sprintf("... (scan stopped at %d files; narrow the pattern or path)", globMaxScan))
			} else {
				shown = append(shown, fmt.Sprintf("... (up to %d more; narrow the pattern or path)", len(rels)-i))
			}
			break
		}
		if !ruledOut(rel) {
			shown = append(shown, rel)
		}
	}
	if len(shown) == 0 {
		return fantasy.NewTextResponse("(no matches)"), nil
	}
	return fantasy.NewTextResponse(strings.Join(shown, "\n")), nil
}

// runGrep searches file contents under root, optionally only in files
// matching the include glob. Secret-shaped files are never searched. A
// pattern that is no regexp (an unbalanced paren) is searched as text.
func runGrep(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern, root, include := str(input["pattern"]), searchRoot(input), str(input["include"])
	re, err := regexp.Compile(pattern)
	literal := err != nil
	if literal {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern))
	}
	// a file root is searched as one file of its directory, so rel keeps
	// its shape
	file := ""
	if fi, err := os.Stat(root); err == nil && fi.Mode().IsRegular() {
		root, file = filepath.Dir(root), filepath.Base(root)
	}
	var hits []string
	hidden, ruledOut := map[string]bool{}, p.ruledOut()
	add := func(rel string, line int, text string) bool {
		path := filepath.Join(root, rel)
		h, ok := hidden[rel]
		if !ok {
			h = perm.PathIsSecret(rel) || ruledOut(path)
			hidden[rel] = h
		}
		if !h {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(path), line, truncLine(text)))
		}
		return len(hits) < grepMaxMatches
	}
	args := []string{"--line-number", "--no-heading", "--null", "--max-columns", "1000", "--max-columns-preview", "--regexp", pattern}
	if literal {
		args = append(args, "--fixed-strings")
	}
	if include != "" {
		args = append(args, "--glob", include)
	}
	if file != "" {
		args = append(args, "--with-filename", "--", file)
	}
	ok := ripgrep(ctx, root, func(out string) bool {
		rel, rest, _ := strings.Cut(out, "\x00")
		n, text, _ := strings.Cut(rest, ":")
		line, _ := strconv.Atoi(n)
		return add(rel, line, text)
	}, args...)
	if !ok {
		hits = hits[:0]
		match := func(string) bool { return true }
		if include != "" {
			if match, err = glob.Match(include); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
		}
		visit := func(path, rel string) error {
			if !match(rel) || !isTextFile(path) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) && !add(rel, i+1, line) {
					return filepath.SkipAll
				}
			}
			return nil
		}
		if file != "" {
			err = visit(filepath.Join(root, file), file)
		} else {
			err = walk(ctx, root, visit)
		}
		if err != nil && !errors.Is(err, filepath.SkipAll) {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	out := ""
	if literal {
		out = "(pattern taken literally)\n"
	}
	if len(hits) == 0 {
		return fantasy.NewTextResponse(out + "(no matches)"), nil
	}
	out += strings.Join(hits, "\n") + "\n"
	if len(hits) >= grepMaxMatches {
		out += fmt.Sprintf("... (stopped at %d matches; narrow the pattern, path or include)", grepMaxMatches)
	}
	return fantasy.NewTextResponse(out), nil
}

// ListFiles lists up to limit files under root, relative to it, in no
// particular order. Hidden directories like .github are listed only when
// rg is installed; the walker fallback skips them.
func ListFiles(root string, limit int) []string {
	var files []string
	collect := func(rel string) bool {
		files = append(files, rel)
		return len(files) < limit
	}
	if !ripgrep(context.Background(), root, collect, "--files") {
		files = files[:0]
		walk(context.Background(), root, func(_, rel string) error {
			if !collect(rel) {
				return filepath.SkipAll
			}
			return nil
		})
	}
	return files
}

// ruledOut reports whether a deny or ask rule covers reading path: glob
// and grep are gated on their root only, so each hit meets the read rules.
func (p *Pipeline) ruledOut() func(path string) bool {
	if p == nil || p.Store == nil {
		return func(string) bool { return false }
	}
	eval := p.Store.Evaluator("fs.read", p.Principal)
	return func(path string) bool {
		o := eval(path)
		return o.Rule != nil && o.Decision != perm.Allow
	}
}

// rgBin is ripgrep when installed: it honors .gitignore and is much faster
// than the Go walker, which stays as the fallback.
var rgBin, _ = exec.LookPath("rg")

// ripgrep streams rg's output lines, run in root, to fn until fn returns
// false. false means rg is missing or failed and the caller falls back.
func ripgrep(ctx context.Context, root string, fn func(line string) bool, args ...string) bool {
	if rgBin == "" {
		return false
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := proc.CommandContext(cctx, rgBin, append([]string{"--hidden", "--no-require-git", "--glob", "!.git", "--glob", "!.zemi/worktrees"}, args...)...)
	c.Dir = root
	out, err := c.StdoutPipe()
	if err != nil || proc.Start(c) != nil {
		return false
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(nil, 1<<20)
	stopped := false
	for sc.Scan() {
		if !fn(sc.Text()) {
			stopped = true
			break
		}
	}
	// an unread pipe would block rg forever
	if stopped || sc.Err() != nil {
		cancel()
	}
	err = proc.Wait(c)
	var exit *exec.ExitError
	return stopped || err == nil || (errors.As(err, &exit) && exit.ExitCode() == 1)
}

// walk visits the regular files under root for the Go fallback, which skips
// hidden and generated directories instead of reading .gitignore.
func walk(ctx context.Context, root string, fn func(path, rel string) error) error {
	// WalkDir does not follow a symlinked root, and would visit nothing
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	// the handler below drops errors so an unreadable subdirectory is skipped,
	// and a missing root would read as no matches
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("no such directory: %s", root)
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || skipDir(d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		return fn(path, rel)
	})
}

func truncLine(s string) string {
	if len(s) > 200 {
		return strings.ToValidUTF8(s[:200], "") + "..."
	}
	return s
}

// isTextFile rejects obviously binary files by NUL in the first 512 bytes.
func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return !bytes.ContainsRune(buf[:n], 0)
}

func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build",
		"__pycache__", "coverage", "venv":
		return true
	}
	return false
}

// workdir is the bash execution root.
func workdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
