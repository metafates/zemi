package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
)

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
	env, err := perm.ChildEnv(nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	c.Env = append(env, proc.NonInteractiveEnv...)
	// one buffer keeps stdout and stderr in the order they were written
	buf := &proc.Buffer{Max: 1 << 20}
	c.Stdout, c.Stderr = buf, buf
	if err = proc.Start(c); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(c) }()
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
	// the count goes with the text: job_read resumes exactly after it
	raw, written := buf.Snapshot()
	out := strings.TrimRight(ansi.Strip(raw), "\n")
	var b strings.Builder
	// compare the text, not lengths: the cut marker can outgrow a small cut
	if cut := truncMiddle(out); cut != out {
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
			id := p.Jobs.Adopt(cmd, c, buf, written, wait)
			fmt.Fprintf(&b, "still running after %s, so it moved to the background as %s: read the rest with job_read, stop it with job_kill.", timeout, id)
			return fantasy.NewTextResponse(b.String()), nil
		}
		proc.Kill(c)
		<-wait
		b.WriteString("timed out after " + timeout.String() + " and was stopped. Servers, watch modes and other commands that do not exit belong in job_start.")
		return fantasy.ToolResponse{Type: "text", Content: b.String(), IsError: true}, nil
	}
	if err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if ctx.Err() != nil {
			b.WriteString("stopped: the user interrupted the command")
		} else {
			b.WriteString("exit: " + err.Error())
		}
		return fantasy.ToolResponse{Type: "text", Content: b.String(), IsError: true}, nil
	}
	if b.Len() == 0 {
		return fantasy.NewTextResponse("(no output)"), nil
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// truncMiddle keeps the start and the end of long command output: commands
// print their errors and summaries last.
func truncMiddle(s string) string {
	const head, tail = 3000, 7000
	if len(s) <= head+tail {
		return s
	}
	return strings.ToValidUTF8(s[:head], "") + fmt.Sprintf("\n... (%d characters cut) ...\n", len(s)-head-tail) + strings.ToValidUTF8(s[len(s)-tail:], "")
}

// spill saves output too long for a result to the scratchpad and names the
// file, so the cut part stays one read away; "" without a scratchpad.
func (p *Pipeline) spill(name, s string) string {
	if p == nil || p.ScratchDir == nil {
		return ""
	}
	dir := p.ScratchDir()
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

const (
	readMaxLines   = 2000
	readMaxBytes   = 50_000
	readMaxLineLen = 2000
	readMaxImage   = 5 << 20
)

// runRead reads a file with optional 1-based line offset and limit. A page
// stops at readMaxLines or readMaxBytes and names the offset to continue
// from, so a large file costs pages the model asks for, not one huge result.
func runRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.IsDir():
		return fantasy.NewTextErrorResponse(path + " is a directory; list it with glob"), nil
	case err == nil && !fi.Mode().IsRegular():
		// a FIFO blocks and a device can be endless
		return fantasy.NewTextErrorResponse(path + " is not a regular file"), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	switch mt := http.DetectContentType(data); {
	case mt == "image/png" || mt == "image/jpeg" || mt == "image/gif" || mt == "image/webp":
		if len(data) > readMaxImage {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("%s is a %d byte image, over the %d byte limit", path, len(data), readMaxImage)), nil
		}
		if p.Images != nil && !p.Images() {
			return fantasy.NewTextErrorResponse(path + " is an image and the current model does not take images; go on without it, or ask the user to describe it or switch model"), nil
		}
		return fantasy.NewImageResponse(data, mt), nil
	case bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0:
		return fantasy.NewTextErrorResponse(path + " is a binary file"), nil
	}
	// a page of a file still counts as seen: the edit guard is about what
	// changed under the model, and an exact old_string is its own proof
	p.markRead(path, fi)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start := 0
	if v, ok := input["offset"].(float64); ok && v > 1 {
		start = int(v) - 1
	}
	if start >= len(lines) && start > 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("offset %d is past the end of %s (%d lines)", start+1, path, len(lines))), nil
	}
	end := len(lines)
	if v, ok := input["limit"].(float64); ok && v > 0 {
		if start+int(v) < end {
			end = start + int(v)
		}
	}
	var b strings.Builder
	i := start
	for ; i < end && i-start < readMaxLines; i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if len(line) > readMaxLineLen {
			line = strings.ToValidUTF8(line[:readMaxLineLen], "") + "... (line cut)"
		}
		row := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if i > start && b.Len()+len(row) > readMaxBytes {
			break
		}
		b.WriteString(row)
	}
	if i < end {
		fmt.Fprintf(&b, "... (%d more lines; continue with offset %d)\n", end-i, i+1)
	}
	if p != nil && p.Instructions != nil {
		b.WriteString(p.Instructions(path))
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// markRead records the modification time the model saw path at; fi is
// taken before the read, so a change during it reads as stale.
func (p *Pipeline) markRead(path string, fi os.FileInfo) {
	if p != nil && p.Seen != nil && fi != nil {
		p.Seen.Store(absPath(path), fi.ModTime())
	}
}

// stale refuses changing a file the model has not read, or one that changed
// since (the user's editor, a formatter, another agent): writing over it
// would silently drop those changes. "" when the view is current.
func (p *Pipeline) stale(path string, fi os.FileInfo) string {
	if p == nil || p.Seen == nil {
		return ""
	}
	t, ok := p.Seen.Load(absPath(path))
	switch {
	case !ok:
		return "read " + path + " with the read tool first: this session has not read it, and cat or grep in bash does not count. Then repeat this edit."
	case !t.(time.Time).Equal(fi.ModTime()):
		return path + " changed since you read it (the user's editor, a formatter or another agent). Read it again and redo the edit against the current content."
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

// absPath is the key both the read log and the file locks use: symlinks
// resolve, so an alias and its target are the same file, and case folds
// where the filesystem does, so main.go and Main.go take the same lock. A
// path that does not exist yet (a write) keeps its absolute form.
// NOTE: a case-sensitive volume on darwin or windows makes two real files
// share a key; that costs a re-read, never a wrong write.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}

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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	fi, _ := os.Stat(path)
	p.markRead(path, fi)
	return fantasy.NewTextResponse(fmt.Sprintf("wrote %s (%d bytes)", path, len(content)) + p.afterWrite(path)), nil
}

func (p *Pipeline) afterWrite(path string) string {
	if p.AfterWrite == nil {
		return ""
	}
	return p.AfterWrite(path)
}

func (p *Pipeline) checkpoint(path string) {
	if p.Checkpoint != nil {
		p.Checkpoint(path)
	}
}

func runEdit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	return editFile(p, "edited", str(input["path"]), []any{input})
}

func runMultiedit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	edits, ok := input["edits"].([]any)
	if !ok || len(edits) == 0 {
		return fantasy.NewTextErrorResponse("edits must be a non-empty array"), nil
	}
	return editFile(p, "multiedit", str(input["path"]), edits)
}

// editFile applies edits to a file under its lock, all or nothing. Matching
// runs on LF text and a CRLF file is written back with CRLF.
func editFile(p *Pipeline, verb, path string, edits []any) (fantasy.ToolResponse, error) {
	defer lockFile(path)()
	fi, err := os.Stat(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if msg := p.stale(path, fi); msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	out, n, tolerant, msg := applyEdits(lf(string(data)), edits, path)
	if msg != "" {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// a mixed file takes its majority ending, so a stray line of the other
	// kind never rewrites every line
	if n := bytes.Count(data, []byte("\r\n")); 2*n > bytes.Count(data, []byte("\n")) {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	p.checkpoint(path)
	if err := os.WriteFile(path, []byte(out), fi.Mode().Perm()); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	fi, _ = os.Stat(path)
	p.markRead(path, fi)
	res := fmt.Sprintf("%s %s (%d replacement%s)", verb, path, n, plural(n))
	if tolerant {
		res += "; matched ignoring whitespace, so check the indentation"
	}
	return fantasy.NewTextResponse(res + p.afterWrite(path)), nil
}

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// applyEdits applies edits in order to LF text. msg explains the first edit
// that does not apply, and then nothing changes.
func applyEdits(src string, edits []any, where string) (out string, n int, tolerant bool, msg string) {
	for i, e := range edits {
		em, _ := e.(map[string]any)
		old, new := lf(str(em["old_string"])), lf(str(em["new_string"]))
		all, _ := em["replace_all"].(bool)
		res, c := applyEdit(src, old, new, all)
		if c == 0 && old != "" {
			res, c = applyTolerant(src, old, new, all)
			tolerant = tolerant || c > 0
		}
		if m := editError(src, old, c, all, where); m != "" {
			if len(edits) > 1 {
				m = fmt.Sprintf("edit %d: %s; nothing was written", i, m)
			}
			return src, 0, false, m
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
		for i, l := range strings.Split(src, "\n") {
			if strings.TrimSpace(l) == first {
				return fmt.Sprintf("old_string not found in %s; its first line matches line %d, so a later line differs: read from there and copy the text exactly", where, i+1)
			}
		}
	}
	return "old_string not found in " + where + "; read the file again and copy the text exactly"
}

// applyTolerant matches whole lines ignoring whitespace runs, and moves
// new_string to the file's indentation: the indent old_string used on each
// matched line maps to the indent the file has there.
func applyTolerant(src, old, new string, all bool) (string, int) {
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
	at, count := 0, 0
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
		return src, count
	}
	return strings.Join(append(b, srcLines[at:]...), "\n"), count
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const (
	globMaxFiles   = 200
	globMaxScan    = 20_000
	grepMaxMatches = 100
)

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
	if len(rels) > globMaxFiles {
		rels = append(rels[:globMaxFiles], fmt.Sprintf("... (%d more; narrow the pattern or path)", len(rels)-globMaxFiles))
	}
	return fantasy.NewTextResponse(strings.Join(rels, "\n")), nil
}

// runGrep searches file contents under root, optionally only in files
// matching the include glob. Secret-shaped files are never searched.
func runGrep(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern, root, include := str(input["pattern"]), searchRoot(input), str(input["include"])
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fantasy.NewTextErrorResponse("bad pattern: " + err.Error()), nil
	}
	var hits []string
	add := func(rel string, line int, text string) bool {
		if !perm.PathIsSecret(rel) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(filepath.Join(root, rel)), line, truncLine(text)))
		}
		return len(hits) < grepMaxMatches
	}
	args := []string{"--line-number", "--no-heading", "--null", "--max-columns", "1000", "--max-columns-preview", "--regexp", pattern}
	if include != "" {
		args = append(args, "--glob", include)
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
		err = walk(ctx, root, func(path, rel string) error {
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
		})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	if len(hits) == 0 {
		return fantasy.NewTextResponse("(no matches)"), nil
	}
	out := strings.Join(hits, "\n") + "\n"
	if len(hits) >= grepMaxMatches {
		out += fmt.Sprintf("... (stopped at %d matches; narrow the pattern, path or include)", grepMaxMatches)
	}
	return fantasy.NewTextResponse(out), nil
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
	c := proc.CommandContext(cctx, rgBin, append([]string{"--hidden", "--no-require-git", "--glob", "!.git"}, args...)...)
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

// workdir is the bash execution root; tests override it.
var workdir = osWorkdir

func osWorkdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// SetWorkdir pins the session working directory for bash calls; an empty
// string restores the process working directory.
func SetWorkdir(dir string) {
	workdir = osWorkdir
	if dir != "" {
		workdir = func() string { return dir }
	}
}
