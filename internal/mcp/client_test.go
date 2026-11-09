package mcp

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/goleak"

	"github.com/metafates/zemi/internal/mcp/mcptest"
)

// The test binary doubles as the stdio server when ZEMI_MCP_FAKE is set.
func TestMain(m *testing.M) {
	if os.Getenv("ZEMI_MCP_FAKE") == "1" {
		mcptest.ServeStdio(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	goleak.VerifyTestMain(m)
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	for name, cfg := range map[string]Config{
		"stdio": {Command: os.Args[0], Env: append(os.Environ(), "ZEMI_MCP_FAKE=1")},
		"http":  {URL: srv.URL},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tools, err := c.Tools(ctx)
			if err != nil || len(tools) != 3 || tools[0].Name != "echo" || tools[2].Name != "fail" {
				t.Fatalf("tools = %+v, %v", tools, err)
			}
			if tools[0].ReadOnly() || !tools[1].ReadOnly() {
				t.Fatalf("read-only hints: %+v", tools)
			}
			res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
			if err != nil || res.IsError || res.Text() != "hi" {
				t.Fatalf("echo = %+v, %v", res, err)
			}
			res, err = c.CallTool(ctx, "shot", nil)
			if data, mime, ok := res.Media(); err != nil || !ok || mime != "image/png" || len(data) == 0 {
				t.Fatalf("shot = %+v (%d bytes of %s), %v", res, len(data), mime, err)
			}
			res, err = c.CallTool(ctx, "fail", nil)
			if err != nil || !res.IsError || res.Text() != "boom" {
				t.Fatalf("fail = %+v, %v", res, err)
			}
			if _, err := c.CallTool(ctx, "nope", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("unknown tool err = %v", err)
			}
		})
	}
}

// TestHTTPStreamHeldOpen: a server that keeps its POST stream open after
// answering must not cost the caller its whole timeout. The stream drains
// off the request path and the reply reaches the call as soon as it lands.
func TestHTTPStreamHeldOpen(t *testing.T) {
	release := make(chan struct{})
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp := mcptest.Respond(body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "fake-session")
			w.Header().Set("Content-Type", "application/json")
			w.Write(resp)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\ndata: " + string(resp) + "\n\n"))
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("the test server's writer %T cannot flush", w)
			return
		}
		f.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			close(gone)
		}
	}))
	defer srv.Close()
	// close(release) must run first, or srv.Close() waits forever on the
	// handler parked there; defers run last in, first out.
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil || res.Text() != "hi" {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	if ctx.Err() != nil {
		t.Fatal("the call waited out its deadline on a stream the server held open")
	}
	c.Close()
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close left the held-open stream open")
	}
}

// TestHTTPSessionReconnect: a server that forgets the session answers 404;
// the client starts a new one on the same transport and resends.
func TestHTTPSessionReconnect(t *testing.T) {
	var (
		mu    sync.Mutex
		live  = ""
		inits = 0
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		defer mu.Unlock()
		if m.Method == "initialize" {
			inits++
			live = "s" + strconv.Itoa(inits)
			w.Header().Set("Mcp-Session-Id", live)
			w.Header().Set("Content-Type", "application/json")
		} else if r.Header.Get("Mcp-Session-Id") != live {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		resp := mcptest.Respond(body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if m.Method != "initialize" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Write(resp)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cl, err := Start(ctx, Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if res, err := cl.CallTool(ctx, "echo", map[string]any{"text": "hi"}); err != nil || res.Text() != "hi" {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	mu.Lock()
	live = "forgotten" // a redeploy dropped the session
	mu.Unlock()
	if res, err := cl.CallTool(ctx, "echo", map[string]any{"text": "hi"}); err != nil || res.Text() != "hi" {
		t.Fatalf("echo after the session was forgotten = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if inits != 2 {
		t.Fatalf("%d handshakes, want 2", inits)
	}
}

// TestCallCancelNotifies: a caller that gives up tells the server to stop,
// so it does not keep working on an answer nothing is waiting for.
func TestCallCancelNotifies(t *testing.T) {
	cancelled := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Method string `json:"method"`
			Params struct {
				RequestID jsonv1.Number `json:"requestId"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &m)
		switch m.Method {
		case "notifications/cancelled":
			select {
			case cancelled <- m.Params.RequestID.String():
			default:
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/call": // answered never: the stream stays open
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		default:
			resp := mcptest.Respond(body)
			if resp == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Mcp-Session-Id", "fake-session")
			w.Header().Set("Content-Type", "application/json")
			w.Write(resp)
		}
	}))
	defer srv.Close()

	c, err := Start(t.Context(), Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"}); err == nil {
		t.Fatal("a call against a silent server returned no error")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("no notifications/cancelled reached the server")
	}
}

// TestStdioBroken: a server that dies, or sends a message over the size cap
// and keeps writing, ends the connection instead of leaving calls to time out.
func TestStdioBroken(t *testing.T) {
	for name, script := range map[string]string{
		"exit":     "exit 3",
		"oversize": "head -c 40000000 /dev/zero; sleep 30",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := Start(ctx, Config{Command: "sh", Args: []string{"-c", script}}); err == nil {
				t.Fatal("start against a broken server must fail")
			}
			if ctx.Err() != nil {
				t.Fatal("start waited out its deadline")
			}
		})
	}
}

// TestStdioEarlyMessages: a server may ping and announce a tool list change
// before it answers initialize; both are handled while Start still waits.
// The server announces the change only after it reads the ping reply.
func TestStdioEarlyMessages(t *testing.T) {
	changed := make(chan struct{}, 1)
	script := `printf '%s\n' '{"jsonrpc":"2.0","id":"p","method":"ping"}'
while read l; do case $l in *'"id":"p"'*'"result":{}'*) break;; esac; done
printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}'`
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _ = Start(ctx, Config{Command: "sh", Args: []string{"-c", script}, OnToolsChanged: func() { changed <- struct{}{} }})
	select {
	case <-changed:
	default:
		t.Fatal("OnToolsChanged did not run")
	}
}

// TestToolsChangedPanic: a panic in the handler ends the connection instead
// of taking zemi down, and reaps the server, which would otherwise keep
// running unwaited.
func TestToolsChangedPanic(t *testing.T) {
	script := `read l; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"x","version":"1"}}}'; read l; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}'; sleep 30`
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{Command: "sh", Args: []string{"-c", script}, OnToolsChanged: func() { panic("boom") }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("the connection outlived a handler panic")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportErrorRedactsURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := func(u string) error { _, err := Start(ctx, Config{URL: u}); return err }
	for name, err := range map[string]error{
		"post":   start("http://u:pw123@127.0.0.1:1/mcp?exaApiKey=SECRET123"),
		"badURL": start("http://127.0.0.1:1/mcp?key=SECRET123\x7f"),
	} {
		if err == nil || strings.Contains(err.Error(), "SECRET123") || strings.Contains(err.Error(), "pw123") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestResultText: each content part flattens to what a model reads; a
// resource_link keeps its URI, the one field that made it meaningful. The
// wire is decoded the way a call's result arrives.
func TestResultText(t *testing.T) {
	for _, c := range []struct {
		name, wire, want string
	}{
		{"text", `{"content":[{"type":"text","text":"hi"}]}`, "hi"},
		{"resource link", `{"content":[{"type":"resource_link","uri":"file:///tmp/a"}]}`, "[resource_link file:///tmp/a]"},
		{"embedded text", `{"content":[{"type":"resource","resource":{"uri":"u","text":"readme"}}]}`, "readme"},
		{"embedded blob", `{"content":[{"type":"resource","resource":{"uri":"u","mimeType":"image/png","blob":"aGk="}}]}`, "[resource u]"},
		{"image placeholder", `{"content":[{"type":"image","mimeType":"image/png"}]}`, "[image image/png]"},
		{"structured only", `{"structuredContent":{"a":1}}`, `{"a":1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var r sdk.CallToolResult
			if err := jsonv1.Unmarshal([]byte(c.wire), &r); err != nil {
				t.Fatal(err)
			}
			if got := resultFromSDK(&r).Text(); got != c.want {
				t.Errorf("Text = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHTTPAuthAfterRenew: a 401 on a refused token is a sign-in ask, never a
// generic http error - unless the grant's refresh fixes it, in which case the
// retried request carries the fresh one and the server answers.
func TestHTTPAuthAfterRenew(t *testing.T) {
	for _, c := range []struct {
		name    string
		grant   bool
		wantErr error
	}{
		{"no grant", false, ErrAuthRequired},
		{"the refresh fixes it", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				tools   int
				n       = 1 // the grant's refresh token is r1
				current = "t1"
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var m struct {
					Method string `json:"method"`
				}
				_ = json.Unmarshal(body, &m)
				mu.Lock()
				defer mu.Unlock()
				switch m.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "s1")
					w.Header().Set("Content-Type", "application/json")
					w.Write(mcptest.Respond(body))
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					tools++
					if r.Header.Get("Authorization") != "Bearer "+current {
						w.Header().Set("WWW-Authenticate", `Bearer`)
						http.Error(w, "stale token", http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write(mcptest.Respond(body))
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer srv.Close()
			tSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				mu.Lock()
				defer mu.Unlock()
				if r.Form.Get("refresh_token") != fmt.Sprint("r", n) {
					http.Error(w, "invalid_grant", http.StatusBadRequest)
					return
				}
				n++
				current = fmt.Sprint("t", n)
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, map[string]any{
					"access_token": current, "refresh_token": fmt.Sprint("r", n), "expires_in": 3600,
				}, json.Deterministic(true))
			}))
			defer tSrv.Close()

			auth := &Auth{ServerURL: srv.URL, Path: filepath.Join(t.TempDir(), "grant.json")}
			if c.grant {
				if err := auth.save(&Token{ServerURL: srv.URL, AccessToken: "t1", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute), ClientID: "c", TokenURL: tSrv.URL}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cl, err := Start(ctx, Config{URL: srv.URL, Auth: auth})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cl.Close()
			_, err = cl.Tools(ctx)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("tools err = %v, want %v", err, c.wantErr)
			}
			if c.wantErr != nil && cl.Err() != nil {
				t.Fatalf("a sign-in ask ended the client: %v", cl.Err())
			}
		})
	}
}
