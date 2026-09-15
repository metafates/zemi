package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
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
		timeout = time.Duration(v) * time.Millisecond
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = workdir()
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	err := c.Run()
	var b strings.Builder
	if s := strings.TrimRight(out.String(), "\n"); s != "" {
		b.WriteString(truncOut(s))
	}
	if s := strings.TrimRight(errb.String(), "\n"); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr] " + truncOut(s))
	}
	if err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if ctxErr := cctx.Err(); ctxErr == context.DeadlineExceeded {
			b.WriteString("timed out after " + timeout.String())
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

// runRead reads a file with optional 1-based line offset and limit.
func runRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if v, ok := input["offset"].(float64); ok && v > 1 {
		start = int(v) - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if v, ok := input["limit"].(float64); ok && v > 0 {
		if start+int(v) < end {
			end = start + int(v)
		}
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, strings.TrimSuffix(lines[i], "\r"))
	}
	return fantasy.NewTextResponse(truncOut(b.String())), nil
}

// runWrite creates or overwrites a file (parents created).
func runWrite(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	content := str(input["content"])
	p.checkpoint(path)
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
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

// applyEdit replaces old with new in s; count 0 means not found.
func applyEdit(s, old, new string, all bool) (string, int) {
	n := strings.Count(s, old)
	if n == 0 {
		return s, 0
	}
	if all {
		return strings.ReplaceAll(s, old, new), n
	}
	return strings.Replace(s, old, new, 1), 1
}

// runEdit replaces one exact substring; whitespace-tolerant fallback:
// when the exact match fails, lines are compared trimmed and the matched
// block is replaced preserving the file's line structure.
func runEdit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	old := str(input["old_string"])
	new := str(input["new_string"])
	all := false
	if v, ok := input["replace_all"].(bool); ok {
		all = v
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	src := string(data)
	out, n := applyEdit(src, old, new, all)
	if n == 0 {
		out, n = applyTolerant(src, old, new, all)
	}
	if n == 0 {
		return fantasy.NewTextErrorResponse("old_string not found in " + path), nil
	}
	p.checkpoint(path)
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("edited %s (%d replacement%s)", path, n, plural(n)) + p.afterWrite(path)), nil
}

// applyTolerant matches a multi-line block ignoring whitespace runs:
// leading/trailing space and inner space runs compare equal.
func applyTolerant(src, old, new string, all bool) (string, int) {
	if !strings.Contains(old, "\n") {
		return src, 0
	}
	simp := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	srcLines := strings.Split(src, "\n")
	oldLines := strings.Split(strings.Trim(old, "\n"), "\n")
	n := len(srcLines)
	m := len(oldLines)
	count := 0
	type span struct{ lo, hi int }
	var spans []span
	for i := 0; i+m <= n; i++ {
		ok := true
		for j := 0; j < m; j++ {
			if simp(srcLines[i+j]) != simp(oldLines[j]) {
				ok = false
				break
			}
		}
		if ok {
			spans = append(spans, span{i, i + m})
			count++
			i += m - 1
		}
	}
	if count == 0 {
		return src, 0
	}
	if !all {
		spans = spans[:1]
		count = 1
	}
	newLines := strings.Split(strings.Trim(new, "\n"), "\n")
	var b []string
	at := 0
	for _, s := range spans {
		b = append(b, srcLines[at:s.lo]...)
		b = append(b, newLines...)
		at = s.hi
	}
	b = append(b, srcLines[at:]...)
	return strings.Join(b, "\n"), count
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runMultiedit applies edits in order to one file; a failed edit aborts
// with its index (the file keeps earlier edits, matching the design's
// sequential semantics).
func runMultiedit(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	path := str(input["path"])
	edits, ok := input["edits"].([]any)
	if !ok || len(edits) == 0 {
		return fantasy.NewTextErrorResponse("edits must be a non-empty array"), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	src := string(data)
	applied := 0
	for i, e := range edits {
		em, ok := e.(map[string]any)
		if !ok {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("edit %d: not an object", i)), nil
		}
		old := str(em["old_string"])
		new := str(em["new_string"])
		all, _ := em["replace_all"].(bool)
		var out string
		var n int
		if out, n = applyEdit(src, old, new, all); n == 0 {
			if out, n = applyTolerant(src, old, new, all); n == 0 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("edit %d: old_string not found", i)), nil
			}
		}
		src = out
		applied += n
	}
	p.checkpoint(path)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("multiedit %s (%d replacement%s)", path, applied, plural(applied)) + p.afterWrite(path)), nil
}

// runGlob matches the pattern against paths under root (default session
// dir), skipping hidden dirs and vendored trees.
func runGlob(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern := str(input["pattern"])
	root := argOr(input["path"], ".")
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		ok, _ := filepath.Match(pattern, name)
		if !ok {
			ok, _ = filepath.Match(pattern, filepath.ToSlash(path))
		}
		if ok {
			out = append(out, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	sort.Strings(out)
	if len(out) > 200 {
		out = append(out[:200], fmt.Sprintf("... (%d more)", len(out)-200))
	}
	if len(out) == 0 {
		return fantasy.NewTextResponse("(no matches)"), nil
	}
	return fantasy.NewTextResponse(strings.Join(out, "\n")), nil
}

// grepRe compiles the pattern; invalid regexes are tool errors.
func grepRe(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(pattern)
}

// runGrep searches file contents (Go-native; case sensitive) under root,
// optionally filtered by an include glob on the file name.
func runGrep(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	pattern := str(input["pattern"])
	root := argOr(input["path"], ".")
	include := str(input["include"])
	re, err := grepRe(pattern)
	if err != nil {
		return fantasy.NewTextErrorResponse("bad pattern: " + err.Error()), nil
	}
	var inc *regexp.Regexp
	if include != "" {
		inc, err = regexp.Compile(globToRegexp(include))
		if err != nil {
			inc = nil
		}
	}
	var b strings.Builder
	count := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if inc != nil && !inc.MatchString(name) {
			return nil
		}
		if d.Type().IsRegular() && isTextFile(path) {
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) {
					fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(path), i+1, truncLine(line))
					count++
					if count >= 200 {
						return filepath.SkipAll
					}
					break
				}
			}
		}
		return nil
	})
	_ = err
	if count == 0 {
		return fantasy.NewTextResponse("(no matches)"), nil
	}
	if count >= 200 {
		b.WriteString("... (stopped at 200 files)")
	}
	return fantasy.NewTextResponse(truncOut(b.String())), nil
}

func truncLine(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// globToRegexp converts a filename glob (`*`, `?`) to a regexp.
func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString(`^`)
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`$`)
	return b.String()
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
		"__pycache__", "coverage", ".venv", "venv":
		return true
	}
	return false
}

// workdir is the bash execution root; tests override it.
var workdir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// SetWorkdir pins the session working directory for bash calls; an empty
// string restores the process working directory.
func SetWorkdir(dir string) {
	if dir == "" {
		workdir = func() string {
			wd, err := os.Getwd()
			if err != nil {
				return "."
			}
			return wd
		}
		return
	}
	workdir = func() string { return dir }
}
