package mcp

import (
	"context"
	"net/http/httptest"
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

// TestStdioExit: a server that dies ends the connection, and pending calls
// return instead of hanging.
func TestStdioExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Start(ctx, Config{Command: "sh", Args: []string{"-c", "exit 3"}}); err == nil {
		t.Fatal("start against a dead server must fail")
	}
}
