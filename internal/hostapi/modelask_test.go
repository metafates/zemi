package hostapi

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// askModel answers "pong" once release is closed; asked closes when the
// first call arrives, gone when one ends by its cancel.
type askModel struct {
	*reflectStubModel
	asked, release, gone chan struct{}
	once                 sync.Once
}

func (m *askModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.once.Do(func() { close(m.asked) })
	select {
	case <-m.release:
	case <-ctx.Done():
		close(m.gone)
		return nil, ctx.Err()
	}
	return streamOf(&fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "pong"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 5, OutputTokens: 7},
	}), nil
}

// chunkAs runs src as code of origin: a plugin's is plugin a's.
func chunkAs(ctx context.Context, h *Host, r *lua.Runtime, origin lua.Origin, src string) error {
	path := map[lua.Origin]string{
		lua.OriginUser:    filepath.Join(h.Dirs.ConfigHome, "init.lua"),
		lua.OriginPlugin:  filepath.Join(h.pluginsRoot(), "local", "a", "init.lua"),
		lua.OriginProject: filepath.Join(h.Dirs.ProjectDir, ".agents", "init.lua"),
	}[origin]
	return r.ExecuteChunk(ctx, src, path, origin)
}

// TestModelAsk: zemi.model.ask answers off the interpreter and bills the
// live session; a plugin's own call settles because the plugin is enabled, a
// deny rule still refuses it, project code may not call it, and a reload
// cancels the call.
func TestModelAsk(t *testing.T) {
	const ask = `zemi.state.set("ask", zemi.model.ask({ prompt = "ping" }):wait())`
	for _, c := range []struct {
		name   string
		origin lua.Origin
		deny   bool   // a user deny rule for plugin a
		err    string // what the chunk raises
		reload bool
	}{
		{name: "a wait suspends, the reply is billed", origin: lua.OriginUser},
		{name: "a plugin's call, no rule needed", origin: lua.OriginPlugin},
		{name: "a plugin's call a deny rule refuses", origin: lua.OriginPlugin, deny: true, err: "permission denied (rule)"},
		{name: "project code", origin: lua.OriginProject, err: "project code may not call a model"},
		{name: "a reload cancels it", origin: lua.OriginUser, reload: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			sid := seedSession(t, h)
			m := &askModel{reflectStubModel: &reflectStubModel{}, asked: make(chan struct{}), release: make(chan struct{}), gone: make(chan struct{})}
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
			if c.deny {
				h.Perm.Add(perm.Rule{Subject: "lua.model.ask", Pattern: "*", Decision: perm.Deny, Principal: "plugin:a"})
			}
			done := make(chan error, 1)
			go func() { done <- chunkAs(t.Context(), h, r, c.origin, ask) }()
			if c.err != "" {
				if err := <-done; err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			<-m.asked
			// the waiting chunk left the interpreter free
			runUser(t, r, `zemi.state.set("free", true)`)
			if c.reload {
				if err := r.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration); err != nil {
					t.Fatal(err)
				}
				<-m.gone
				return
			}
			close(m.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			key := "ask"
			if c.origin == lua.OriginPlugin {
				key = "plugin:a:ask" // a plugin's state is its own
			}
			if v, _ := h.GlobalDB.GetKV(key); v != `"pong"` {
				t.Errorf("reply = %s", v)
			}
			if sess, err := h.DB.GetSession(sid); err != nil || sess.PromptTokens != 5 {
				t.Errorf("session = %+v, %v; want 5 input tokens billed", sess, err)
			}
		})
	}
}
