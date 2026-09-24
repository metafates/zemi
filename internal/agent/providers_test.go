package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

func TestCallOptions(t *testing.T) {
	r := NewRegistry()
	r.Reset(Catalog{Specs: []Spec{
		{ID: "b", Kind: KindAnthropic, Builtin: true, Models: []Model{{ID: "haiku"},
			{ID: "opus", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Effort: "high"},
			{ID: "pro", Efforts: []string{"low", "high"}, Effort: "high"}}},
		{ID: "bo", Kind: KindOpenAI, Builtin: true, Models: []Model{{ID: "gpt-4o"}, {ID: "gpt-5.5", Efforts: []string{"low", "medium", "high"}, Effort: "medium"}}},
		{ID: "bg", Kind: KindGoogle, Builtin: true, Models: []Model{{ID: "gemini-2.5-pro"}, {ID: "lite", Efforts: []string{"minimal", "low"}, Effort: "minimal"}}},
	}})
	for _, s := range []Spec{{ID: "a", Kind: "anthropic"}, {ID: "o", Kind: "openai"}, {ID: "c", Kind: "openai_compat", BaseURL: "http://127.0.0.1:1/v1"}, {ID: "g", Kind: "google"}} {
		if err := r.Register(s, "k"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(Spec{ID: "bare", Kind: "openai_compat"}, "k"); err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("openai_compat without base_url: %v", err)
	}
	for _, c := range []struct {
		model, pick, key, sent string
	}{
		{"a/claude", "high", "anthropic", "high"},
		{"o/gpt", "low", "openai", "low"},
		{"c/deepseek", "medium", "openai-compat", "medium"},
		{"g/gemini-3-pro", "high", "google", "high"},
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
		opts := r.CallOptions(c.model, r.Effort(c.model, c.pick))
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
	}
	if e == nil {
		return ""
	}
	return strings.ToLower(*e)
}

// TestCallOptionsBody: a spec's extra body fields ride every request, with
// or without a reasoning effort.
func TestCallOptionsBody(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Spec{ID: "c", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", Body: map[string]any{"service_tier": "flex"}}, "k"); err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"", "high"} {
		po, ok := r.CallOptions("c/m", effort)["openai-compat"].(*openaicompat.ProviderOptions)
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
		kind          string
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
	var ne net.Error
	for _, c := range []struct {
		err   error
		retry bool
	}{{refused, false}, {reset, true}, {&net.DNSError{Name: "x"}, false}} {
		err := ModelError{"p", c.err}
		if errors.As(err, &ne) != c.retry || !errors.Is(err, c.err) {
			t.Errorf("%v: net.Error %v, want %v", c.err, errors.As(err, &ne), c.retry)
		}
	}
}

// TestBuiltin: the catalog resolves keys and endpoints through getenv, a
// provider with no exported key is Missing, unsupported types are absent
// and no catalog header is carried.
func TestBuiltin(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-a", "OPENAI_API_KEY": "sk-o", "OPENAI_API_ENDPOINT": "http://gw/v1", "OPENROUTER_API_KEY": "sk-r",
		"AZURE_OPENAI_API_KEY": "az", "AWS_ACCESS_KEY_ID": "aws", "PIONEER_API_KEY": "pi"}
	c := Builtin(func(k string) string { return env[k] })
	byID := map[string]Spec{}
	for _, s := range c.Specs {
		byID[s.ID] = s
		if !s.Builtin || len(s.Headers) > 0 || len(s.Models) == 0 || s.DefaultModel == "" || c.Keys[s.ID] == "" {
			t.Errorf("%s: %+v key %q", s.ID, s, c.Keys[s.ID])
		}
	}
	for _, tc := range []struct{ id, kind, base string }{
		{"anthropic", KindAnthropic, ""},
		{"openai", KindOpenAI, "http://gw/v1"},
		{"openrouter", KindOpenAICompat, "https://openrouter.ai/api/v1"},
		{"deepseek", "", ""}, {"azure", "", ""}, {"bedrock", "", ""}, {"vertexai", "", ""}, {"copilot", "", ""}, {"pioneer", "", ""},
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
	// an openai-compat entry whose endpoint resolves to "" is never a spec
	bare := builtin([]catwalk.Provider{{ID: "bare", Type: catwalk.TypeOpenAICompat, APIKey: "$BARE_KEY", APIEndpoint: "$BARE_URL"}}, func(k string) string { return map[string]string{"BARE_KEY": "k"}[k] })
	if len(bare.Specs) != 0 || len(bare.Keys) != 0 {
		t.Errorf("bare openai-compat: %+v", bare)
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
