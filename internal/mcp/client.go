// Package mcp is a small Model Context Protocol client (DESIGN §13, M5):
// JSON-RPC over a child process's stdio or over streamable HTTP, with the
// calls zemi needs: initialize, tools/list and tools/call.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the version zemi asks for; a server answers with the
// one it speaks.
const ProtocolVersion = "2025-06-18"

const maxMessage = 32 << 20

// Config picks the transport: Command starts a child process, URL talks
// streamable HTTP.
type Config struct {
	Command string
	Args    []string
	Env     []string // the child's whole environment
	Dir     string
	Stderr  io.Writer // child stderr; nil discards it

	URL     string
	Headers map[string]string
}

// Tool is one tool a server lists.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Content is one part of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text,omitempty"`
	} `json:"resource,omitempty"`
}

// Result is a tools/call result.
type Result struct {
	Content    []Content `json:"content"`
	IsError    bool      `json:"isError"`
	Structured any       `json:"structuredContent,omitempty"`
}

// Text flattens a result for a model: text as is, other parts as a short
// note, structured content as JSON when nothing else came back.
func (r *Result) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch {
		case c.Type == "text":
			parts = append(parts, c.Text)
		case c.Resource != nil && c.Resource.Text != "":
			parts = append(parts, c.Resource.Text)
		case c.Resource != nil:
			parts = append(parts, "["+c.Type+" "+c.Resource.URI+"]")
		default:
			parts = append(parts, strings.TrimSpace("["+c.Type+" "+c.MimeType+"]"))
		}
	}
	if len(parts) == 0 && r.Structured != nil {
		b, _ := json.Marshal(r.Structured)
		return string(b)
	}
	return strings.Join(parts, "\n")
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type transport interface {
	send(ctx context.Context, msg []byte) error
	close() error
}

// Client is one server connection. Calls are safe from any goroutine.
type Client struct {
	t       transport
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan message
	done    chan struct{}
	endOnce sync.Once
	endErr  error

	// OnToolsChanged runs on the reader goroutine when the server reports
	// a new tool list; it must return quickly.
	OnToolsChanged func()
}

// Start connects and runs the initialize handshake.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	c := &Client{pending: map[int64]chan message{}, done: make(chan struct{})}
	switch {
	case cfg.Command != "":
		t, err := startStdio(cfg, c)
		if err != nil {
			return nil, err
		}
		c.t = t
	case cfg.URL != "":
		c.t = &httpTransport{cfg: cfg, c: c}
	default:
		return nil, errors.New("mcp: command or url is required")
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "zemi", "version": "1"},
	}
	if err := c.call(ctx, "initialize", params, &init); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: initialize: %w", err)
	}
	if ht, ok := c.t.(*httpTransport); ok {
		ht.mu.Lock()
		ht.version = init.ProtocolVersion
		ht.mu.Unlock()
	}
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Tools lists every tool, following pagination.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for range 100 {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// CallTool runs one tool. A tool-level failure is a Result with IsError;
// the error return is for protocol and transport failures.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res Result
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Done is closed when the connection ends; Err says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is the reason the connection ended (nil while it is open).
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.endErr
	default:
		return nil
	}
}

// Close ends the connection and stops a child process.
func (c *Client) Close() error {
	c.end(errors.New("mcp: closed"))
	return c.t.close()
}

func (c *Client) end(err error) {
	c.endOnce.Do(func() {
		c.endErr = err
		close(c.done)
	})
}

func (c *Client) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.t.send(ctx, b)
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
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.send(ctx, req); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out == nil || len(m.Result) == 0 {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.endErr
	}
}

// handle routes one incoming message: a response to its caller, a server
// request to answer, or a notification.
func (c *Client) handle(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return
	}
	if data[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(data, &batch) == nil {
			for _, m := range batch {
				c.handle(m)
			}
		}
		return
	}
	var m message
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		// Server requests zemi does not offer (sampling, roots, elicitation)
		// get method-not-found; ping gets its empty result.
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if m.Method == "ping" {
			resp["result"] = map[string]any{}
		} else {
			resp["error"] = rpcError{Code: -32601, Message: "method not found"}
		}
		b, _ := json.Marshal(resp)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = c.t.send(ctx, b)
		}()
	case m.Method == "notifications/tools/list_changed":
		if c.OnToolsChanged != nil {
			c.OnToolsChanged()
		}
	case m.Method == "":
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

// ---------------------------------------------------------------------------
// stdio

type stdioTransport struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	stdin io.WriteCloser
}

func startStdio(cfg Config, c *Client) (*stdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env, cmd.Dir, cmd.Stderr = cfg.Env, cfg.Dir, cfg.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %s: %w", cfg.Command, err)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.end(fmt.Errorf("mcp: reader panic: %v", r))
			}
		}()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxMessage)
		for sc.Scan() {
			c.handle(sc.Bytes())
		}
		err := cmd.Wait()
		if err == nil {
			err = errors.New("exited")
		}
		c.end(fmt.Errorf("mcp: %s: %w", cfg.Command, err))
	}()
	return &stdioTransport{cmd: cmd, stdin: stdin}, nil
}

func (t *stdioTransport) send(_ context.Context, msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.stdin.Write(append(msg, '\n'))
	return err
}

// close closes stdin, which ends a well-behaved server, and kills it if it
// is still running a moment later.
func (t *stdioTransport) close() error {
	t.mu.Lock()
	err := t.stdin.Close()
	t.mu.Unlock()
	time.AfterFunc(2*time.Second, func() { _ = t.cmd.Process.Kill() })
	return err
}

// ---------------------------------------------------------------------------
// streamable HTTP

type httpTransport struct {
	cfg     Config
	c       *Client
	mu      sync.Mutex
	session string
	version string
}

var httpClient = &http.Client{}

// send POSTs one message. The reply is JSON or an SSE stream whose events
// are messages; both are routed through handle before send returns.
func (t *httpTransport) send(ctx context.Context, msg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.version != "" {
		req.Header.Set("MCP-Protocol-Version", t.version)
	}
	t.mu.Unlock()
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		t.session = id
		t.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("mcp: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
		return readSSE(resp.Body, t.c.handle)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return err
	}
	t.c.handle(body)
	return nil
}

func (t *httpTransport) close() error { return nil }

// readSSE feeds each event's data to handle until the stream ends.
func readSSE(r io.Reader, handle func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var data []string
	flush := func() {
		if len(data) > 0 {
			handle([]byte(strings.Join(data, "\n")))
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return sc.Err()
}
