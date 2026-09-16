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

	"github.com/metafates/zemi/internal/proc"
)

// Config starts one server.
type Config struct {
	Command string
	Args    []string
	Env     []string // the child's whole environment
	Root    string   // workspace root
	Stderr  io.Writer
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
	pubs    map[string]int          // publish count by path
	opened  map[string]int          // document version by path
	seq     int                     // publishes so far
	pubSeq  map[string]int          // seq of the last publish by path
	synced  map[string]int          // seq when the path was last synced
	wake    chan struct{}           // closed and replaced on every publish

	done    chan struct{}
	endOnce sync.Once
	endErr  error
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
		pubs: map[string]int{}, opened: map[string]int{}, pubSeq: map[string]int{}, synced: map[string]int{},
		wake: make(chan struct{}), done: make(chan struct{}),
	}
	go c.read(bufio.NewReader(stdout))

	root := fileURI(cfg.Root)
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   root,
		"workspaceFolders": []any{
			map[string]any{"uri": root, "name": filepath.Base(cfg.Root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{},
			},
			"workspace": map[string]any{"configuration": true, "workspaceFolders": true},
		},
	}
	if err := c.call(ctx, "initialize", params, nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("lsp: initialize %s: %w", cfg.Command, err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key := normPath(path)
	before, err := c.sync(key, fileURI(path), languageID, string(data))
	if err != nil {
		return nil, err
	}

	quiet := (<-chan time.Time)(nil)
	// A server that publishes nothing for this file (an unsupported type,
	// no project) would otherwise hold the caller to its whole timeout.
	floor := time.After(1500 * time.Millisecond)
	for {
		c.mu.Lock()
		fresh, wake := c.pubs[key] > before, c.wake
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

// sync sends the document's current text and reports the publish count it
// started from. The version bump and the notify share one lock so a later
// version can never reach the server first; c.mu, which the reader also
// takes, is never held across the write.
func (c *Client) sync(key, uri, languageID, text string) (int, error) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.mu.Lock()
	before := c.pubs[key]
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
	_ = c.call(ctx, "shutdown", nil, nil)
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

func (c *Client) call(ctx context.Context, method string, params, out any) error {
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
			// Server requests: configuration gets one null per item, the rest
			// (capability registration, progress) a null result.
			var result any
			if m.Method == "workspace/configuration" {
				var p struct {
					Items []any `json:"items"`
				}
				_ = json.Unmarshal(m.Params, &p)
				result = make([]any, len(p.Items))
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
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Message  string `json:"message"`
			Source   string `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	u, err := url.Parse(p.URI)
	if err != nil {
		return
	}
	path := normPath(filepath.FromSlash(u.Path))
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
	c.pubs[path]++
	c.seq++
	c.pubSeq[path] = c.seq
	close(c.wake)
	c.wake = make(chan struct{})
}

// normPath resolves symlinks so a server that answers with a canonicalized
// URI (/tmp -> /private/tmp) keys the same file as the path asked about.
func normPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func fileURI(path string) string {
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
