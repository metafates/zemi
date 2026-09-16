// Package lsp is a small Language Server Protocol client for diagnostics:
// it starts a server over stdio, keeps the files zemi touches open with full
// text sync, and collects the diagnostics the server publishes.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/proc"
)

// Config starts one server.
type Config struct {
	Command string
	Args    []string
	Env     []string // the child's whole environment
	Root    string   // workspace root
	Stderr  io.Writer
	// InitOptions is sent as initializationOptions; Settings answers
	// workspace/configuration (by dotted section) and is pushed once after
	// the handshake. Both nil for servers that need neither.
	InitOptions any
	Settings    any
}

// Diagnostic is one published problem; Line and Col are 1-based.
type Diagnostic struct {
	Line, Col int
	Severity  int // 1 error, 2 warning, 3 information, 4 hint
	Message   string
	Source    string
}

// SeverityName is the word for a severity ("error", "warning", ...).
func SeverityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	}
	return "hint"
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client is one running server. Safe for concurrent use.
type Client struct {
	cmd   *exec.Cmd
	wmu   sync.Mutex
	smu   sync.Mutex // orders document syncs so versions arrive in order
	stdin io.WriteCloser

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan message
	diags   map[string][]Diagnostic // by absolute path
	opened  map[string]int          // document version by path
	docs    map[string]docState     // what was last sent, by path
	seq     int                     // publishes so far
	pubSeq  map[string]int          // seq of the last publish by path
	synced  map[string]int          // seq when the path was last synced
	wake    chan struct{}           // closed and replaced on every publish

	done     chan struct{}
	endOnce  sync.Once
	endErr   error
	settings any
	root     string
	watchers map[string][]watcher // by registration id
}

// watcher is one glob a server asked to be told about, with the change
// kinds it wants (bit 1 create, 2 change, 4 delete).
type watcher struct {
	match func(rel string) bool
	base  string // directory the glob is relative to
	kind  int
}

// FileChange is one disk change for NotifyChanged.
type FileChange struct {
	Path string
	Type int // 1 created, 2 changed, 3 deleted
}

// Start launches the server and runs the initialize handshake; ctx bounds
// the handshake only, not the server's life.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	cmd := proc.Command(cfg.Command, cfg.Args...)
	cmd.Env, cmd.Dir, cmd.Stderr = cfg.Env, cfg.Root, cfg.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := proc.Start(cmd); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", cfg.Command, err)
	}
	c := &Client{
		cmd: cmd, stdin: stdin,
		pending: map[int64]chan message{}, diags: map[string][]Diagnostic{},
		opened: map[string]int{}, docs: map[string]docState{}, pubSeq: map[string]int{}, synced: map[string]int{},
		wake: make(chan struct{}), done: make(chan struct{}), settings: cfg.Settings,
		root: cfg.Root, watchers: map[string][]watcher{},
	}
	go c.read(bufio.NewReader(stdout))

	root := DocumentURI(cfg.Root)
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   root,
		"workspaceFolders": []any{
			map[string]any{"uri": root, "name": filepath.Base(cfg.Root)},
		},
		"capabilities": map[string]any{
			// one encoding only, so every server answers in the units
			// Offset and Pos count
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{"versionSupport": true},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
			},
			"workspace": map[string]any{
				"configuration": true, "workspaceFolders": true,
				// servers keep their own copy of files nobody opened and
				// only re-read one when told it changed
				"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true, "relativePatternSupport": true},
				// versioned edits, so a rename planned against an older
				// text can be refused instead of applied
				"workspaceEdit": map[string]any{"documentChanges": true},
			},
		},
	}
	if cfg.InitOptions != nil {
		params["initializationOptions"] = cfg.InitOptions
	}
	if err := c.Call(ctx, "initialize", params, nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("lsp: initialize %s: %w", cfg.Command, err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	if cfg.Settings != nil {
		_ = c.notify("workspace/didChangeConfiguration", map[string]any{"settings": cfg.Settings})
	}
	return c, nil
}

// ErrNoFresh is Touch's error when the server published nothing for the
// text it was sent: what it holds describes an older version, so nothing
// is returned rather than something stale.
var ErrNoFresh = errors.New("lsp: no diagnostics for the current text")

// Touch syncs path's current content to the server and waits for the
// diagnostics that follow (servers often publish twice, a quick pass then
// the full check, so a short quiet period ends the wait).
func (c *Client) Touch(ctx context.Context, path, languageID string) ([]Diagnostic, error) {
	c.refresh()
	key := normPath(path)
	before, err := c.syncFile(path, languageID)
	if err != nil {
		return nil, err
	}

	quiet := (<-chan time.Time)(nil)
	// A server that publishes nothing for this file (an unsupported type,
	// no project) would otherwise hold the caller to its whole timeout.
	floor := time.After(800 * time.Millisecond)
	for {
		c.mu.Lock()
		fresh, wake := c.pubSeq[key] > before, c.wake
		c.mu.Unlock()
		if fresh {
			floor = nil
			if quiet == nil {
				quiet = time.After(300 * time.Millisecond)
			}
		}
		select {
		case <-wake:
			if fresh {
				quiet = time.After(300 * time.Millisecond)
			}
		case <-quiet:
			return c.Diagnostics(path), nil
		case <-floor:
			return nil, ErrNoFresh
		case <-ctx.Done():
			return nil, ErrNoFresh
		case <-c.done:
			return nil, c.endErr
		}
	}
}

// Open syncs path's current content to the server without waiting for
// diagnostics, so a query that follows sees the text on disk.
func (c *Client) Open(path, languageID string) error {
	c.refresh()
	_, err := c.syncFile(path, languageID)
	return err
}

// docState is what the server holds for an open document: the server uses
// that text, not the disk, so a file changed behind it (a rename's edits to
// other files, a formatter, git) has to be sent again before it is queried.
type docState struct {
	uri, lang string
	mtime     time.Time
}

// syncFile sends path's text from disk and records its modification time.
func (c *Client) syncFile(path, languageID string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	key := normPath(path)
	c.mu.Lock()
	c.docs[key] = docState{uri: DocumentURI(path), lang: languageID, mtime: fi.ModTime()}
	c.mu.Unlock()
	return c.sync(key, DocumentURI(path), languageID, string(data))
}

// refresh re-sends every open document whose file changed since it was
// last sent and closes the ones whose file is gone, so the server never
// answers from text that is no longer on disk.
func (c *Client) refresh() {
	c.mu.Lock()
	stale, gone := map[string]docState{}, map[string]docState{}
	for key, d := range c.docs {
		switch fi, err := os.Stat(key); {
		case errors.Is(err, os.ErrNotExist):
			gone[key] = d
		case err == nil && !fi.ModTime().Equal(d.mtime):
			// the mtime is taken before the read, so a write between the
			// two only makes the next refresh send the file again
			d.mtime = fi.ModTime()
			stale[key] = d
		}
	}
	c.mu.Unlock()
	for key, d := range stale {
		data, err := os.ReadFile(key)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.docs[key] = d
		c.mu.Unlock()
		_, _ = c.sync(key, d.uri, d.lang, string(data))
	}
	for key, d := range gone {
		c.closeDoc(key, d.uri)
	}
}

// closeDoc tells the server the document is closed and forgets it: its
// diagnostics, version and text no longer describe anything.
func (c *Client) closeDoc(key, uri string) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.mu.Lock()
	delete(c.docs, key)
	delete(c.opened, key)
	delete(c.diags, key)
	c.mu.Unlock()
	_ = c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

// Version is the document version last sent for path, 0 when it is not
// open. A server's edit that names another version was planned against
// text the server no longer has.
func (c *Client) Version(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened[normPath(path)]
}

// NotifyChanged tells the server about files that changed on disk, those
// it registered a watcher for. An open document is not among them: its
// text is what the client sent, and refresh keeps that current.
func (c *Client) NotifyChanged(changes []FileChange) {
	c.mu.Lock()
	var events []any
	for _, ch := range changes {
		key := normPath(ch.Path)
		if _, open := c.docs[key]; open && ch.Type != 3 {
			continue
		}
		if c.watched(ch.Path, ch.Type) {
			events = append(events, map[string]any{"uri": DocumentURI(ch.Path), "type": ch.Type})
		}
	}
	c.mu.Unlock()
	if len(events) > 0 {
		_ = c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": events})
	}
}

// watched reports whether any registered watcher covers path for typ;
// c.mu held.
func (c *Client) watched(path string, typ int) bool {
	path = normPath(path)
	for _, ws := range c.watchers {
		for _, w := range ws {
			rel, err := filepath.Rel(w.base, path)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			if w.kind&(1<<(typ-1)) != 0 && w.match(rel) {
				return true
			}
		}
	}
	return false
}

// register keeps a server's workspace/didChangeWatchedFiles registration;
// unregister drops it by id. Other capabilities are accepted and ignored.
func (c *Client) register(raw json.RawMessage, on bool) {
	var p struct {
		Registrations []struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Opts   struct {
				Watchers []struct {
					Pattern json.RawMessage `json:"globPattern"`
					Kind    *int            `json:"kind"`
				} `json:"watchers"`
			} `json:"registerOptions"`
		} `json:"registrations"`
		Unregistrations []struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		} `json:"unregisterations"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !on {
		for _, u := range p.Unregistrations {
			delete(c.watchers, u.ID)
		}
		return
	}
	for _, r := range p.Registrations {
		if r.Method != "workspace/didChangeWatchedFiles" {
			continue
		}
		var ws []watcher
		for _, w := range r.Opts.Watchers {
			pattern, base := "", c.root
			var rp struct {
				BaseURI json.RawMessage `json:"baseUri"`
				Pattern string          `json:"pattern"`
			}
			if json.Unmarshal(w.Pattern, &pattern) != nil && json.Unmarshal(w.Pattern, &rp) == nil {
				pattern = rp.Pattern
				var s string
				var f struct {
					URI string `json:"uri"`
				}
				if json.Unmarshal(rp.BaseURI, &s) == nil {
					base = Location{URI: s}.Path()
				} else if json.Unmarshal(rp.BaseURI, &f) == nil {
					base = Location{URI: f.URI}.Path()
				}
			}
			match, err := glob.Match(pattern)
			if err != nil {
				continue
			}
			kind := 7
			if w.Kind != nil {
				kind = *w.Kind
			}
			ws = append(ws, watcher{match: match, base: normPath(base), kind: kind})
		}
		c.watchers[r.ID] = ws
	}
}

// Position is an LSP position: 0-based line and UTF-16 character.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP range, end exclusive.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is one place in a file.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Path is the file a URI names.
func (l Location) Path() string {
	u, err := url.Parse(l.URI)
	if err != nil {
		return l.URI
	}
	return filepath.FromSlash(u.Path)
}

// TextEdit replaces a range with NewText.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// Offset converts a position to a byte offset in text, clamped to its end.
func Offset(text string, pos Position) int {
	off := 0
	for range pos.Line {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return len(text)
		}
		off += i + 1
	}
	units := 0
	for i, r := range text[off:] {
		if units >= pos.Character || r == '\n' {
			return off + i
		}
		units++
		if r > 0xFFFF {
			units++
		}
	}
	return len(text)
}

// Pos converts a byte offset in text to a position.
func Pos(text string, off int) Position {
	off = min(off, len(text))
	line := strings.Count(text[:off], "\n")
	start := strings.LastIndexByte(text[:off], '\n') + 1
	units := 0
	for _, r := range text[start:off] {
		units++
		if r > 0xFFFF {
			units++
		}
	}
	return Position{Line: line, Character: units}
}

// Others returns the errors published for files other than path since path
// was last synced: a change there that broke code elsewhere.
func (c *Client) Others(path string) map[string][]Diagnostic {
	key := normPath(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]Diagnostic{}
	for p, ds := range c.diags {
		if p == key || c.pubSeq[p] <= c.synced[key] {
			continue
		}
		for _, d := range ds {
			if d.Severity == 1 {
				out[p] = append(out[p], d)
			}
		}
	}
	return out
}

// sync sends the document's current text and reports the publish seq it
// started from. The version bump and the notify share one lock so a later
// version can never reach the server first; c.mu, which the reader also
// takes, is never held across the write.
func (c *Client) sync(key, uri, languageID, text string) (int, error) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.mu.Lock()
	before := c.seq
	version := c.opened[key] + 1
	c.opened[key] = version
	c.synced[key] = c.seq
	c.mu.Unlock()
	var err error
	if version == 1 {
		err = c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": uri, "languageId": languageID, "version": version, "text": text,
		}})
	} else {
		err = c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []any{map[string]any{"text": text}},
		})
	}
	if err != nil {
		return before, err
	}
	_ = c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})
	return before, nil
}

// Diagnostics returns the last published diagnostics for path.
func (c *Client) Diagnostics(path string) []Diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Diagnostic(nil), c.diags[normPath(path)]...)
}

// Done is closed when the server exits.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close asks the server to shut down and kills it if it lingers.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Call(ctx, "shutdown", nil, nil)
	_ = c.notify("exit", nil)
	c.wmu.Lock()
	err := c.stdin.Close()
	c.wmu.Unlock()
	time.AfterFunc(2*time.Second, func() { proc.Kill(c.cmd) })
	return err
}

func (c *Client) end(err error) {
	c.endOnce.Do(func() {
		c.endErr = err
		close(c.done)
	})
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteMessage(c.stdin, b)
}

func (c *Client) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.send(msg)
}

// Call sends a request and decodes its result into out (nil to ignore).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.send(msg); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("lsp error %d: %s", m.Error.Code, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.endErr
	}
}

func (c *Client) read(r *bufio.Reader) {
	defer func() {
		if p := recover(); p != nil {
			c.end(fmt.Errorf("lsp: reader panic: %v", p))
		}
	}()
	for {
		body, err := ReadMessage(r)
		if err != nil {
			werr := proc.Wait(c.cmd)
			if werr == nil {
				werr = err
			}
			c.end(fmt.Errorf("lsp: server exited: %w", werr))
			return
		}
		var m message
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			// Server requests: configuration gets each item's section of the
			// settings (null without any), the rest (capability registration,
			// progress) a null result.
			var result any
			switch m.Method {
			case "client/registerCapability":
				c.register(m.Params, true)
			case "client/unregisterCapability":
				c.register(m.Params, false)
			case "workspace/configuration":
				var p struct {
					Items []struct {
						Section string `json:"section"`
					} `json:"items"`
				}
				_ = json.Unmarshal(m.Params, &p)
				answers := make([]any, len(p.Items))
				for i, it := range p.Items {
					answers[i] = section(c.settings, it.Section)
				}
				result = answers
			}
			_ = c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		case m.Method == "textDocument/publishDiagnostics":
			c.publish(m.Params)
		case m.Method == "":
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				// a duplicate id must not block the reader on a full channel
				select {
				case ch <- m:
				default:
				}
			}
		}
	}
}

func (c *Client) publish(raw json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
		Version     *int   `json:"version"`
		Diagnostics []struct {
			Range    Range  `json:"range"`
			Severity int    `json:"severity"`
			Message  string `json:"message"`
			Source   string `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	path := normPath(Location{URI: p.URI}.Path())
	var out []Diagnostic
	for _, d := range p.Diagnostics {
		sev := d.Severity
		if sev == 0 {
			sev = 1
		}
		out = append(out, Diagnostic{Line: d.Range.Start.Line + 1, Col: d.Range.Start.Character + 1, Severity: sev, Message: d.Message, Source: d.Source})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// a publish for text older than what was sent is stale, not fresh
	if p.Version != nil && *p.Version < c.opened[path] {
		return
	}
	c.diags[path] = out
	c.seq++
	c.pubSeq[path] = c.seq
	close(c.wake)
	c.wake = make(chan struct{})
}

// section walks settings by a dotted section name ("gopls.analyses");
// "" is the whole table and a missing key is nil.
func section(settings any, name string) any {
	cur := settings
	for k := range strings.SplitSeq(name, ".") {
		if k == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// normPath resolves symlinks so a server that answers with a canonicalized
// URI (/tmp -> /private/tmp) keys the same file as the path asked about; a
// path that no longer exists resolves through its directory.
func normPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// DocumentURI is the file URI for a path, as the client sends it.
func DocumentURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// ReadMessage reads one Content-Length framed message.
func ReadMessage(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if n < 0 || n > 64<<20 {
		return nil, errors.New("lsp: bad Content-Length")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}

// WriteMessage writes one Content-Length framed message.
func WriteMessage(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}
