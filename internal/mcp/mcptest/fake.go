// Package mcptest is a fake MCP server for tests, reachable over stdio and
// HTTP: it lists echo, shot and fail over two pages; echo returns its text
// argument, shot is annotated read-only and returns a png, and fail returns
// a tool error. Auth gates the HTTP server behind the OAuth flow zemi runs.
package mcptest

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
)

// Respond answers one JSON-RPC message; notifications get nil.
func Respond(line []byte) []byte {
	var m struct {
		ID     jsontext.Value `json:"id"`
		Method string         `json:"method"`
		Params struct {
			Cursor    string         `json:"cursor"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || len(m.ID) == 0 {
		return nil
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	tool := func(name, desc string) map[string]any {
		return map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"},
		}}
	}
	switch m.Method {
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake", "version": "1"},
		}
	case "tools/list":
		if m.Params.Cursor == "" {
			shot := tool("shot", "take a screenshot")
			shot["annotations"] = map[string]any{"readOnlyHint": true}
			resp["result"] = map[string]any{"tools": []any{tool("echo", "echo the text"), shot}, "nextCursor": "2"}
		} else {
			resp["result"] = map[string]any{"tools": []any{tool("fail", "always fails")}}
		}
	case "tools/call":
		switch m.Params.Name {
		case "echo":
			text, _ := m.Params.Arguments["text"].(string)
			resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		case "shot":
			resp["result"] = map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "the screen"},
				map[string]any{"type": "image", "mimeType": "image/png", "data": onePixelPNG},
			}}
		case "fail":
			resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "boom"}}, "isError": true}
		default:
			resp["error"] = map[string]any{"code": -32602, "message": "unknown tool " + m.Params.Name}
		}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	b, _ := json.Marshal(resp, json.Deterministic(true))
	return b
}

// onePixelPNG is a 1x1 transparent png, base64 as the wire carries it.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// authToken is the bearer the Auth gate hands out.
const authToken = "fake-token"

// Auth is the fake's sign-in gate: it answers 401 until a bearer token its
// own OAuth flow handed out arrives. Base is the server's own URL, set once
// httptest.NewServer built it; Refuse makes the authorization endpoint send
// the browser back with an error.
type Auth struct {
	Base   atomic.Value
	Refuse atomic.Bool
}

func (a *Auth) base() string { b, _ := a.Base.Load().(string); return b }

// Handler serves the gated MCP endpoint and the OAuth discovery,
// registration, authorization and token endpoints around it.
func (a *Auth) Handler() http.Handler {
	mcp := Handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+authToken {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.base()+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "sign in", http.StatusUnauthorized)
			return
		}
		mcp.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"resource": a.base() + "/mcp", "authorization_servers": []string{a.base() + "/auth"}}, json.Deterministic(true))
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"authorization_endpoint": a.base() + "/auth/authorize",
			"token_endpoint":         a.base() + "/auth/token", "registration_endpoint": a.base() + "/auth/register",
		}, json.Deterministic(true))
	})
	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"client_id": "fake"}, json.Deterministic(true))
	})
	mux.HandleFunc("/auth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back := url.Values{"state": {q.Get("state")}, "code": {"abc"}}
		if a.Refuse.Load() {
			back = url.Values{"state": {q.Get("state")}, "error": {"access_denied"}, "error_description": {"the fake said no"}}
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "abc" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"access_token": authToken, "expires_in": 3600}, json.Deterministic(true))
	})
	return mux
}

// ServeStdio answers newline-delimited messages until in closes.
func ServeStdio(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if resp := Respond(sc.Bytes()); resp != nil {
			out.Write(append(resp, '\n'))
		}
	}
}

// Handler serves streamable HTTP: initialize hands out a session that
// later requests must send, and tools/list answers as an SSE stream.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if m.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "fake-session" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		resp := Respond(body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "fake-session")
		}
		if m.Method == "tools/list" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: message\ndata: " + string(resp) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})
}
