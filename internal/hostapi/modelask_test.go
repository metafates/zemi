package hostapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
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
		lua.OriginPlugin:  filepath.Join(h.pluginsRoot(), "local", "a", ".zemi", "init.lua"),
		lua.OriginProject: filepath.Join(h.Dirs.ProjectDir, ".agents", "init.lua"),
	}[origin]
	return r.ExecuteChunk(ctx, src, path, origin)
}

// TestModelAsk: zemi.model.ask answers off the interpreter and bills the
// live session; a plugin's own call settles where its manifest declares
// "model", project code may not call it, and a reload cancels the call.
func TestModelAsk(t *testing.T) {
	const ask = `zemi.state.set("ask", zemi.model.ask({ prompt = "ping" }):wait())`
	for _, c := range []struct {
		name   string
		origin lua.Origin
		err    string // what the chunk raises
		reload bool
	}{
		{name: "a wait suspends, the reply is billed", origin: lua.OriginUser},
		{name: "a plugin's call, no rule needed", origin: lua.OriginPlugin},
		{name: "project code", origin: lua.OriginProject, err: "project code may not call a model"},
		{name: "a reload cancels it", origin: lua.OriginUser, reload: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHostPlugin(t, `["model"]`)
			sid := seedSession(t, h)
			m := &askModel{reflectStubModel: &reflectStubModel{}, asked: make(chan struct{}), release: make(chan struct{}), gone: make(chan struct{})}
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
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

// objModel answers an object call with reply, recording every call it saw.
type objModel struct {
	*reflectStubModel
	mu    sync.Mutex
	calls []fantasy.ObjectCall
	reply func(fantasy.ObjectCall) (*fantasy.ObjectResponse, error)
}

func (m *objModel) GenerateObject(_ context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return m.reply(call)
}

// TestModelAskSchema: a spec with a schema returns the decoded value and the
// raw reply, a reply that does not fit the schema comes back as nil with both
// the reply and the error, and a schema zemi cannot read is refused before any
// call.
func TestModelAskSchema(t *testing.T) {
	const objectSchema = `{ type = "object", properties = { allow = { type = "boolean", description = "may it run" } }, required = { "allow" } }`
	for _, c := range []struct {
		name, src, reply string
		object           any
		err              error
		state            map[string]string
	}{
		{
			name:   "an object schema decodes a table",
			src:    `local v, raw, err = zemi.model.ask({ prompt = "p", schema = ` + objectSchema + ` }):wait() assert(err == nil, tostring(err)) zemi.state.set("v", v.allow) zemi.state.set("raw", raw)`,
			object: map[string]any{"allow": true}, reply: `{"allow":true}`,
			state: map[string]string{"v": "true", "raw": `{"allow":true}`},
		},
		{
			name:   "a scalar schema decodes a number",
			src:    `local v, raw, err = zemi.model.ask({ prompt = "p", schema = { type = "integer", minimum = 0, maximum = 9 } }):wait() assert(err == nil, tostring(err)) zemi.state.set("v", v) zemi.state.set("raw", raw)`,
			object: float64(7), reply: "7",
			state: map[string]string{"v": "7", "raw": "7"},
		},
		{
			name:   "an enum keeps the raw reply",
			src:    `local v, raw, err = zemi.model.ask({ prompt = "p", schema = { type = "string", enum = { "allow", "deny" } } }):wait() assert(err == nil, tostring(err)) zemi.state.set("v", v) zemi.state.set("raw", raw)`,
			object: "allow", reply: `"allow"`,
			state: map[string]string{"v": "allow", "raw": `"allow"`},
		},
		{
			name:  "a reply that does not fit the schema keeps it",
			src:   `local v, why = zemi.model.ask({ prompt = "p", schema = { type = "boolean" } }):wait() assert(v == nil and why ~= nil, "want nil, err") assert(why:find("I would say yes", 1, true), why) zemi.state.set("bad", true)`,
			err:   &fantasy.NoObjectGeneratedError{RawText: "I would say yes", Usage: fantasy.Usage{InputTokens: 3, OutputTokens: 4}},
			state: map[string]string{"bad": "true"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			sid := seedSession(t, h)
			m := &objModel{reflectStubModel: &reflectStubModel{}}
			m.reply = func(fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
				if c.err != nil {
					return nil, c.err
				}
				return &fantasy.ObjectResponse{
					Object: c.object, RawText: c.reply, Usage: fantasy.Usage{InputTokens: 3, OutputTokens: 4},
				}, nil
			}
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
			runUser(t, r, c.src)
			if len(m.calls) != 1 {
				t.Fatalf("the model saw %d object calls, want 1", len(m.calls))
			}
			if call := m.calls[0]; call.Schema.Type == "" || call.MaxOutputTokens == nil {
				t.Errorf("the call carried %+v, want the schema and the output cap", call)
			}
			for key, want := range c.state {
				if v, _ := h.GlobalDB.GetKV(key); v != strconv.Quote(want) && v != want {
					t.Errorf("%s = %s, want %q", key, v, want)
				}
			}
			sess, err := h.DB.GetSession(sid)
			if err != nil || sess.PromptTokens != 3 || sess.CompletionTokens != 4 {
				t.Errorf("session = %+v, %v; want the call billed", sess, err)
			}
		})
	}
	// a schema zemi cannot read never reaches a provider
	h, r := newTestHost(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		t.Error("a refused schema called the model")
		return nil, errors.New("no")
	}
	for _, c := range []struct{ name, schema, want string }{
		{"a type that is not one", `{ type = "objectt" }`, `schema type "objectt" must be object, array, string, number, integer or boolean`},
		{"no type", `{ properties = { a = { type = "string" } } }`, `schema type "" must be object`},
		{"a field fantasy has not", `{ type = "object", properties = { a = { kind = "string" } } }`, `schema: json:`},
		{"a schema that is not a table", `"object"`, "schema must be a table"},
	} {
		if err := r.ExecuteChunk(t.Context(), `zemi.model.ask({ prompt = "p", schema = `+c.schema+` })`, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// fakeObject answers a chat-completions request with body and reports the last
// request it saw.
func fakeObject(t *testing.T, body string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = string(raw)
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return seen }
}

// TestModelAskObjectMode: an object ask over a real provider, one case per
// object_mode. The mode a user pinned is the route the request takes, and every
// route ends in the decoded value.
func TestModelAskObjectMode(t *testing.T) {
	t.Parallel()
	const ask = `local v, raw, err = zemi.model.ask({
			prompt = "Is it safe?",
			schema = { type = "object", properties = { allow = { type = "boolean" } }, required = { "allow" } },
		}):wait()
		assert(err == nil, tostring(err))
		assert(v.allow == true, "allow = " .. tostring(v.allow))
		assert(raw ~= "", "no raw reply")
		zemi.state.set("allow", v.allow)`
	const head = `{"id":"1","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":`
	const usage = `,"finish_reason":"%s"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
	jsonReply := head + `{"role":"assistant","content":"{\"allow\":true}"}` + fmt.Sprintf(usage, "stop")
	toolReply := head + `{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"generate_object","arguments":"{\"allow\":true}"}}]}` + fmt.Sprintf(usage, "tool_calls")
	for _, c := range []struct{ mode, reply, marker string }{
		{"auto", jsonReply, `"response_format"`},
		{"json", jsonReply, `"response_format"`},
		{"tool", toolReply, `"tool_choice"`},
		{"text", jsonReply, "You must respond with valid JSON"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			srv, seen := fakeObject(t, c.reply)
			h, r := newTestHostDB(t)
			runUser(t, r, fmt.Sprintf(`zemi.provider.add({ id = "fake", kind = "openai", base_url = %q, api_key = "k", object_mode = %q, models = { { id = "m" } } })`, srv.URL, c.mode))
			runUser(t, r, ask)
			if v, _ := h.GlobalDB.GetKV("allow"); v != "true" {
				t.Errorf("allow = %s, want true", v)
			}
			if body := seen(); !strings.Contains(body, c.marker) {
				t.Errorf("the request does not carry %s: %s", c.marker, body)
			}
		})
	}
}
