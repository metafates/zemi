package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/mcp/mcptest"
)

// The test binary doubles as the stdio server when ZEMI_MCP_FAKE is set.
func TestMain(m *testing.M) {
	if os.Getenv("ZEMI_MCP_FAKE") == "1" {
		mcptest.ServeStdio(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	for name, cfg := range map[string]Config{
		"stdio": {Command: os.Args[0], Env: append(os.Environ(), "ZEMI_MCP_FAKE=1")},
		"http":  {URL: srv.URL},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tools, err := c.Tools(ctx)
			if err != nil || len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "fail" {
				t.Fatalf("tools = %+v, %v", tools, err)
			}
			res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
			if err != nil || res.IsError || res.Text() != "hi" {
				t.Fatalf("echo = %+v, %v", res, err)
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
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	// close(release) must run first, or srv.Close() waits forever on the
	// handler parked there; defers run last in, first out.
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil || res.Text() != "hi" {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the call waited %s for a stream the server held open", elapsed)
	}
}

// TestCallCancelNotifies: a caller that gives up tells the server to stop,
// so it does not keep working on an answer nothing is waiting for.
func TestCallCancelNotifies(t *testing.T) {
	cancelled := make(chan int64, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Method string `json:"method"`
			Params struct {
				RequestID int64 `json:"requestId"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &m)
		switch m.Method {
		case "notifications/cancelled":
			select {
			case cancelled <- m.Params.RequestID:
			default:
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/call": // accepted, answered never
			w.WriteHeader(http.StatusAccepted)
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

	c, err := Start(context.Background(), Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
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
		"oversize": "head -c 40000000 /dev/zero | tr '\\0' a; sleep 30",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			if _, err := Start(ctx, Config{Command: "sh", Args: []string{"-c", script}}); err == nil {
				t.Fatal("start against a broken server must fail")
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("start waited %s", elapsed)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = Start(ctx, Config{Command: "sh", Args: []string{"-c", script}, OnToolsChanged: func() { changed <- struct{}{} }})
	select {
	case <-changed:
	default:
		t.Fatal("OnToolsChanged did not run")
	}
}

// TestSSEEventCap: an event whose data lines never end is refused at the
// message cap instead of growing without bound.
func TestSSEEventCap(t *testing.T) {
	stream := strings.Repeat("data: "+strings.Repeat("x", 1<<20)+"\n", maxMessage>>20+1)
	if err := readSSE(strings.NewReader(stream), func([]byte) { t.Fatal("oversized event delivered") }); err == nil {
		t.Fatal("oversized event read without an error")
	}
}

// TestStdioReaderPanic: a panic on the reader ends the connection and
// reaps the server, which would otherwise keep running unwaited.
func TestStdioReaderPanic(t *testing.T) {
	script := `read l; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'; read l; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}'; sleep 30`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Start(ctx, Config{Command: "sh", Args: []string{"-c", script}, OnToolsChanged: func() { panic("boom") }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("the connection outlived a reader panic")
	}
	if state := c.t.(*stdioTransport).cmd.ProcessState; state == nil || !strings.Contains(c.Err().Error(), "reader panic") {
		t.Fatalf("err %v, server state %v", c.Err(), state)
	}
}

func TestTransportErrorRedactsURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := func(u string) error { _, err := Start(ctx, Config{URL: u}); return err }
	token := func(u string) error { _, err := tokenRequest(ctx, &Token{TokenURL: u}, url.Values{}); return err }
	for name, err := range map[string]error{
		"post":    start("http://u:pw123@127.0.0.1:1/mcp?exaApiKey=SECRET123"),
		"badURL":  start("http://127.0.0.1:1/mcp?key=SECRET123\x7f"),
		"getJSON": getJSON(ctx, "http://127.0.0.1:1/meta?key=SECRET123", &struct{}{}),
		"token":   token("http://127.0.0.1:1/t?key=SECRET123"),
	} {
		if err == nil || strings.Contains(err.Error(), "SECRET123") || strings.Contains(err.Error(), "pw123") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
