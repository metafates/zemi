// Package mcp is a Model Context Protocol client (DESIGN §11) built on
// github.com/modelcontextprotocol/go-sdk: the transports, the handshake and
// the tool calls are the SDK's; this file is the facade zemi's hostapi speaks,
// the process boundary for stdio servers, and the URL redaction.
package mcp

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/metafates/zemi/internal/proc"
)

// ProtocolVersion is the version zemi asks for; a server answers with the
// one it speaks.
const ProtocolVersion = "2025-06-18"

const _maxEvent = 32 << 20

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
	Name        string
	Description string
	InputSchema map[string]any
	Annotations *struct {
		ReadOnlyHint    bool
		DestructiveHint *bool
	}
}

// ReadOnly reports a tool the server marked read-only and not destructive.
func (t Tool) ReadOnly() bool {
	a := t.Annotations
	return a != nil && a.ReadOnlyHint && (a.DestructiveHint == nil || !*a.DestructiveHint)
}

// Content is one part of a tool result. Data and Resource.Blob are decoded.
type Content struct {
	Type     string // text, image, audio, resource_link, resource
	Text     string
	Data     []byte
	URI      string
	MimeType string
	Resource *struct {
		URI      string
		Text     string
		Blob     []byte
		MimeType string
	}
}

// Result is a tools/call result.
type Result struct {
	Content    []Content
	IsError    bool
	Structured any
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
		data, mime = c.Data, c.MimeType
		if c.Resource != nil && len(c.Resource.Blob) > 0 {
			data, mime = c.Resource.Blob, c.Resource.MimeType
		}
		if len(data) > 0 && (strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/")) {
			return data, mime, true
		}
	}
	return nil, "", false
}

// NOTE: the SDK's HTTP transport logger is unexported, so some of its
// transport warnings land in slog's discard.

// Client is one server connection. Calls are safe from any goroutine.
type Client struct {
	cfg Config

	sctx   context.Context // the session context: cancelling it returns every in-flight call
	cancel context.CancelFunc

	mu  sync.Mutex
	cs  *sdk.ClientSession
	gen int

	renewMu sync.Mutex // one re-Connect at a time

	done    chan struct{}
	endOnce sync.Once
	endErr  error

	closeOnce sync.Once

	cmd     *exec.Cmd // a stdio server's child
	release func()    // proc.Adopt's, run once the child is reaped
}

// Start connects and runs the initialize handshake.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Command == "" && cfg.URL == "" {
		return nil, errors.New("mcp: command or url is required")
	}
	c := &Client{cfg: cfg, done: make(chan struct{})}
	c.sctx, c.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if err := c.connect(ctx); err != nil {
		return nil, redactAll(err)
	}
	go c.watch()
	return c, nil
}

// connect dials the transport and runs the handshake; the session it leaves
// in c.cs outlives ctx.
func (c *Client) connect(ctx context.Context) error {
	if err := proc.Refused(); err != nil {
		return err
	}
	opts := &sdk.ClientOptions{
		// empty on purpose: no roots advertised, so unknown server requests
		// get method-not-found and ping is answered, the wire zemi has always
		// spoken; the SDK default would advertise roots
		Capabilities: &sdk.ClientCapabilities{},
	}
	if fn := c.cfg.OnToolsChanged; fn != nil {
		// a reader-goroutine panic must end the connection, not zemi
		opts.ToolListChangedHandler = func(context.Context, *sdk.ToolListChangedRequest) {
			defer func() {
				if r := recover(); r != nil {
					c.end(fmt.Errorf("mcp: tools changed handler panic: %v", r))
				}
			}()
			fn()
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "zemi", Version: "1"}, opts)
	var transport sdk.Transport
	switch {
	case c.cfg.Command != "":
		cmd := proc.Command(c.cfg.Command, c.cfg.Args...)
		cmd.Env, cmd.Dir, cmd.Stderr = c.cfg.Env, c.cfg.Dir, c.cfg.Stderr
		// the SDK calls Start and Wait itself; Adopt keeps the child killable
		c.cmd = cmd
		transport = &sdk.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}
	default:
		st := &sdk.StreamableClientTransport{Endpoint: c.cfg.URL, MaxEventSize: _maxEvent}
		if len(c.cfg.Headers) > 0 {
			st.HTTPClient = &http.Client{Transport: headerTransport{base: &http.Transport{}, headers: c.cfg.Headers}}
		}
		if c.cfg.Auth != nil {
			st.OAuthHandler = passiveHandler{c.cfg.Auth}
		}
		transport = st
	}
	cs, err := client.Connect(ctx, transport, &sdk.ClientSessionOptions{ProtocolVersion: ProtocolVersion})
	if err != nil {
		if c.cmd != nil {
			proc.Kill(c.cmd) // never started or left running by a failed handshake
		}
		return err
	}
	if c.cmd != nil && c.release == nil {
		c.release = proc.Adopt(c.cmd)
	}
	c.mu.Lock()
	c.cs, c.gen = cs, c.gen+1
	c.mu.Unlock()
	return nil
}

// watch ends the client when its session does; a session replaced by a
// re-Connect keeps it watching the new one.
func (c *Client) watch() {
	for {
		c.mu.Lock()
		cs, gen := c.cs, c.gen
		c.mu.Unlock()
		if cs == nil {
			return
		}
		err := cs.Wait()
		c.mu.Lock()
		current := c.cs == cs && gen == c.gen
		c.mu.Unlock()
		if !current {
			continue
		}
		c.reap(cs)
		c.end(fmt.Errorf("mcp: %s: %w", cmp.Or(c.cfg.Command, "http"), cmp.Or(err, errors.New("exited"))))
		return
	}
}

// reap closes the session again (idempotent), kills the child's group and
// forgets the pid.
func (c *Client) reap(cs *sdk.ClientSession) {
	if cs != nil {
		_ = cs.Close() // idempotent; the process is killed below regardless
	}
	if c.cmd != nil {
		proc.Kill(c.cmd)
	}
	c.mu.Lock()
	release := c.release
	c.release = nil
	c.mu.Unlock()
	if release != nil {
		release()
	}
}

// Tools lists every tool, following pagination. NOTE: a server still paging
// after 100 pages is cut short, and the tools left on its later pages never
// appear.
func (c *Client) Tools(ctx context.Context) (all []Tool, err error) {
	err = c.withSession(ctx, func(ctx context.Context, cs *sdk.ClientSession) error {
		all = nil
		cursor := ""
		for range 100 {
			page, err := cs.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
			if err != nil {
				return err
			}
			for _, t := range page.Tools {
				all = append(all, toolFromSDK(t))
			}
			if page.NextCursor == "" {
				return nil
			}
			cursor = page.NextCursor
		}
		return nil
	})
	return all, redactAll(err)
}

// CallTool runs one tool. A tool-level failure is a Result with IsError;
// the error return is for protocol and transport failures.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res *Result
	err := c.withSession(ctx, func(ctx context.Context, cs *sdk.ClientSession) error {
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err
		}
		res = resultFromSDK(r)
		return nil
	})
	return res, redactAll(err)
}

// withSession runs fn against the live session. A session the server forgot
// (404 on a session id: a redeploy) is re-Connected once, on the same
// transport config, and fn retried.
func (c *Client) withSession(ctx context.Context, fn func(context.Context, *sdk.ClientSession) error) error {
	gen := c.generation()
	err := c.call(ctx, fn)
	if !errors.Is(err, sdk.ErrSessionMissing) {
		return err
	}
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	if c.generation() == gen {
		if err := c.connect(c.sctx); err != nil {
			return err
		}
	}
	return c.call(ctx, fn)
}

// call merges the session context into ctx, so Close returns every in-flight
// call at once, and runs fn with the result.
func (c *Client) call(ctx context.Context, fn func(context.Context, *sdk.ClientSession) error) error {
	c.mu.Lock()
	cs := c.cs
	c.mu.Unlock()
	if cs == nil {
		return errors.New("mcp: closed")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.sctx, cancel)
	defer stop()
	return fn(cctx, cs)
}

func (c *Client) generation() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
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

// Close cancels every in-flight call, ends the session and stops a child
// process: stdin closes first, a server that ignores that is terminated
// after its grace, and the group kill takes what is left.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.mu.Lock()
		cs := c.cs
		c.cs = nil
		c.mu.Unlock()
		c.reap(cs)
		c.end(errors.New("mcp: closed"))
	})
	return nil
}

func (c *Client) end(err error) {
	c.endOnce.Do(func() {
		c.endErr = err
		close(c.done)
	})
}

func toolFromSDK(t *sdk.Tool) Tool {
	out := Tool{Name: t.Name, Description: t.Description, InputSchema: schemaMap(t.InputSchema)}
	if a := t.Annotations; a != nil {
		ann := &struct {
			ReadOnlyHint    bool
			DestructiveHint *bool
		}{ReadOnlyHint: a.ReadOnlyHint, DestructiveHint: a.DestructiveHint}
		out.Annotations = ann
	}
	return out
}

// schemaMap keeps the schema a server sent as the map hostapi hands to
// ResolveSchemaRefs; anything else arrives as nil and drops the tool.
func schemaMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func resultFromSDK(r *sdk.CallToolResult) *Result {
	out := &Result{IsError: r.IsError, Structured: r.StructuredContent}
	for _, c := range r.Content {
		var fc Content
		switch c := c.(type) {
		case *sdk.TextContent:
			fc = Content{Type: "text", Text: c.Text}
		case *sdk.ImageContent:
			fc = Content{Type: "image", Data: c.Data, MimeType: c.MIMEType}
		case *sdk.AudioContent:
			fc = Content{Type: "audio", Data: c.Data, MimeType: c.MIMEType}
		case *sdk.ResourceLink:
			fc = Content{Type: "resource_link", URI: c.URI, MimeType: c.MIMEType}
		case *sdk.EmbeddedResource:
			rc := &struct {
				URI      string
				Text     string
				Blob     []byte
				MimeType string
			}{}
			if c.Resource != nil {
				rc.URI, rc.Text, rc.Blob, rc.MimeType = c.Resource.URI, c.Resource.Text, c.Resource.Blob, c.Resource.MIMEType
			}
			fc = Content{Type: "resource", Resource: rc}
		default:
			fc = Content{Type: "unknown"}
		}
		out.Content = append(out.Content, fc)
	}
	return out
}

// headerTransport sets a server's static headers on every request; the
// transport's own client has no Timeout, since the standalone SSE GET is
// long-lived.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, redactAll(err)
	}
	return resp, nil
}

// redactAll cuts every URL in err's text to scheme://host/path: servers take
// API keys in the query, and the text reaches the model, logs and Lua. The
// in-chain *url.Errors are cut too, so a wrap after this one stays clean;
// the SDK bakes url.Errors into its messages eagerly (fmt renders %w at
// Errorf), which is why the text itself gets scrubbed.
func redactAll(err error) error {
	if err == nil {
		return nil
	}
	// the SDK bakes url.Errors into its messages eagerly (fmt renders %w at
	// Errorf), so the redaction has to run on the rendered text
	clean := _urlInText.ReplaceAllStringFunc(err.Error(), func(u string) string {
		pu, perr := url.Parse(u)
		if perr != nil {
			return "(unparsable url)"
		}
		return pu.Scheme + "://" + pu.Host + pu.EscapedPath()
	})
	if clean == err.Error() {
		return err
	}
	return redacted{err: err, text: clean}
}

// _urlInText finds an http(s) URL in an error's rendered text.
var _urlInText = regexp.MustCompile(`https?://[^\s"']+`)

// redacted carries an error whose rendered text had its URLs cut; errors.Is
// and errors.As still see the original.
type redacted struct {
	err  error
	text string
}

func (e redacted) Error() string { return e.text }
func (e redacted) Unwrap() error { return e.err }
