// Package mcptest is a fake MCP server for tests, reachable over stdio and
// HTTP: it lists echo, shot and fail over two pages; echo returns its text
// argument, shot is annotated read-only and returns a png, and fail returns
// a tool error.
package mcptest

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
)

// Respond answers one JSON-RPC message; notifications get nil.
func Respond(line []byte) []byte {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
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
		resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake", "version": "1"}}
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
	b, _ := json.Marshal(resp)
	return b
}

// onePixelPNG is a 1x1 transparent png, base64 as the wire carries it.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

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
