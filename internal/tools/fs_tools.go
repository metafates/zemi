package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // FitImage decodes the gifs read and attach send
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// redraws); the returned func stops the poll. The line stays until the
// result replaces it, so the row changes once.
func (p *Pipeline) progress(ctx context.Context, buf *proc.Buffer) func() {
	id, _ := ctx.Value(callIDKey{}).(string)
	if p.Progress == nil || id == "" {
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
			for l := range strings.SplitSeq(cmdText(raw), "\n") {
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
	}
}

// runShell executes the command in the session directory with a timeout
// (default 60s, capped at 10min). Every call is a fresh process.
func runShell(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
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
	c := proc.ShellCommand(cmd)
	c.Dir = workdir()
	c.Env = p.cmdEnv()
	// one buffer keeps stdout and stderr in the order they were written
	buf := &proc.Buffer{Max: 1 << 20}
	c.Stdout, c.Stderr = buf, buf
	changes := func() (string, string, []string) { return "", "", nil }
	if p.Changes != nil {
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
	if err := proc.Start(c); err != nil {
		return changed(fantasy.NewTextErrorResponse(err.Error()))
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(c) }()
	stopProgress := p.progress(ctx, buf)
	timedOut := false
	var err error
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
	var b strings.Builder
	b.WriteString(p.cmdOut("shell", raw, written-int64(buf.Max)))
	if timedOut {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// the work so far is not thrown away: it keeps running as a job
		if p.Jobs != nil {
			id := p.Jobs.Adopt(cmd, c, buf, written, wait)
			fmt.Fprintf(&b, "still running after %s, so it %s%s: read the rest with job_read (wait_ms blocks until it exits or goes quiet), stop it with job_kill.", timeout, JobHandoff, id)
			return changed(fantasy.NewTextResponse(b.String()))
		}
		proc.Kill(c)
		<-wait
		b.WriteString("timed out after " + timeout.String() + " and was stopped. Servers, watch modes and other commands that do not exit belong in job_start.")
		return changed(fantasy.ToolResponse{Type: "text", Content: b.String(), IsError: true})
	}
	// the shell exited 0; a child it left behind held the pipe until
	// WaitDelay, and proc.Wait killed it
	note := ""
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
		note = "\na background process it started was stopped when the command returned; a server or watcher belongs in job_start"
	}
	if err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if ctx.Err() != nil {
			b.WriteString("stopped: the user interrupted the command")
		} else {
			b.WriteString(err.Error())
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

// ShellFailure is why a failed command failed, for a reader that has only the
// result text (zemi run's --json error): the last line runShell writes a
// reason on, else the command's own last output line. A response carries no
// field for it.
func ShellFailure(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	reason := func(l string) bool {
		return strings.HasPrefix(l, "exit status ") || strings.HasPrefix(l, "signal: ") ||
			strings.HasPrefix(l, "stopped: ") || strings.HasPrefix(l, "timed out ")
	}
	for _, l := range slices.Backward(lines) {
		if reason(l) {
			return l
		}
	}
	return lines[len(lines)-1]
}

// imageMaxSide and imageMaxBytes are what an image sent to a model may be:
// Anthropic refuses over 8000 px a side (2000 with more than 20 images) and
// over 5 MB of base64, about 3.75 MB of bytes.
const (
	imageMaxSide  = 2000
	imageMaxBytes = 3_750_000
	// NOTE: a decode holds 4 to 8 bytes a pixel, and a 1 MB PNG can declare
	// 30000x30000; past this ceiling an image is refused, not tiled.
	imageMaxPixels = 40_000_000
)

// FitImage returns an image a provider takes: as it is when it fits, else
// shrunk to imageMaxSide with a box filter and re-encoded as PNG, or as
// JPEG when the PNG is still too big. An error says why none would fit.
// NOTE: the stdlib decodes no webp (x/image does), so a webp is judged by
// its bytes alone.
func FitImage(data []byte, mt string) ([]byte, string, error) {
	tooBig := fmt.Errorf("a %d byte image, too large to send even shrunk", len(data))
	if mt == "image/webp" {
		if len(data) > imageMaxBytes {
			return nil, "", fmt.Errorf("a %d byte webp image, over the %d byte limit", len(data), imageMaxBytes)
		}
		return data, mt, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("not a readable image: %w", err)
	}
	long := max(cfg.Width, cfg.Height)
	if long <= imageMaxSide && len(data) <= imageMaxBytes {
		return data, mt, nil
	}
	if cfg.Width*cfg.Height > imageMaxPixels {
		return nil, "", fmt.Errorf("a %dx%d image, over the %d megapixels zemi decodes", cfg.Width, cfg.Height, imageMaxPixels/1_000_000)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("not a readable image: %w", err)
	}
	w, h := cfg.Width, cfg.Height
	if long > imageMaxSide {
		w, h = max(w*imageMaxSide/long, 1), max(h*imageMaxSide/long, 1)
	}
	small := boxScale(src, w, h)
	var buf bytes.Buffer
	if png.Encode(&buf, small) == nil && buf.Len() <= imageMaxBytes {
		return buf.Bytes(), "image/png", nil
	}
	buf.Reset()
	if jpeg.Encode(&buf, small, &jpeg.Options{Quality: 85}) == nil && buf.Len() <= imageMaxBytes {
		return buf.Bytes(), "image/jpeg", nil
	}
	return nil, "", tooBig
}

// boxScale averages each cell of src into one pixel of a w x h image.
func boxScale(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// one band of source rows at a time goes through draw's fast paths
	// (RGBA, NRGBA, YCbCr, Gray), so a screenshot never calls At per pixel
	band := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()/h+1))
	for y := range h {
		y0, y1 := y*b.Dy()/h, max((y+1)*b.Dy()/h, y*b.Dy()/h+1)
		draw.Draw(band, image.Rect(0, 0, b.Dx(), y1-y0), src, image.Pt(b.Min.X, b.Min.Y+y0), draw.Src)
		for x := range w {
			x0, x1 := x*b.Dx()/w, max((x+1)*b.Dx()/w, x*b.Dx()/w+1)
			var sum [4]int
			for yy := range y1 - y0 {
				row := band.Pix[yy*band.Stride+x0*4 : yy*band.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					sum[0], sum[1], sum[2], sum[3] = sum[0]+int(row[i]), sum[1]+int(row[i+1]), sum[2]+int(row[i+2]), sum[3]+int(row[i+3])
				}
			}
			n, o := (x1-x0)*(y1-y0), dst.PixOffset(x, y)
			for c := range 4 {
				dst.Pix[o+c] = uint8(sum[c] / n)
			}
		}
	}
	return dst
}

// sniffBytes is how much of a body looksBinary reads.
const sniffBytes = 8000

// looksBinary is the sniff read, fetch and command output share: a NUL near
// the start, as git decides.
func looksBinary[T string | []byte](b T) bool {
	return strings.IndexByte(string(b[:min(len(b), sniffBytes)]), 0) >= 0
}

// cmdText is command output as the model reads it: like read and fetch,
// binary is named instead of sent as control bytes the model cannot use.
func cmdText(raw string) string {
	if looksBinary(raw) {
		return fmt.Sprintf("(binary output, %d bytes)", len(raw))
	}
	return strings.TrimRight(lastFrames(ansi.Strip(raw)), "\n")
}

// lastFrames keeps what a terminal shows of each line: the text after its
// last carriage return, so a progress bar is its final frame. NOTE: a frame
// shorter than the one it overwrites loses the old frame's tail, which a
// terminal would still show; progress bars pad their frames.
func lastFrames(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		lines[i] = l[strings.LastIndexByte(l, '\r')+1:]
	}
	return strings.Join(lines, "\n")
}

// resultMaxOut is what a tool result may carry of a command's or a fetch's
// output; the rest is spilled to the scratchpad. A skill body has its own cap.
const resultMaxOut = 10_000

// Cut cuts tool output to n and spills the whole text to dir, so the cut part
// stays one read away and its line numbers are read offsets. whole keeps the
// text as it is: a call a lua script drove keeps its output whole, since the
// script reads it and says what of it the model gets.
func Cut(dir, name, s string, n int, whole bool) string {
	if whole {
		return s
	}
	if out := TruncOut(s, n); out != s {
		return out + Spill(dir, name, s)
	}
	return s
}

// cut is Cut for this run.
func (p *Pipeline) cut(name, s string, n int) string { return Cut(p.Pad, name, s, n, p.Script != "") }

// CmdOut is a command's output as a result carries it: cmdText cut to
// resultMaxOut, the kept text spilled to dir. lost counts what the proc.Buffer
// already dropped from the middle, which no spill file can hold.
func CmdOut(dir, name, raw string, lost int64, whole bool) string {
	out := Cut(dir, name, cmdText(raw), resultMaxOut, whole)
	if lost > 0 {
		out += fmt.Sprintf("\n(%d more bytes in the middle were not kept)", lost)
	}
	return out
}

// cmdOut is CmdOut for this run.
func (p *Pipeline) cmdOut(name, raw string, lost int64) string {
	return CmdOut(p.Pad, name, raw, lost, p.Script != "")
}

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
	return "\n(output saved to " + f.Name() + ")"
}

// cmdEnv is the environment of a command the model starts: the pad is its
// temp dir, so a file it leaves there is where the prompt says. The touch
// tells another process's sweep the pad is used.
func (p *Pipeline) cmdEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, proc.NonInteractiveEnv...)
	if p.Pad != "" && os.MkdirAll(p.Pad, 0o700) == nil {
		now := time.Now()
		_ = os.Chtimes(p.Pad, now, now)
		env = append(env, "TMPDIR="+p.Pad, "TMP="+p.Pad, "TEMP="+p.Pad)
	}
	return env
}

const (
	readMaxLines   = 2000
	readMaxBytes   = 50_000
	readMaxLineLen = 2000
	readMaxImage   = 20 << 20 // what is read at all; FitImage sizes what is sent
	editMaxBytes   = 10 << 20
)

// runRead reads a file with optional 1-based line offset and limit. A page
// stops at readMaxLines or readMaxBytes and names the offset to continue
// from. It streams: the peak cost is the page, not the file, so a huge log
// pages like any other file instead of ending the process.
func runRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := p.Path.String()
	f, fi, err := OpenRegular(p.Path)
	switch {
	case fi != nil && fi.IsDir():
		return listDir(p.Path), nil
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
	head, _ := br.Peek(sniffBytes)
	switch mt := http.DetectContentType(head); {
	case mt == "image/png" || mt == "image/jpeg" || mt == "image/gif" || mt == "image/webp":
		if fi.Size() > readMaxImage {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("%s is a %d byte image, over the %d byte limit", rel, fi.Size(), readMaxImage)), nil
		}
		if p.Images != nil && !p.Images() {
			return fantasy.NewTextErrorResponse(rel + " is an image and the current model does not take images, so a screenshot is no use either; go on without it, or ask the user to describe it or switch model"), nil
		}
		data, err := io.ReadAll(br)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if data, mt, err = FitImage(data, mt); err != nil {
			return fantasy.NewTextErrorResponse(rel + ": " + err.Error()), nil
		}
		return fantasy.NewImageResponse(data, mt), nil
	case looksBinary(head):
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
	var rows []string
	size := 0
	more := 0
	for ; ; n++ {
		line, err := readLine(br, readMaxLineLen)
		if err != nil {
			break
		}
		if n < start {
			continue
		}
		// the number prefix is at most 4 wide, so +6 bounds the row overhead
		if kept == limit || kept > 0 && size+len(line)+6 > readMaxBytes {
			more = n + 1
			break
		}
		rows = append(rows, line)
		size += len(line)
		kept++
	}
	if kept == 0 {
		if start > 0 {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("offset %d is past the end of %s (%d lines)", start+1, rel, n)), nil
		}
		return fantasy.NewTextResponse("(empty file)"), nil
	}
	// the gutter is as wide as the last line number, not a fixed field
	width := len(strconv.Itoa(start + kept))
	for i, line := range rows {
		fmt.Fprintf(&b, "%*d\t%s\n", width, start+i+1, line)
	}
	if more != 0 {
		fmt.Fprintf(&b, "... (more lines; continue with offset %d)\n", more)
	}
	if p.Instructions != nil {
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
// "/".
func listDir(path perm.Path) fantasy.ToolResponse {
	f, err := path.Open(os.O_RDONLY, 0)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error())
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error())
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	var shown []string
	for i, e := range entries {
		if len(shown) == globMaxFiles {
			shown = append(shown, fmt.Sprintf("... (up to %d more; narrow it with glob)", len(entries)-i))
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		shown = append(shown, name)
	}
	if len(shown) == 0 {
		return fantasy.NewTextResponse("(empty directory)")
	}
	return fantasy.NewTextResponse(strings.Join(shown, "\n"))
}

// OpenRegular opens a file for reading. fi is set once the file is open,
// so a directory or a FIFO is reported without reading it; the caller
// closes f.
func OpenRegular(path perm.Path) (f *os.File, fi os.FileInfo, err error) {
	// nonblocking: opening a FIFO must not wait for a writer
	f, err = path.Open(os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		// a FIFO blocks and a device can be endless
		f.Close()
		return nil, fi, cmp.Or(err, error(&os.PathError{Op: "read", Path: path.String(), Err: errors.New("not a regular file")}))
	}
	return f, fi, nil
}

// readGated reads a whole file for edit. A file over editMaxBytes is refused
// rather than allocated: the edit would have to hold it twice over.
func readGated(path perm.Path) (data []byte, fi os.FileInfo, err error) {
	f, fi, err := OpenRegular(path)
	if err != nil {
		return nil, fi, err
	}
	defer f.Close()
	if fi.Size() > editMaxBytes {
		return nil, fi, fmt.Errorf("%s is %d bytes, over edit's %d MB limit; make this change with a shell command", workRel(path.String()), fi.Size(), editMaxBytes>>20)
	}
	data, err = io.ReadAll(f)
	return data, fi, err
}

// WriteFile replaces a file's content, making missing parent directories.
func WriteFile(path perm.Path, data []byte, mode os.FileMode) error {
	f, err := path.Open(os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return cmp.Or(err, f.Close())
}

// markRead records the modification time the model saw path at; fi is
// taken before the read, so a change during it reads as stale.
func (p *Pipeline) markRead(path string, fi os.FileInfo) {
	if p.Seen != nil && fi != nil {
		p.Seen.Store(perm.CanonPath(path), fi.ModTime())
	}
}

// stale refuses writing a file the model has not read, or one that changed
// since (a command it ran, the user's editor, a formatter, another agent):
// writing over it would silently drop those changes. A shell call never updates
// Seen, so the model re-reads what a command rewrote. Nor does an
// edit: runEdit holds the edit of a file whose current version the model has
// not read to one exact match, which proves it knows the text it replaces
// and keeps every other change, and leaves the file unseen. "" when the view
// is current.
func (p *Pipeline) stale(path string, fi os.FileInfo, edit bool) string {
	rel := workRel(path)
	// before the read log: a directory listing never marks it seen, so the
	// "read it first" advice would loop
	if fi.IsDir() {
		return rel + " is a directory"
	}
	if p.Seen == nil {
		return ""
	}
	v, _ := p.Seen.Load(perm.CanonPath(path))
	t, ok := v.(time.Time) // Seen is written from two packages; a non-time reads as unseen
	switch {
	case edit:
	case !ok:
		return "read " + rel + " with the read tool first, then repeat this call: this session has not read it, and cat, sed or grep in a shell command does not count."
	case !t.Equal(fi.ModTime()):
		return rel + " changed since you read it (a command you ran, the user's editor, a formatter or another agent), and writing it whole would drop that change: read it again first."
	}
	return ""
}

// unseen reports a file whose current version the session has not read.
func (p *Pipeline) unseen(path string, fi os.FileInfo) bool {
	if p.Seen == nil {
		return false
	}
	v, _ := p.Seen.Load(perm.CanonPath(path))
	t, ok := v.(time.Time)
	return !ok || !t.Equal(fi.ModTime())
}

var fileLocks sync.Map // perm.CanonPath -> *sync.Mutex

// lockFile serializes read-modify-write of one file across concurrent
// agents and returns the unlock.
func lockFile(path string) func() {
	m, _ := fileLocks.LoadOrStore(perm.CanonPath(path), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// callOrder keeps the write tools' calls on one file in the order the model
// made them, though fantasy runs a step's calls in parallel: CallStart records
// every call of a step before any runs, and Run waits until its id leads its
// file. Shared by every runner, it clears only the ids a caller names, keyed by
// the turn's CallScope: a provider may number calls per conversation.
var callOrder = struct {
	mu    sync.Mutex
	cond  *sync.Cond
	path  map[string]string   // call key -> file key
	ahead map[string][]string // file key -> call keys, in call order
}{path: map[string]string{}, ahead: map[string][]string{}}

type callScopeKey struct{}

var callScopes atomic.Uint64

// CallScope gives the calls a turn makes under ctx their own keys.
func CallScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, callScopeKey{}, callScopes.Add(1))
}

func callKey(ctx context.Context, id string) string {
	n, _ := ctx.Value(callScopeKey{}).(uint64)
	return strconv.FormatUint(n, 10) + "\x00" + id
}

// CallStart records a write tool's call as the model made it and reports
// whether it did; other tools are ignored. The agent runner calls it from
// fantasy's OnToolCall and owes a CallDone for every recorded id.
func CallStart(ctx context.Context, id, tool, input string) bool {
	if tool != ToolWrite && tool != ToolEdit {
		return false
	}
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.Path == "" {
		return false
	}
	key, id := perm.CanonPath(in.Path), callKey(ctx, id)
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
func CallDone(ctx context.Context, ids ...string) {
	c := &callOrder
	c.mu.Lock()
	for _, id := range ids {
		id = callKey(ctx, id)
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
	defer context.AfterFunc(ctx, func() { c.mu.Lock(); c.cond.Broadcast(); c.mu.Unlock() })()
	c.mu.Lock()
	defer c.mu.Unlock()
	k := callKey(ctx, id)
	key, ok := c.path[k]
	for ok && c.ahead[key][0] != k && ctx.Err() == nil {
		c.cond.Wait()
		key, ok = c.path[k]
	}
	return func() { CallDone(ctx, id) }
}

func init() { callOrder.cond = sync.NewCond(&callOrder.mu) }

// runWrite creates or overwrites a file (parents created).
func runWrite(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := p.Path.String()
	content := str(input["content"])
	defer lockFile(path)()
	if fi, err := os.Stat(path); err == nil {
		if msg := p.stale(path, fi, false); msg != "" {
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}
	if err := p.writeThrough(p.Path, []byte(content), 0o644); err != nil {
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

// checkpoint leaves out the pad, which is no project a rewind restores.
func (p *Pipeline) checkpoint(path string) {
	if p.Checkpoint != nil && !perm.InPad(p.Pad, path) {
		p.Checkpoint(path)
	}
}

// writeThrough is how every file tool writes: the prior bytes are
// checkpointed first. The caller records the file as read after, once any
// afterWrite hook has rewritten it.
func (p *Pipeline) writeThrough(path perm.Path, data []byte, mode os.FileMode) error {
	p.checkpoint(path.String())
	return WriteFile(path, data, mode)
}

// runEdit applies an edit to a file under its lock. Matching runs on LF
// text and a CRLF file is written back with CRLF.
func runEdit(ctx context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
	path := p.Path.String()
	defer lockFile(path)()
	fi, err := os.Stat(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(p.notFound(path, err)), nil
	}
	if msg := p.stale(path, fi, true); msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	data, _, err := readGated(p.Path)
	if err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error()), nil
	}
	rel := workRel(path)
	out, n, tolerant, msg := editText(lf(string(data)), in, rel)
	if msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	unseen := p.unseen(path, fi)
	if unseen && (n != 1 || tolerant.count > 0) {
		return fantasy.NewTextErrorResponse("read " + rel + " with the read tool first (the lines you change are enough), then repeat this call: this session has not read its current version, so old_string must match exactly once as written."), nil
	}
	// a mixed file takes its majority ending, so a stray line of the other
	// kind never rewrites every line
	if n := bytes.Count(data, []byte("\r\n")); 2*n > bytes.Count(data, []byte("\n")) {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := p.writeThrough(p.Path, []byte(out), fi.Mode().Perm()); err != nil {
		return fantasy.NewTextErrorResponse(relErr(err).Error()), nil
	}
	res := fmt.Sprintf("edited %s (%d replacement%s)", rel, n, plural(n))
	if tolerant.count > 0 {
		// the model never re-reads after a success, so what its text became
		// under the file's indentation has to be in the result
		res += "; matched ignoring whitespace and moved new_string to the file's indentation. Written as:" + numbered(strings.Split(out, "\n"), tolerant.at, tolerant.count)
	}
	if unseen {
		// the match proves only the replaced text: a write must still read first
		return fantasy.NewTextResponse(res + p.afterWrite(path)), nil
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

// notFound words a path error; for a missing file it names the three
// siblings nearest what a typo could have meant (app.go for apps.go or
// ap.go).
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
	base := strings.ToLower(filepath.Base(path))
	stem := stemOf(base)
	type sibling struct {
		name       string
		stem, full int // edit distances
	}
	var near []sibling
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		s := stemOf(name)
		if e.IsDir() {
			continue
		}
		if d := editDistance(s, stem); s == stem || len(stem) >= 3 && (strings.Contains(s, stem) || strings.Contains(stem, s)) || d <= 2 {
			near = append(near, sibling{name: e.Name(), stem: d, full: editDistance(name, base)})
		}
	}
	slices.SortStableFunc(near, func(a, b sibling) int { return cmp.Or(a.stem-b.stem, a.full-b.full) })
	if len(near) > 0 {
		names := make([]string, 0, 3)
		for _, s := range near[:min(3, len(near))] {
			names = append(names, s.name)
		}
		msg += " (did you mean " + strings.Join(names, ", ") + "?)"
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
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		pe.Path = workRel(pe.Path)
	}
	return err
}

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// region is where a whitespace-tolerant edit put its new lines.
type region struct{ at, count int }

// editText applies an edit's old_string and new_string to LF text; msg
// explains why it does not apply, and then out is to be ignored.
func editText(src string, in map[string]any, where string) (out string, n int, tolerant region, msg string) {
	old, new := lf(str(in["old_string"])), lf(str(in["new_string"]))
	all, _ := in["replace_all"].(bool)
	out, n = applyEdit(src, old, new, all)
	if n == 0 && old != "" {
		var at int
		if out, n, at = applyTolerant(src, old, new, all); n > 0 {
			tolerant = region{at, strings.Count(strings.Trim(new, "\n"), "\n") + 1}
		}
	}
	msg = editError(src, old, n, all, where)
	if old != "" && old == new {
		msg = "old_string and new_string are the same; nothing to change"
	}
	return out, n, tolerant, msg
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
		var hits []string
		at := 0
		// whole lines first; Contains only when none match, since the model
		// may start old_string mid-line
		for _, match := range []func(string) bool{
			func(l string) bool { return strings.TrimSpace(l) == first },
			func(l string) bool { return strings.Contains(l, first) },
		} {
			for i, l := range lines {
				if match(l) {
					if len(hits) == 0 {
						at = i
					}
					if hits = append(hits, strconv.Itoa(i+1)); len(hits) == 4 {
						hits[3] = "..."
						break
					}
				}
			}
			if len(hits) > 0 {
				break
			}
		}
		// the file's lines only when the hit is unique: a common first line
		// like "}" would show the wrong block
		if len(hits) > 1 {
			return fmt.Sprintf("old_string not found in %s; its first line matches lines %s, so a later line differs: read the block you meant and copy it exactly", where, strings.Join(hits, ", "))
		}
		if len(hits) == 1 {
			return fmt.Sprintf("old_string not found in %s; its first line matches line %d, so a later line differs. The file there reads (copy it without the line numbers):", where, at+1) +
				numbered(lines, at, strings.Count(strings.Trim(old, "\n"), "\n")+1)
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
	// ListLimit caps a glob, grep or zemi.fs listing, so a walk of a huge
	// tree outside any repository ends.
	ListLimit = 200_000
	// grepMaxSize is the largest file grep reads.
	// NOTE: a bigger file (a bundle, a dump, a huge lock file) is not
	// searched; a streaming line reader would lift the cap, at the price of
	// the one pass over the whole file that rules most files out.
	grepMaxSize = 4 << 20
)

// walkedNote tells the model that walk, not git, picked the files it sees.
const walkedNote = "(no git file list here: .gitignore not read, hidden and build directories skipped)"

// listWait bounds git's file listing. git walks every untracked file before
// it prints one, which in a huge untracked tree (a $HOME dotfiles repo) runs
// for seconds; the biggest repository measured, 26k files, lists in 0.3 s cold.
var listWait = 2 * time.Second

// runGlob lists files under root (default session dir) matching pattern,
// newest first: recently changed files are the likely targets.
func runGlob(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern, root := strings.TrimPrefix(str(input["pattern"]), "./"), searchRoot(input)
	// the gate saw only root, so an absolute pattern may not reach past it
	if filepath.IsAbs(pattern) {
		abs, _ := filepath.Abs(root)
		rel, ok := perm.Under(abs, pattern)
		if !ok {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("%s is not under %s: pass its directory as path and a pattern relative to it", pattern, root)), nil
		}
		pattern = filepath.ToSlash(rel)
	}
	match, err := glob.Match(pattern)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	files, note, err := ListFiles(ctx, root, ListLimit)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	var rels []string
	mod := map[string]time.Time{}
	for _, f := range files {
		if match(f) {
			rel := filepath.ToSlash(filepath.Join(root, f))
			rels = append(rels, rel)
			if fi, err := os.Lstat(rel); err == nil {
				mod[rel] = fi.ModTime()
			}
		}
	}
	slices.SortFunc(rels, func(a, b string) int { return cmp.Or(mod[b].Compare(mod[a]), strings.Compare(a, b)) })
	var out []string
	if note != "" {
		out = append(out, note)
	}
	shown := 0
	for i, rel := range rels {
		if shown == globMaxFiles {
			out = append(out, fmt.Sprintf("... (up to %d more; narrow the pattern or path)", len(rels)-i))
			break
		}
		out = append(out, rel)
		shown++
	}
	if shown == 0 {
		out = append(out, "(no matches)")
	}
	if len(files) == ListLimit {
		out = append(out, fmt.Sprintf("... (listing stopped at %d files; narrow the path)", ListLimit))
	}
	return fantasy.NewTextResponse(strings.Join(out, "\n")), nil
}

// runGrep searches file contents under root, optionally only in files
// matching the include glob. A pattern that is no regexp (an unbalanced
// paren) is searched as text.
func runGrep(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern, root, include := str(input["pattern"]), searchRoot(input), str(input["include"])
	re, err := regexp.Compile(pattern)
	literal := err != nil
	if literal {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern))
	}
	match := func(string) bool { return true }
	if include != "" {
		if match, err = glob.Match(include); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	name := func(rel string) string { return filepath.ToSlash(filepath.Join(root, rel)) }
	var files []string
	note := ""
	if fi, err := os.Stat(root); err == nil && fi.Mode().IsRegular() {
		// Grep opens no symbolic link, but one named as path was asked for
		given, real := filepath.ToSlash(filepath.Clean(root)), root
		if r, err := filepath.EvalSymlinks(root); err == nil {
			real = r
		}
		root, files = filepath.Dir(real), []string{filepath.Base(real)}
		name = func(string) string { return given }
	} else if files, note, err = ListFiles(ctx, root, ListLimit); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	capped := len(files) == ListLimit
	files = slices.DeleteFunc(files, func(rel string) bool { return !match(rel) })
	hits, err := Grep(ctx, root, files, re, grepMaxMatches, nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	var out []string
	if note != "" {
		out = append(out, note)
	}
	if literal {
		out = append(out, "(pattern taken literally)")
	}
	for _, h := range hits {
		out = append(out, fmt.Sprintf("%s:%d: %s", name(h.Rel), h.Line, truncLine(h.Text)))
	}
	switch {
	case len(hits) == grepMaxMatches:
		out = append(out, fmt.Sprintf("... (stopped at %d matches; narrow the pattern, path or include)", grepMaxMatches))
	case capped:
		out = append(out, fmt.Sprintf("... (searched the first %d files; narrow the path)", ListLimit))
	}
	if len(hits) == 0 {
		out = append(out, "(no matches)")
	}
	return fantasy.NewTextResponse(strings.Join(out, "\n")), nil
}

// ListFiles lists up to limit regular files under root, relative to it, in
// path order: in a git work tree what git tracks or would track (the
// .gitignore files read), else what walk finds, with note the line saying
// why. Neither lists a symbolic link, .git or a nested repository.
// git and not a Go matcher: only the index knows a tracked file an ignore
// rule covers, and snapshots and lsp sync read git's rules too.
func ListFiles(ctx context.Context, root string, limit int) (files []string, note string, err error) {
	gitCtx, cancel := context.WithTimeout(ctx, listWait)
	defer cancel()
	out, err := proc.Git{Dir: root}.Run(gitCtx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	// an index entry is the repository's word: a planted one can say "../x"
	// or "sub/.git/config", and a tracked directory may since have become a
	// symbolic link
	realDir := map[string]bool{".": true}
	var inRoot func(dir string) bool
	inRoot = func(dir string) bool {
		ok, seen := realDir[dir]
		if !seen {
			fi, err := os.Lstat(filepath.Join(root, dir))
			ok = err == nil && fi.IsDir() && filepath.Base(dir) != ".git" && inRoot(filepath.Dir(dir))
			realDir[dir] = ok
		}
		return ok
	}
	for rel := range strings.SplitSeq(out, "\x00") {
		// a nested repository comes as its directory, a deleted or a
		// submodule's entry as no regular file
		if err != nil || strings.HasSuffix(rel, "/") || strings.HasPrefix(rel, ".agents/worktrees/") ||
			!filepath.IsLocal(rel) || filepath.Clean(rel) != rel || !inRoot(filepath.Dir(rel)) {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(root, rel)); err == nil && fi.Mode().IsRegular() {
			files = append(files, rel)
		}
	}
	// nothing listed: no repository, no git, or a root git ignores whole
	if len(files) == 0 {
		note = walkedNote
		if ctx.Err() == nil && errors.Is(gitCtx.Err(), context.DeadlineExceeded) {
			note = fmt.Sprintf("(git took over %s to list the files here, so a walk did: .gitignore not read, hidden and build directories skipped)", listWait)
		}
		err = walk(ctx, root, func(rel string) error {
			files = append(files, filepath.ToSlash(rel))
			if len(files) == limit {
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return nil, note, err
		}
	}
	slices.Sort(files)
	// an unmerged path is listed once per stage
	files = slices.Compact(files)
	return files[:min(len(files), limit)], note, nil
}

// Hit is one line Grep found, Rel relative to the root it searched.
type Hit struct {
	Rel  string
	Line int
	Text string
}

// Grep returns the first limit lines matching re in files, relative to
// root, in their order. It reads a file only when it is regular, at most
// grepMaxSize bytes and not binary; show, asked once per file with hits,
// can leave that file out.
func Grep(ctx context.Context, root string, files []string, re *regexp.Regexp, limit int, show func(rel string) bool) ([]Hit, error) {
	whole := wholeFile(re)
	found, done := make([][]Hit, len(files)), make([]bool, len(files))
	// the files before prefix are all searched; the hits among them decide
	// the answer, so it does not depend on which worker finished first.
	// NOTE: workers run ahead of a slow file unbounded, holding up to limit
	// hits per file they pass; a window on next-prefix would cap that.
	var mu sync.Mutex
	prefix, n := 0, 0
	var next atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			var buf []byte
			for !stop.Load() && ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				hits := grepFile(root, files[i], re, whole, limit, &buf)
				mu.Lock()
				found[i], done[i] = hits, true
				for ; prefix < len(files) && done[prefix]; prefix++ {
					if len(found[prefix]) > 0 && show != nil && !show(files[prefix]) {
						found[prefix] = nil
					}
					n += len(found[prefix])
				}
				stop.Store(n >= limit)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, f := range found[:prefix] {
		hits = append(hits, f...)
	}
	return hits[:min(len(hits), limit)], nil
}

// grepFile returns up to limit matching lines of one file, read into the
// worker's buf. It reads only what a fresh Lstat and the opened file agree
// is one regular file, so a symbolic link swapped in since the listing is
// never followed.
// NOTE: a parent directory swapped for a link still redirects the read;
// only the file itself is checked.
func grepFile(root, rel string, re, whole *regexp.Regexp, limit int, buf *[]byte) []Hit {
	path := filepath.Join(root, rel)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > grepMaxSize {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !os.SameFile(fi, st) {
		return nil
	}
	if int64(cap(*buf)) < fi.Size() {
		*buf = make([]byte, fi.Size())
	}
	data := (*buf)[:fi.Size()]
	n, _ := io.ReadFull(f, data)
	data = data[:n]
	if looksBinary(data) || whole != nil && !whole.Match(data) {
		return nil
	}
	var hits []Hit
	n = 0
	for line := range bytes.Lines(data) {
		n++
		if line = bytes.TrimSuffix(line, []byte("\n")); re.Match(line) {
			if hits = append(hits, Hit{Rel: rel, Line: n, Text: string(line)}); len(hits) == limit {
				break
			}
		}
	}
	return hits
}

// wholeFile is re made to match at line breaks, so one pass over a whole
// file rules out every line of it; nil when re anchors to the text (\A, \z,
// ^ under (?-m)), which a line-wise search matches at every line.
func wholeFile(re *regexp.Regexp) *regexp.Regexp {
	src := "(?m)" + re.String()
	parsed, err := syntax.Parse(src, syntax.Perl)
	if err != nil || anchorsText(parsed) {
		return nil
	}
	whole, err := regexp.Compile(src)
	if err != nil {
		return nil
	}
	return whole
}

func anchorsText(re *syntax.Regexp) bool {
	return re.Op == syntax.OpBeginText || re.Op == syntax.OpEndText || slices.ContainsFunc(re.Sub, anchorsText)
}

// walk visits the regular files under root, by path relative to it, when
// git cannot list them.
// NOTE: it reads no .gitignore and skips hidden and generated directories
// by name instead, so a tracked .github or build/ is missed and an ignored
// output directory listed; the root .gitignore read through internal/glob
// (negations, anchoring, directory-only lines) would get closer.
func walk(ctx context.Context, root string, fn func(rel string) error) error {
	// WalkDir does not follow a symlinked root, and would visit nothing
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	// the handler below drops errors so an unreadable subdirectory is skipped,
	// and a missing root would read as no matches
	fi, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("no such file or directory: %s", root)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is a file: pass its directory as path", root)
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
		return fn(rel)
	})
}

func truncLine(s string) string {
	if len(s) > 200 {
		return strings.ToValidUTF8(s[:200], "") + "..."
	}
	return s
}

func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build",
		"__pycache__", "coverage", "venv":
		return true
	}
	return false
}

// workdir is the shell execution root.
func workdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
