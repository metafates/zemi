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
	stdin io.WriteCloser

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan message
	diags   map[string][]Diagnostic // by absolute path
	pubs    map[string]int          // publish count by path
	opened  map[string]int          // document version by path
	wake    chan struct{}           // closed and replaced on every publish

	done    chan struct{}
	endOnce sync.Once
	endErr  error
}

// Start launches the server and runs the initialize handshake; ctx bounds
// the handshake only, not the server's life.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env, cmd.Dir, cmd.Stderr = cfg.Env, cfg.Root, cfg.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", cfg.Command, err)
	}
	c := &Client{
		cmd: cmd, stdin: stdin,
		pending: map[int64]chan message{}, diags: map[string][]Diagnostic{},
		pubs: map[string]int{}, opened: map[string]int{},
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

// Touch syncs path's current content to the server and waits for the
// diagnostics that follow (servers often publish twice, a quick pass then
// the full check, so a short quiet period ends the wait). On timeout it
// returns what it has with ctx's error.
func (c *Client) Touch(ctx context.Context, path, languageID string) ([]Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	uri := fileURI(path)
	c.mu.Lock()
	before := c.pubs[path]
	version := c.opened[path] + 1
	c.opened[path] = version
	c.mu.Unlock()
	if version == 1 {
		err = c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": uri, "languageId": languageID, "version": version, "text": string(data),
		}})
	} else {
		err = c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []any{map[string]any{"text": string(data)}},
		})
	}
	if err != nil {
		return nil, err
	}
	_ = c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})

	quiet := (<-chan time.Time)(nil)
	for {
		c.mu.Lock()
		fresh, wake := c.pubs[path] > before, c.wake
		c.mu.Unlock()
		if fresh && quiet == nil {
			quiet = time.After(300 * time.Millisecond)
		}
		select {
		case <-wake:
			if fresh {
				quiet = time.After(300 * time.Millisecond)
			}
		case <-quiet:
			return c.Diagnostics(path), nil
		case <-ctx.Done():
			return c.Diagnostics(path), ctx.Err()
		case <-c.done:
			return c.Diagnostics(path), c.endErr
		}
	}
}

// Diagnostics returns the last published diagnostics for path.
func (c *Client) Diagnostics(path string) []Diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Diagnostic(nil), c.diags[path]...)
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
	time.AfterFunc(2*time.Second, func() { _ = c.cmd.Process.Kill() })
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
			werr := c.cmd.Wait()
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
				ch <- m
			}
		}
	}
}

func (c *Client) publish(raw json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
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
	path := filepath.FromSlash(u.Path)
	var out []Diagnostic
	for _, d := range p.Diagnostics {
		sev := d.Severity
		if sev == 0 {
			sev = 1
		}
		out = append(out, Diagnostic{Line: d.Range.Start.Line + 1, Col: d.Range.Start.Character + 1, Severity: sev, Message: d.Message, Source: d.Source})
	}
	c.mu.Lock()
	c.diags[path] = out
	c.pubs[path]++
	close(c.wake)
	c.wake = make(chan struct{})
	c.mu.Unlock()
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
