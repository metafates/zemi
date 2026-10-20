package hostapi

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// fakeEndpoint serves one reply to every request and reports the bodies and
// headers it was sent.
type fakeEndpoint struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []string
	head []http.Header
}

func newEndpoint(t *testing.T, reply func(body string) string) *fakeEndpoint {
	t.Helper()
	f := &fakeEndpoint{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, string(raw))
		f.head = append(f.head, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, reply(string(raw)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEndpoint) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// headers is one request's headers, in order.
func (f *fakeEndpoint) headers() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.head...)
}

// chatCompletion is one openai-shaped reply: content, and the token
// probabilities of its first token where top is set.
func chatCompletion(content string, top map[string]float64) string {
	lp := "null"
	if len(top) > 0 {
		var entries []string
		for token, p := range top {
			entries = append(entries, fmt.Sprintf(`{"token":%q,"logprob":%v}`, token, p))
		}
		lp = `{"content":[{"token":` + fmt.Sprintf("%q", content) + `,"logprob":` +
			fmt.Sprintf("%v", top[content]) + `,"top_logprobs":[` + strings.Join(entries, ",") + `]}]}`
	}
	return `{"id":"1","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":` +
		fmt.Sprintf("%q", content) + `},"finish_reason":"stop","logprobs":` + lp + `}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":4}}}`
}

// classifyAskSrc is one classify call: its questions are always the one bool,
// and what it learned lands in the state for the test to read.
const classifyAskSrc = `
	local res, err = zemi.classify.ask({
		model = "clf",
		state = { cmd = "rm -rf build" },
		questions = {
			safe = {
				type = "bool",
				instructions = "is this command safe to run",
				criteria = { ["true"] = "it deletes nothing the user made", ["false"] = "it deletes their work" },
			},
		},
	}):wait()
	if err then
		zemi.state.set("failed", err)
		return
	end
	zemi.state.set("strategy", res.strategy)
	zemi.state.set("value", res.answers.safe.value)
	zemi.state.set("probability", res.answers.safe.probability or -1)
	zemi.state.set("cost", res.usage.cost)
	zemi.state.set("in", res.usage["in"])
`

// TestClassifyAsk: one call per strategy over the Lua surface. Each one answers
// through its own route, reports which strategy it was, and bills the session.
func TestClassifyAsk(t *testing.T) {
	const systemOne = `{"answers": {"safe": {"type": "noul", "noul": 0.97}}, "usage": {"input_tokens": 120, "output_tokens": 6}}`
	for _, c := range []struct {
		name         string
		providers    string // zemi.provider.add lines, with %s the endpoint
		reply        string
		wantStrategy string
		wantValue    string
		wantProb     string
		wantRequests int
		wantInput    int64 // prompt tokens the endpoint reports per request
		wantCache    int64 // of those, the ones it read from its prompt cache
		wantCost     float64
		markers      []string
	}{
		{
			name:         "a decisions model over its own protocol",
			providers:    `zemi.provider.add({ id = "jev", kind = "system_one", base_url = "%s", api_key = "k", models = { { id = "m", decisions = true, cost_per_1m_in = 1, cost_per_1m_out = 2, cost_per_1m_cache_read = 3 } } }) zemi.model.add("clf", { desc = "the classifier", classify = true, model = "jev/m" })`,
			reply:        systemOne,
			wantStrategy: "protocol",
			wantValue:    "true",
			wantProb:     "0.97",
			wantRequests: 1,
			wantInput:    120,
			wantCost:     132e-6, // 120 in at 1 + 6 out at 2; the protocol reports no cache counts
			markers:      []string{`"model":"m"`},
		},
		{
			name:         "a chat model whose provider returns token probabilities",
			providers:    `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "%s", api_key = "k", body = { routing = "cheap" }, models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2, cost_per_1m_cache_read = 3 } } }) zemi.model.add("clf", { desc = "the classifier", classify = true, model = "a/m" })`,
			reply:        chatCompletion("Yes", map[string]float64{"Yes": -0.1, "No": -2.0}),
			wantStrategy: "logprobs",
			wantValue:    "true",
			wantProb:     "0.869",
			wantRequests: 1,
			wantInput:    10,
			wantCache:    4,
			wantCost:     20e-6, // 6 in at 1 + 1 out at 2 + 4 cached at 3
			// the readout rides the provider's own body fields
			markers: []string{`"top_logprobs"`, `"routing":"cheap"`},
		},
		{
			name:         "a chat model that returns none falls through to a schema",
			providers:    `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "%s", api_key = "k", object_mode = "text", models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2, cost_per_1m_cache_read = 3 } } }) zemi.model.add("clf", { desc = "the classifier", classify = true, model = "a/m" })`,
			reply:        chatCompletion(`{"safe":true}`, nil),
			wantStrategy: "schema",
			wantValue:    "true",
			wantProb:     "-1",
			wantRequests: 4, // the readout at each depth, then the object call
			wantInput:    10,
			wantCache:    4,
			wantCost:     20e-6, // the price above, per request
			markers:      []string{"safe"},
		},
		{
			// vercel surfaces no token probabilities, so the readout is not
			// worth asking for: one call answers it
			name:         "a kind that surfaces none goes straight to a schema",
			providers:    `zemi.provider.add({ id = "a", kind = "vercel", base_url = "%s", api_key = "k", object_mode = "text", models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2, cost_per_1m_cache_read = 3 } } }) zemi.model.add("clf", { desc = "the classifier", classify = true, model = "a/m" })`,
			reply:        chatCompletion(`{"safe":true}`, nil),
			wantStrategy: "schema",
			wantValue:    "true",
			wantProb:     "-1",
			wantRequests: 1,
			wantInput:    10,
			wantCache:    4,
			wantCost:     20e-6,
			markers:      []string{"You must respond with valid JSON"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newEndpoint(t, func(string) string { return c.reply })
			h, r := newTestHostDB(t)
			sid := seedSession(t, h)
			runUser(t, r, fmt.Sprintf(c.providers, f.srv.URL))
			runUser(t, r, classifyAskSrc)
			if failed, _ := h.GlobalDB.GetKV("failed"); failed != "" {
				t.Fatalf("the call failed: %s", failed)
			}
			got := func(key string) string {
				v, _ := h.GlobalDB.GetKV(key)
				return strings.Trim(v, `"`)
			}
			if got("strategy") != c.wantStrategy {
				t.Errorf("strategy = %s, want %s", got("strategy"), c.wantStrategy)
			}
			if got("value") != c.wantValue {
				t.Errorf("value = %s, want %s", got("value"), c.wantValue)
			}
			if p := got("probability"); !strings.HasPrefix(p, c.wantProb) {
				t.Errorf("probability = %s, want %s", p, c.wantProb)
			}
			if n := len(f.bodies()); n != c.wantRequests {
				t.Errorf("the endpoint saw %d requests, want %d", n, c.wantRequests)
			}
			for _, body := range f.bodies() {
				for _, marker := range c.markers {
					if !strings.Contains(body, marker) {
						t.Errorf("a request does not carry %s: %s", marker, body)
					}
				}
			}
			// every request of the call carries the session's affinity, so a
			// router keeps the questions of one call on one replica
			for _, head := range f.headers() {
				if got := head.Get("x-session-affinity"); got != sid {
					t.Errorf("x-session-affinity = %q, want %q", got, sid)
				}
			}
			sess, err := h.DB.GetSession(sid)
			// every request the ladder made is billed, abandoned attempts
			// included, and the prompt cache counts reach the totals and the
			// spend
			n := int64(len(f.bodies()))
			if err != nil || sess.PromptTokens != c.wantInput*n || sess.CacheRead != c.wantCache*n {
				t.Errorf("session = %+v, %v; want %d requests, %d cached tokens billed", sess, err, n, c.wantCache*n)
			}
			if want := c.wantCost * float64(n); math.Abs(sess.Cost-want) > 1e-9 {
				t.Errorf("session cost = %v, want %v", sess.Cost, want)
			}
			if v, err := strconv.ParseFloat(got("cost"), 64); err != nil || math.Abs(v-c.wantCost*float64(n)) > 1e-9 {
				t.Errorf("the result's cost = %s, want %v", got("cost"), c.wantCost*float64(n))
			}
		})
	}
}

// TestClassifyAskErrors: a call no strategy answered is an error, and so is a
// question zemi cannot put to a model.
func TestClassifyAskErrors(t *testing.T) {
	t.Parallel()
	f := newEndpoint(t, func(string) string { return "not json" })
	h, r := newTestHostDB(t)
	runUser(t, r, fmt.Sprintf(`zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "%s", api_key = "k", models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2, cost_per_1m_cache_read = 3 } } }) zemi.model.add("clf", { desc = "the classifier", classify = true, model = "a/m" })`, f.srv.URL))
	runUser(t, r, classifyAskSrc)
	if failed, _ := h.GlobalDB.GetKV("failed"); failed == "" {
		t.Fatal("a provider answering nothing but prose answered the call")
	} else if v, _ := h.GlobalDB.GetKV("strategy"); v != "" {
		t.Errorf("strategy = %s, want none", v)
	}
	for _, c := range []struct{ name, src, want string }{
		{"no state", `zemi.classify.ask({ questions = {} })`, "state is the data to judge"},
		{"no questions", `zemi.classify.ask({ state = {} })`, "questions are what to judge"},
		{"no instructions", `zemi.classify.ask({ state = {}, questions = { q = { type = "bool", criteria = { ["true"] = "x", ["false"] = "y" } } } })`, "question q needs instructions"},
		{"no criteria", `zemi.classify.ask({ state = {}, questions = { q = { type = "bool", instructions = "x" } } })`, `question q needs criteria: { ["true"] = "what true means"`},
		{"an unknown type", `zemi.classify.ask({ state = {}, questions = { q = { type = "maybe", instructions = "x", criteria = { a = "b" } } } })`, `question q has type "maybe"`},
		{"an answer that is not true or false", `zemi.classify.ask({ state = {}, questions = { q = { type = "bool", instructions = "x", criteria = { sure = "y" } } } })`, `question q is answered "true" or "false", not "sure"`},
		{"one label", `zemi.classify.ask({ state = {}, questions = { q = { type = "choice", instructions = "x", criteria = { a = "b" } } } })`, "question q: a choice question needs at least 2 labels, got 1"},
	} {
		if err := r.ExecuteChunk(t.Context(), c.src, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
	// a decisions model is no chat model: neither a turn nor /model takes one
	h2, r2 := newTestHost(t)
	runUser(t, r2, `zemi.provider.add({ id = "jev", kind = "system_one", base_url = "http://x", api_key = "k", models = { { id = "m", decisions = true, cost_per_1m_in = 1, cost_per_1m_out = 2 } } })`)
	if err := r2.ExecuteChunk(t.Context(), `zemi.model.set("jev/m")`, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), "is a decisions model") {
		t.Errorf("model.set: err %v, want a refusal", err)
	}
	if _, err := h2.Providers.Resolve("jev/m"); err == nil || !strings.Contains(err.Error(), "is a decisions model") {
		t.Errorf("resolve: err %v, want a refusal", err)
	}
}

// TestClassifyGate: a plugin's own call settles where its manifest declares
// "model", a rule of the user's still decides first, project code may not call
// a model, and the picker offers decisions models to the slots that classify
// alone.
func TestClassifyGate(t *testing.T) {
	t.Parallel()
	h, r := newTestHostPlugin(t, `["model"]`)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `zemi.provider.add({ id = "jev", kind = "system_one", base_url = "http://x", api_key = "k", models = { { id = "m", decisions = true, cost_per_1m_in = 1, cost_per_1m_out = 2 } } })
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", api_key = "k", models = { { id = "big" } } })
		zemi.model.add("clf", { desc = "the classifier", classify = true })`)
	if err := h.OpenModelPicker("clf"); err != nil {
		t.Fatal(err)
	}
	if !pickerHas(ui.opened[0].Items, "jev/m") {
		t.Error("a classifier slot is not offered the decisions model")
	}
	if err := h.OpenModelPicker("titles"); err != nil {
		t.Fatal(err)
	}
	if pickerHas(ui.opened[1].Items, "jev/m") {
		t.Error("a chat role is offered a decisions model")
	}
	if !pickerHas(ui.opened[1].Items, "a/big") {
		t.Error("a chat role lost the chat models")
	}
	// the row the classifier slot was offered is one a pick actually runs:
	// that slot takes the decisions model, a chat role does not
	if err := h.SetModel("clf jev/m"); err != nil {
		t.Errorf("/model clf jev/m: %v", err)
	}
	if got := h.Settings.Str("models.clf"); got != "jev/m" {
		t.Errorf("models.clf = %q, want the decisions model", got)
	}
	if id := h.roleModel("clf"); id != "jev/m" {
		t.Errorf("the classifier slot runs on %q", id)
	}
	if err := h.SetModel("titles jev/m"); err == nil || !strings.Contains(err.Error(), "is a decisions model") {
		t.Errorf("/model titles jev/m: err %v, want a refusal", err)
	}
	if got := h.Settings.Str("models.titles"); got != "" {
		t.Errorf("models.titles = %q after a refused pick", got)
	}

	// the gate: a plugin's declared call settles with no rule (its manifest's
	// `model` is the consent), and project code may not call it at all
	src := `zemi.classify.ask({ state = {}, questions = { q = { type = "bool", instructions = "x", criteria = { ["true"] = "y", ["false"] = "n" } } } })`
	if err := r.ExecuteChunk(t.Context(), src, h.pluginsRoot()+"/local/a/.zemi/init.lua", lua.OriginPlugin); err != nil {
		t.Errorf("a plugin with no rule: %v", err)
	}
	if err := r.ExecuteChunk(t.Context(), src, ".agents/p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "project code may not call a model") {
		t.Errorf("project code: err = %v", err)
	}
}

// TestClassifyProtocolClient: a decisions model's call goes out on the client
// the chat path uses, whose header timeout bounds a gateway that accepts the
// connection and then says nothing.
func TestClassifyProtocolClient(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({ id = "jev", kind = "system_one", base_url = "http://x", api_key = "k", models = { { id = "m", decisions = true } } })`)
	req, err := h.classifyRequest("jev/m", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Protocol.HTTP == nil {
		t.Fatal("the protocol call runs on http.DefaultClient, which waits for headers forever")
	}
	tr, ok := req.Protocol.HTTP.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout == 0 {
		t.Fatalf("the protocol call's client has no header timeout: %+v", req.Protocol.HTTP.Transport)
	}
}

// pickerHas reports whether a model id is one of an overlay's rows.
func pickerHas(items []any, id string) bool {
	return slices.ContainsFunc(items, func(it any) bool {
		row, _ := it.(map[string]any)
		s, _ := row["copy"].(string)
		return s == id
	})
}

// TestClassifyStub: a plugin's own test answers lua.classify.ask, so a
// classifier mode is testable without a provider, the way lua.model.ask is.
func TestClassifyStub(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "local", "a", ".zemi", "tests", "clf.lua")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`local test = zemi.test
test("a stubbed classify stands in for the call", function()
	test.stub("lua.classify.ask", "*", {
		strategy = "logprobs",
		answers = { safe = { value = true, probability = 0.93 } },
	})
	local res, err = zemi.classify.ask({
		state = { cmd = "rm -rf build" },
		questions = { safe = { type = "bool", instructions = "is it safe", criteria = { ["true"] = "y", ["false"] = "n" } } },
	}):wait()
	assert(err == nil, tostring(err))
	assert(res.strategy == "logprobs", res.strategy)
	assert(res.answers.safe.value == true, tostring(res.answers.safe.value))
	assert(res.answers.safe.probability == 0.93, tostring(res.answers.safe.probability))
	test.stub("lua.classify.ask", "*", { error = "the classifier is down" })
	local failed, failErr = zemi.classify.ask({
		state = { cmd = "rm -rf build" },
		questions = { safe = { type = "bool", instructions = "is it safe", criteria = { ["true"] = "y", ["false"] = "n" } } },
	}):wait()
	assert(failed == nil, "a failed classify answers no result")
	assert(tostring(failErr):match("the classifier is down"), tostring(failErr))
end)`), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	h, pt := NewPluginTest(log, lua.PathClasses{{Prefix: root, Origin: lua.OriginPlugin}}, config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()}, file, 0)
	defer h.Close()
	if err := h.RT.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), pt.Step()}); err != nil {
		t.Fatal(err)
	}
	if lines := pt.Run(t.Context(), 10*time.Second); lines != nil {
		t.Fatalf("the plugin test failed: %v", lines)
	}
}

// TestLogprobKinds: only the kinds whose provider reports token probabilities
// take a readout request. The rest go straight to the schema strategy, instead
// of asking for probabilities that never come and re-sending the state at every
// depth.
func TestLogprobKinds(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({ id = "c", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })
		zemi.provider.add({ id = "o", kind = "openai", models = { { id = "m" } } })
		zemi.provider.add({ id = "or", kind = "openrouter", models = { { id = "m" } } })
		zemi.provider.add({ id = "v", kind = "vercel", models = { { id = "m" } } })
		zemi.provider.add({ id = "g", kind = "google", models = { { id = "m" } } })
		zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "m" } } })
		zemi.provider.add({ id = "s", kind = "system_one", base_url = "http://x", models = { { id = "m", decisions = true } } })`)
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"c/m", true},
		{"o/m", true},
		{"or/m", false},
		{"v/m", false},
		{"g/m", false},
		{"a/m", false},
		{"s/m", false},
		// a responses model drops logprobs itself, with a warning
		{"o/gpt-5", false},
	} {
		if got := h.Providers.LogprobOptions(agent.ModelID(c.id)) != nil; got != c.want {
			t.Errorf("%s takes a readout = %v, want %v", c.id, got, c.want)
		}
	}
}
