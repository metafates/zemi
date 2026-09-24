package hostapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// TestPluginText: what a plugin sends or rewrites reaches the model, but is
// never the user's words for a memory quote, and the session stops being
// local only, so memoryAuto is off; a print run refuses a plugin's send.
func TestPluginText(t *testing.T) {
	const send = `assert(zemi.agent.send("the deploy key lives in vault"))`
	for _, c := range []struct {
		name     string
		origin   lua.Origin
		src      string
		headless bool
		rewrite  bool // run a tool.post after the chunk
		err      string
		quote    bool // the text counts as the user's
	}{
		{name: "the user's send", origin: lua.OriginUser, src: send, quote: true},
		{name: "a plugin's send", origin: lua.OriginPlugin, src: send},
		{name: "a plugin's send in a print run", origin: lua.OriginPlugin, src: send, headless: true, err: "print run"},
		{name: "a plugin's rewrite", origin: lua.OriginPlugin, src: `zemi.event.on("tool.post", function() return { output = "y" } end)`, rewrite: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			reflectStub(t, h, "ok")
			if !c.headless {
				h.SetUIHooks(&fakeUI{})
			}
			h.Perm.Add(perm.Rule{Subject: "lua.agent.send", Pattern: "*", Decision: perm.Allow, Principal: "plugin:a"})
			sid := seedSession(t, h)
			err := chunkAs(h, r, c.origin, c.src)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.rewrite {
				if out := h.toolPost(context.Background(), "read", nil, true, "x", nil, 0); out != "y" {
					t.Fatalf("output = %q", out)
				}
			} else {
				waitFor(t, func() error {
					if !slices.ContainsFunc(h.Session().Transcript, func(m ChatMessage) bool { return m.Text == "ok" }) || h.sessionLocalOnly(sid) != c.quote {
						return fmt.Errorf("the send is not answered and marked yet: %+v", h.Session().Transcript)
					}
					return nil
				})
				waitIdle(t, h)
			}
			if got := h.quoteInSession(sid, "deploy key lives"); got != c.quote {
				t.Errorf("quote counts = %v, want %v", got, c.quote)
			}
			if got := h.sessionLocalOnly(sid); got != c.quote {
				t.Errorf("local only = %v, want %v", got, c.quote)
			}
			notice := slices.ContainsFunc(h.Session().Transcript, func(m ChatMessage) bool {
				return m.Kind == "notice" && m.Text == "plugin:a: the deploy key lives in vault"
			})
			if want := c.origin == lua.OriginPlugin && !c.rewrite; notice != want {
				t.Errorf("plugin notice row %v, want %v: %+v", notice, want, h.Session().Transcript)
			}
		})
	}
}
