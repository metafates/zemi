package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
)

func TestCallOptions(t *testing.T) {
	r := NewRegistry()
	r.Reset(Catalog{Specs: []Spec{
		{ID: "b", Kind: KindAnthropic, Builtin: true, Models: []Model{
			{ID: "haiku"},
			{ID: "opus", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Effort: "high"},
			{ID: "pro", Efforts: []string{"low", "high"}, Effort: "high"},
		}},
		{ID: "bo", Kind: KindOpenAI, Builtin: true, Models: []Model{{ID: "gpt-4o"}, {ID: "gpt-5.5", Efforts: []string{"low", "medium", "high"}, Effort: "medium"}}},
		{ID: "bg", Kind: KindGoogle, Builtin: true, Models: []Model{{ID: "gemini-2.5-pro"}, {ID: "lite", Efforts: []string{"minimal", "low"}, Effort: "minimal"}}},
	}})
	for _, s := range []Spec{{ID: "a", Kind: "anthropic"}, {ID: "o", Kind: "openai"}, {ID: "c", Kind: "openai_compat", BaseURL: "http://127.0.0.1:1/v1"}, {ID: "g", Kind: "google"}, {ID: "or", Kind: "openrouter"}, {ID: "v", Kind: "vercel"}} {
		if err := r.Register(s, "k"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(Spec{ID: "bare", Kind: "openai_compat"}, "k"); err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("openai_compat without base_url: %v", err)
	}
	if err := r.Register(Spec{ID: "orp", Kind: "openrouter", BaseURL: "https://proxy.example/v1"}, "k"); err == nil || !strings.Contains(err.Error(), "fixed base_url") {
		t.Fatalf("openrouter with base_url: %v", err)
	}
	for _, c := range []struct {
		model, pick, key, sent string
	}{
		{"a/claude", "high", "anthropic", "high"},
		{"o/gpt", "low", "openai", "low"},
		{"o/gpt", "", "openai", ""}, // the session's prompt_cache_key alone
		{"c/deepseek", "medium", "openai-compat", "medium"},
		{"g/gemini-3-pro", "high", "google", "high"},
		{"or/anthropic/claude-sonnet-4", "high", "openrouter", "high"},
		{"v/anthropic/claude-sonnet-4", "low", "vercel", "low"},
		{"a/claude", "", "", ""},
		{"a/claude", "extreme", "", ""},
		{"missing/x", "high", "", ""},
		{"b/haiku", "high", "", ""},
		{"b/opus", "", "anthropic", "high"},
		{"b/opus", "low", "anthropic", "low"},
		{"b/pro", "medium", "anthropic", "low"},
		{"b/unlisted", "medium", "anthropic", "medium"},
		{"bo/gpt-4o", "high", "openai", ""},
		{"bo/gpt-5.5", "", "openai", "medium"},
		{"bg/gemini-2.5-pro", "high", "", ""},
		{"bg/lite", "", "google", "minimal"},
		{"bg/lite", "high", "google", "low"},
	} {
		opts := r.CallOptions(c.model, r.Effort(c.model, Effort(c.pick)), "s1")
		if c.key == "" {
			if opts != nil {
				t.Fatalf("%s %q: want no options, got %v", c.model, c.pick, opts)
			}
			continue
		}
		if _, ok := opts[c.key]; !ok || len(opts) != 1 {
			t.Fatalf("%s %q: options %v, want key %s", c.model, c.pick, opts, c.key)
		}
		if got := sentEffort(opts[c.key]); got != c.sent {
			t.Fatalf("%s %q: sent %q, want %q", c.model, c.pick, got, c.sent)
		}
		if key := cacheKey(opts[c.key]); (c.key == "openai") != (key == "s1") {
			t.Fatalf("%s %q: prompt_cache_key %q", c.model, c.pick, key)
		}
	}
}

// sentEffort reads the reasoning level out of any kind's options.
func sentEffort(o fantasy.ProviderOptionsData) string {
	var e *string
	switch o := o.(type) {
	case *anthropic.ProviderOptions:
		e = (*string)(o.Effort)
	case *openai.ProviderOptions:
		e = (*string)(o.ReasoningEffort)
	case *openai.ResponsesProviderOptions:
		e = (*string)(o.ReasoningEffort)
	case *openaicompat.ProviderOptions:
		e = (*string)(o.ReasoningEffort)
	case *google.ProviderOptions:
		e = o.ThinkingConfig.ThinkingLevel
	case *openrouter.ProviderOptions:
		e = (*string)(o.Reasoning.Effort)
	case *vercel.ProviderOptions:
		e = (*string)(o.Reasoning.Effort)
	}
	if e == nil {
		return ""
	}
	return strings.ToLower(*e)
}

func cacheKey(o fantasy.ProviderOptionsData) string {
	var k *string
	switch o := o.(type) {
	case *openai.ProviderOptions:
		k = o.PromptCacheKey
	case *openai.ResponsesProviderOptions:
		k = o.PromptCacheKey
	}
	if k == nil {
		return ""
	}
	return *k
}

// TestCallOptionsBody: a spec's extra body fields ride every request, with
// or without a reasoning effort.
func TestCallOptionsBody(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Spec{ID: "c", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", Body: map[string]any{"service_tier": "flex"}}, "k"); err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"", "high"} {
		po, ok := r.CallOptions("c/m", effort, "")["openai-compat"].(*openaicompat.ProviderOptions)
		if !ok || po.ExtraBody["service_tier"] != "flex" {
			t.Fatalf("effort %q: options %+v", effort, po)
		}
		if (po.ReasoningEffort != nil) != (effort != "") {
			t.Fatalf("effort %q: reasoning effort %v", effort, po.ReasoningEffort)
		}
	}
}

// generateStub answers Generate with a fixed usage.
type generateStub struct {
	fantasy.LanguageModel
	usage fantasy.Usage
}

func (s generateStub) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{Usage: s.usage}, nil
}

func (s generateStub) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: s.usage})
	}, nil
}

// TestGoogleUsage: Gemini's prompt count includes its cached tokens and its
// output count leaves out the thinking; a 180k-of-200k cached prompt bills
// 20k input and counts 200k of context plus the whole reply.
func TestGoogleUsage(t *testing.T) {
	raw := fantasy.Usage{InputTokens: 200_000, CacheReadTokens: 180_000, OutputTokens: 100, ReasoningTokens: 50}
	for _, c := range []struct {
		kind          Kind
		input, output int64
	}{{KindGoogle, 20_000, 150}, {KindAnthropic, 200_000, 100}} {
		m := idleModel{LanguageModel: generateStub{usage: raw}, idle: time.Minute, kind: c.kind}
		res, err := m.Generate(t.Context(), fantasy.Call{})
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := m.Stream(t.Context(), fantasy.Call{})
		for part := range seq {
			for _, u := range []fantasy.Usage{res.Usage, part.Usage} {
				if u.InputTokens != c.input || u.OutputTokens != c.output || u.CacheReadTokens != 180_000 {
					t.Errorf("%s: %+v", c.kind, u)
				}
			}
			if c.kind == KindGoogle && ContextUsed(part.Usage) != 200_150 {
				t.Errorf("context used %d", ContextUsed(part.Usage))
			}
		}
	}
}

// TestModelErrorFinal: a refused connection stays what it is to callers
// but is no net.Error to fantasy's retry check, while a reset still is.
func TestModelErrorFinal(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}
	reset := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}
	for _, c := range []struct {
		err   error
		retry bool
	}{{refused, false}, {reset, true}, {&net.DNSError{Name: "x"}, false}} {
		err := ModelError{"p", c.err}
		if _, isNet := errors.AsType[net.Error](err); isNet != c.retry || !errors.Is(err, c.err) {
			t.Errorf("%v: net.Error %v, want %v", c.err, isNet, c.retry)
		}
	}
}

// TestBuiltin: the catalog resolves keys and endpoints through getenv, a
// provider with no exported key is Missing, unsupported types are absent
// and no catalog header is carried.
func TestBuiltin(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-a", "OPENAI_API_KEY": "sk-o", "OPENAI_API_ENDPOINT": "http://gw/v1", "OPENROUTER_API_KEY": "sk-r",
		"AZURE_OPENAI_API_KEY": "az", "AWS_ACCESS_KEY_ID": "aws", "PIONEER_API_KEY": "pi",
	}
	c := Builtin(embedded.GetAll(), func(k string) string { return env[k] })
	byID := map[string]Spec{}
	for _, s := range c.Specs {
		byID[s.ID] = s
		if !s.Builtin || len(s.Headers) > 0 || len(s.Models) == 0 || s.DefaultModel == "" || c.Keys[s.ID] == "" {
			t.Errorf("%s: %+v key %q", s.ID, s, c.Keys[s.ID])
		}
	}
	for _, tc := range []struct {
		id   string
		kind Kind
		base string
	}{
		{"anthropic", KindAnthropic, ""},
		{"openai", KindOpenAI, "http://gw/v1"},
		{"openrouter", KindOpenRouter, "https://openrouter.ai/api/v1"},
		{"deepseek", "", ""},
		{"azure", "", ""},
		{"bedrock", "", ""},
		{"vertexai", "", ""},
		{"copilot", "", ""},
		{"pioneer", "", ""},
	} {
		s, ok := byID[tc.id]
		if ok != (tc.kind != "") || s.Kind != tc.kind || s.BaseURL != tc.base {
			t.Errorf("%s: present %v, %+v", tc.id, ok, s)
		}
	}
	if byID["anthropic"].KeyEnv != "ANTHROPIC_API_KEY" || c.Keys["anthropic"] != "sk-a" || !slices.Contains(c.Missing, MissingKey{"deepseek", "DEEPSEEK_API_KEY"}) || slices.ContainsFunc(c.Missing, func(m MissingKey) bool { return m.ID == "azure" || m.ID == "bedrock" }) {
		t.Errorf("keys %d, missing %v", len(c.Keys), c.Missing)
	}
	if c.Specs[0].ID != "anthropic" || c.Specs[1].ID != "openai" {
		t.Errorf("order: %s, %s", c.Specs[0].ID, c.Specs[1].ID)
	}
	for _, p := range []catwalk.Provider{
		{ID: "bare openai-compat", Type: catwalk.TypeOpenAICompat, APIKey: "$XAI_API_KEY", APIEndpoint: "$GEMINI_API_ENDPOINT"},
		{ID: "unbundled key", Type: catwalk.TypeAnthropic, APIKey: "$GITHUB_TOKEN"},
		{ID: "unbundled endpoint", Type: catwalk.TypeOpenAICompat, APIKey: "$XAI_API_KEY", APIEndpoint: "$GITHUB_TOKEN"},
	} {
		if c := Builtin([]catwalk.Provider{p}, func(k string) string { return map[string]string{"XAI_API_KEY": "k", "GITHUB_TOKEN": "gh"}[k] }); len(c.Specs) != 0 || len(c.Keys) != 0 {
			t.Errorf("%s: %+v", p.ID, c)
		}
	}
}

var soloCatalog = []catwalk.Provider{{ID: "solo", Type: catwalk.TypeAnthropic, APIKey: "$ANTHROPIC_API_KEY", Models: []catwalk.Model{{ID: "m"}}}}

func soloEnv(string) string { return "k" }

// TestLoadCatalog: a saved catalog loads only when it parses and was
// fetched under the bundled version; otherwise the bundled one runs.
func TestLoadCatalog(t *testing.T) {
	fetched := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		file  any // nil: no file
		saved bool
		err   bool
	}{
		{name: "no file"},
		{name: "version mismatch", file: savedCatalog{"v0.0.1", fetched, soloCatalog}},
		{name: "garbage", file: "{", err: true},
		{name: "no supported provider", file: savedCatalog{bundledVersion(), fetched, nil}, err: true},
		{name: "valid", file: savedCatalog{bundledVersion(), fetched, soloCatalog}, saved: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.file != nil {
				data, _ := json.Marshal(c.file, json.Deterministic(true))
				if s, ok := c.file.(string); ok {
					data = []byte(s)
				}
				if err := os.WriteFile(filepath.Join(dir, catalogFile), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cat := LoadCatalog(dir, soloEnv)
			solo := len(cat.Specs) == 1 && cat.Specs[0].ID == "solo"
			if solo != c.saved || cat.Info.Fetched.Equal(fetched) != c.saved || (cat.Info.Err != nil) != c.err {
				t.Errorf("specs %d, info %+v", len(cat.Specs), cat.Info)
			}
		})
	}
}

// TestUpdateCatalog: a fetch saves the catalog for later boots, the next
// one sends its etag and reads a 304 as current, and a feed with no
// provider zemi supports leaves the saved file alone.
func TestUpdateCatalog(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Trim(r.Header.Get("If-None-Match"), `"`) == catwalk.Etag(body) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	for _, c := range []struct {
		name    string
		feed    []catwalk.Provider
		changed bool
		err     bool
	}{
		{name: "fetched", feed: soloCatalog, changed: true},
		{name: "not modified", feed: soloCatalog},
		{name: "no supported provider", feed: []catwalk.Provider{}, err: true},
	} {
		body, _ = json.Marshal(c.feed, jsontext.WithIndent("  "), json.Deterministic(true))
		cat, changed, err := UpdateCatalog(t.Context(), srv.URL, dir, soloEnv)
		if (err != nil) != c.err || changed != c.changed || !c.err && (cat.Info.Providers != 1 || cat.Info.Fetched.IsZero()) {
			t.Fatalf("%s: changed %v, %+v, %v", c.name, changed, cat.Info, err)
		}
		fi, err := os.Stat(filepath.Join(dir, catalogFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: saved file: %v, %v", c.name, fi, err)
		}
		if saved := LoadCatalog(dir, soloEnv); len(saved.Specs) != 1 || saved.Info.Fetched.IsZero() {
			t.Fatalf("%s: saved catalog does not load: %+v", c.name, saved.Info)
		}
	}
}

// TestRegistryBuiltin: Default prefers a user spec over the catalog and
// the spec's default model, Register clears Builtin so DropBuiltin keeps
// the override, Delete and Register drop an id from Missing.
func TestRegistryBuiltin(t *testing.T) {
	r := NewRegistry()
	r.Reset(Catalog{Specs: []Spec{
		{ID: "a", Kind: KindAnthropic, Builtin: true, DefaultModel: "big", Models: []Model{{ID: "big"}}},
		{ID: "o", Kind: KindOpenAI, Builtin: true, Models: []Model{{ID: "first"}}},
	}, Keys: map[string]string{"a": "ka", "o": "ko"}, Missing: []MissingKey{{"x", "X_KEY"}, {"y", "Y_KEY"}}})
	if r.Default() != "a/big" || !r.HasKey("o") {
		t.Fatalf("seed: %s", r.Default())
	}
	r.Delete("a")
	r.Delete("nope")
	r.Delete("x")
	if r.Default() != "o/first" || r.HasKey("a") || !slices.Equal(r.Missing(), []MissingKey{{"y", "Y_KEY"}}) {
		t.Fatalf("after delete: %s %v", r.Default(), r.Missing())
	}
	if err := r.Register(Spec{ID: "empty", Kind: KindAnthropic, BaseURL: "http://gw"}, ""); err != nil {
		t.Fatal(err)
	}
	if r.Default() != "o/first" {
		t.Fatalf("model-less user spec ahead of a builtin: %s", r.Default())
	}
	r.Delete("empty")
	if err := r.Register(Spec{ID: "local", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", Builtin: true, Models: []Model{{ID: "q"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if r.Default() != "local/q" {
		t.Fatalf("user spec after a builtin: %s", r.Default())
	}
	if err := r.Register(Spec{ID: "o", Kind: KindOpenAI, BaseURL: "http://gw/v1", Models: []Model{{ID: "g"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if r.Default() != "o/g" || r.HasKey("o") {
		t.Fatalf("override: %s", r.Default())
	}
	if err := r.Register(Spec{ID: "y", Kind: KindAnthropic}, ""); err != nil {
		t.Fatal(err)
	}
	if len(r.Missing()) != 0 {
		t.Fatalf("user add under a missing builtin still waits for its key: %v", r.Missing())
	}
	r.DropBuiltin()
	ids := []string{}
	for _, s := range r.List() {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"o", "local", "y"}) || r.Default() != "o/g" || len(r.Missing()) != 0 {
		t.Fatalf("after drop: %v %v", ids, r.Missing())
	}
	r.Reset(Catalog{})
	if r.Default() != "" || len(r.List()) != 0 {
		t.Fatal("reset to empty")
	}
}
