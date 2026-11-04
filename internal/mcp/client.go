// Package mcp is a small Model Context Protocol client (DESIGN §11):
// JSON-RPC over a child process's stdio or over streamable HTTP, with the
// calls zemi needs: initialize, tools/list and tools/call.
package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/set"
)

// ProtocolVersion is the version zemi asks for; a server answers with the
// one it speaks.
const ProtocolVersion = "2025-06-18"

const (
	maxMessage    = 32 << 20
	sessionHeader = "Mcp-Session-Id"
	sendTries     = 3 // attempts per send before a rate limit fails the call
)

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
	Auth    *Auth // OAuth grant for a server that requires sign-in; nil sends none

	// OnToolsChanged runs on a reader goroutine when the server reports a
	// new tool list; it must return quickly.
	OnToolsChanged func()
}

// Tool is one tool a server lists. Annotations are the server's own word
// about what the tool does.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint    bool  `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
	} `json:"annotations,omitempty"`
}

// ReadOnly reports a tool the server marked read-only and not destructive.
func (t Tool) ReadOnly() bool {
	a := t.Annotations
	return a != nil && a.ReadOnlyHint && (a.DestructiveHint == nil || !*a.DestructiveHint)
}

// Content is one part of a tool result. Data and Resource.Blob are base64.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"`
		MimeType string `json:"mimeType,omitempty"`
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
			parts = append(parts, "["+c.Type+" "+cmp.Or(c.URI, c.MimeType)+"]")
		}
	}
	if len(parts) == 0 && r.Structured != nil {
		b, _ := json.Marshal(r.Structured, json.Deterministic(true))
		return string(b)
	}
	return strings.Join(parts, "\n")
}

// Media decodes the first image or audio part; a model that takes images
// gets it instead of Text's placeholder for it.
func (r *Result) Media() (data []byte, mime string, ok bool) {
	for _, c := range r.Content {
		b64, mt := c.Data, c.MimeType
		if c.Resource != nil && c.Resource.Blob != "" {
			b64, mt = c.Resource.Blob, c.Resource.MimeType
		}
		if b64 == "" || !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "audio/") {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(b64); err == nil && len(b) > 0 {
			return b, mt, true
		}
	}
	return nil, "", false
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

type message struct {
	ID     jsontext.Value `json:"id,omitempty"`
	Method string         `json:"method,omitempty"`
	Result jsontext.Value `json:"result,omitempty"`
	Error  *rpcError      `json:"error,omitempty"`
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

	wg      sync.WaitGroup // goroutines spawned for the connection; Close joins them
	closing bool           // mu-guarded: set before Close waits, so no spawn slips past the wait

	onToolsChanged func()
}

// Start connects and runs the initialize handshake.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	c := &Client{pending: map[int64]chan message{}, done: make(chan struct{}), onToolsChanged: cfg.OnToolsChanged}
	switch {
	case cfg.Command != "":
		if err := startStdio(cfg, c); err != nil {
			return nil, err
		}
	case cfg.URL != "":
		c.t = &httpTransport{cfg: cfg, c: c, streams: set.Set[io.Closer]{}}
	default:
		return nil, errors.New("mcp: command or url is required")
	}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// initialize runs the handshake that opens a session.
func (c *Client) initialize(ctx context.Context) error {
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "zemi", "version": "1"},
	}
	if err := c.call(ctx, "initialize", params, &init); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	if ht, ok := c.t.(*httpTransport); ok {
		ht.mu.Lock()
		ht.version = init.ProtocolVersion
		ht.mu.Unlock()
	}
	return c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

// Tools lists every tool, following pagination. NOTE: a server still paging
// after 100 pages is cut short, and the tools left on its later pages never
// appear.
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

// Close ends the connection, stops a child process and joins the goroutines
// still running for it: a send wedged on a full pipe, an SSE drain. A stdio
// server that ignores stdin EOF is killed after its grace, which bounds the
// wait.
func (c *Client) Close() error {
	c.end(errors.New("mcp: closed"))
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	err := c.t.close() // first: a send waiting on the transport returns
	c.wg.Wait()
	return err
}

// spawn runs fn off the caller's goroutine, tracked so Close joins it. Past
// Close it refuses: the connection fn would speak on is gone.
func (c *Client) spawn(fn func()) bool {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return false
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		fn()
	}()
	return true
}

func (c *Client) end(err error) {
	c.endOnce.Do(func() {
		c.endErr = err
		close(c.done)
	})
}

func (c *Client) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v, json.Deterministic(true))
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
		// initialize is the one request the protocol forbids cancelling;
		// without the notice the server keeps working for nobody
		if method != "initialize" {
			c.sendAsync(map[string]any{
				"jsonrpc": "2.0", "method": "notifications/cancelled",
				"params": map[string]any{"requestId": id, "reason": "client cancelled"},
			})
		}
		return ctx.Err()
	case <-c.done:
		return c.endErr
	}
}

// sendAsync sends v off the caller's goroutine: the reader must not wait on
// a server that stopped reading, and neither must a cancelled call.
func (c *Client) sendAsync(v any) {
	c.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.send(ctx, v)
	})
}

// handle routes one incoming message: a response to its caller, a server
// request to answer, or a notification.
func (c *Client) handle(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return
	}
	if data[0] == '[' {
		var batch []jsontext.Value
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
		c.sendAsync(resp)
	case m.Method == "notifications/tools/list_changed":
		if c.onToolsChanged != nil {
			c.onToolsChanged()
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

// stdio

type stdioTransport struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	stdin io.WriteCloser
}

var _ transport = (*stdioTransport)(nil)

// startStdio sets c.t before the reader starts: handle answers server
// requests through it. Close joins the reader, whose own defer reaps the
// server, so neither the pipe nor the process outlives the client.
func startStdio(cfg Config, c *Client) error {
	cmd := proc.Command(cfg.Command, cfg.Args...)
	cmd.Env, cmd.Dir, cmd.Stderr = cfg.Env, cfg.Dir, cfg.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := proc.Start(cmd); err != nil {
		return fmt.Errorf("mcp: start %s: %w", cfg.Command, err)
	}
	c.t = &stdioTransport{cmd: cmd, stdin: stdin}
	c.spawn(func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("reader panic: %v", r)
			}
			// A reader that stopped early (an oversized message, a panic)
			// leaves the server writing into a pipe nobody reads, so Wait
			// would never return.
			if err != nil {
				proc.Kill(cmd)
			}
			if werr := proc.Wait(cmd); err == nil {
				err = cmp.Or(werr, errors.New("exited"))
			}
			c.end(fmt.Errorf("mcp: %s: %w", cfg.Command, err))
		}()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxMessage)
		for sc.Scan() {
			c.handle(sc.Bytes())
		}
		err = sc.Err()
	})
	return nil
}

func (t *stdioTransport) send(_ context.Context, msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.stdin.Write(append(msg, '\n'))
	return err
}

// close closes stdin, which ends a well-behaved server, and kills its
// process group if it is still running a moment later. It skips the send
// lock: a write blocked on a server that stopped reading returns once the
// pipe closes.
func (t *stdioTransport) close() error {
	err := t.stdin.Close()
	time.AfterFunc(2*time.Second, func() { proc.Kill(t.cmd) })
	return err
}

// streamable HTTP

type httpTransport struct {
	cfg     Config
	c       *Client
	mu      sync.Mutex
	session string
	version string
	streams set.Set[io.Closer] // SSE bodies draining off the request path
	renewMu sync.Mutex         // one new session at a time
}

var _ transport = (*httpTransport)(nil)

// renewing marks the handshake renew runs, whose sends must not renew again.
type renewing struct{}

// httpConn is the transport the streams dial with, so closing a client drops
// its idle connections instead of parking them in the process-wide pool.
var httpConn = &http.Transport{}

var httpClient = &http.Client{Transport: httpConn}

// send POSTs one message. The reply is JSON or an SSE stream whose events
// are messages; both are routed through handle before send returns.
func (t *httpTransport) send(ctx context.Context, msg []byte) error {
	// each retry cause runs once: a second 401 is a grant the refresh does
	// not fix, a second 404 a session the renew does not fix
	authTried, renewed := false, false
	for attempts := 0; ; attempts++ {
		resp, bearer, err := t.post(ctx, msg)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && t.cfg.Auth != nil {
			resp.Body.Close()
			if authTried || bearer == "" || !t.cfg.Auth.reject(bearer) {
				return ErrAuthRequired
			}
			authTried = true
			continue
		}
		// the server forgot the session (a redeploy): the spec says start another
		if old := resp.Request.Header.Get(sessionHeader); resp.StatusCode == http.StatusNotFound && old != "" && !renewed && ctx.Value(renewing{}) == nil {
			renewed = true
			resp.Body.Close()
			if err := t.renew(ctx, old); err != nil {
				return err
			}
			continue
		}
		if id := resp.Header.Get(sessionHeader); id != "" {
			t.mu.Lock()
			t.session = id
			t.mu.Unlock()
		}
		switch {
		case resp.StatusCode == http.StatusAccepted:
			resp.Body.Close()
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != ""):
			// A rate limit waits out Retry-After and re-posts; a 503 is the
			// same signal only when the server names a wait.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			err = fmt.Errorf("mcp: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			if after := resp.Header.Get("Retry-After"); after != "" {
				if secs, perr := strconv.Atoi(after); perr == nil {
					err = fmt.Errorf("%w (retry after %ds)", err, secs)
				} else {
					err = fmt.Errorf("%w (retry after %s)", err, after)
				}
			}
			if attempts+1 >= sendTries {
				return err
			}
			select {
			case <-ctx.Done():
				return err
			case <-time.After(retryWait(resp, attempts)):
			}
			continue
		case resp.StatusCode/100 != 2:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			return fmt.Errorf("mcp: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
			// A server may hold its POST stream open after answering. Reading
			// it here would cost the caller the whole request timeout, so the
			// stream is drained off the request path and the reply reaches the
			// waiting call through handle.
			t.mu.Lock()
			t.streams.Add(resp.Body)
			t.mu.Unlock()
			if !t.c.spawn(func() {
				defer func() {
					resp.Body.Close()
					t.mu.Lock()
					t.streams.Remove(resp.Body)
					t.mu.Unlock()
				}()
				defer func() {
					if r := recover(); r != nil {
						t.c.end(fmt.Errorf("mcp: sse reader panic: %v", r))
					}
				}()
				_ = readSSE(resp.Body, t.c.handle)
			}) {
				resp.Body.Close() // Close is draining the streams; nothing would read this
			}
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
		resp.Body.Close()
		if err != nil {
			return err
		}
		t.c.handle(body)
		return nil
	}
}

// retryWait reads Retry-After (seconds or an HTTP-date), clamped to
// [1s, 30s]; without a parsable one it backs off exponentially.
func retryWait(resp *http.Response, attempt int) time.Duration {
	if s := strings.TrimSpace(resp.Header.Get("Retry-After")); s != "" {
		var d time.Duration
		if secs, err := strconv.Atoi(s); err == nil {
			d = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(s); err == nil {
			d = time.Until(t)
		}
		return min(max(d, time.Second), 30*time.Second)
	}
	return time.Duration(1<<attempt) * time.Second
}

// renew replaces the session old unless another call already did. A handshake
// the server fails ends the client, so the server shows as failed with a restart.
func (t *httpTransport) renew(ctx context.Context, old string) error {
	t.renewMu.Lock()
	defer t.renewMu.Unlock()
	t.mu.Lock()
	stale := t.session == old
	if stale {
		t.session = ""
	}
	t.mu.Unlock()
	if !stale {
		return nil
	}
	err := t.c.initialize(context.WithValue(ctx, renewing{}, true))
	if err != nil && ctx.Err() != nil {
		// the caller gave up, not the server: the next call renews again
		t.mu.Lock()
		t.session = old
		t.mu.Unlock()
		return err
	}
	if err != nil {
		t.c.end(err)
	}
	return err
}

// post sends one request with the session headers and, when signed in,
// the bearer token it used.
func (t *httpTransport) post(ctx context.Context, msg []byte) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, "", redactURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	bearer := ""
	if t.cfg.Auth != nil && req.Header.Get("Authorization") == "" {
		if bearer, err = t.cfg.Auth.bearer(ctx); err != nil {
			return nil, "", err
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
	}
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set(sessionHeader, t.session)
	}
	if t.version != "" {
		req.Header.Set("MCP-Protocol-Version", t.version)
	}
	t.mu.Unlock()
	resp, err := httpClient.Do(req)
	return resp, bearer, redactURL(err)
}

// redactURL cuts a *url.Error's URL to scheme://host/path: servers take API
// keys in the query, and the text reaches the model, logs and Lua.
func redactURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		u, perr := url.Parse(ue.URL)
		if perr != nil {
			ue.URL = "(unparsable url)"
		} else {
			ue.URL = u.Scheme + "://" + u.Host + u.EscapedPath()
		}
	}
	return err
}

// close ends the SSE streams still draining in the background, so Close
// leaks neither their drain goroutines nor their connections.
func (t *httpTransport) close() error {
	t.mu.Lock()
	streams := t.streams
	t.streams = set.Set[io.Closer]{}
	t.mu.Unlock()
	for b := range streams {
		_ = b.Close()
	}
	httpConn.CloseIdleConnections()
	return nil
}

// readSSE feeds each event's data to handle until the stream ends.
func readSSE(r io.Reader, handle func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var data []string
	size := 0
	flush := func() {
		if len(data) > 0 {
			handle([]byte(strings.Join(data, "\n")))
			data, size = nil, 0
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
		} else if v, ok := strings.CutPrefix(line, "data:"); ok {
			v = strings.TrimPrefix(v, " ")
			if size += len(v); size > maxMessage {
				return errors.New("mcp: sse event exceeds the message cap")
			}
			data = append(data, v)
		}
	}
	flush()
	return sc.Err()
}
