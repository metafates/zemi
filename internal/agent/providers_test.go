package agent

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"charm.land/fantasy/providers/openaicompat"
)

func TestCallOptions(t *testing.T) {
	r := NewRegistry()
	for _, s := range []Spec{{ID: "a", Kind: "anthropic"}, {ID: "o", Kind: "openai"}, {ID: "c", Kind: "openai_compat", BaseURL: "http://127.0.0.1:1/v1"}, {ID: "g", Kind: "google"}} {
		if err := r.Register(s, "k"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(Spec{ID: "bare", Kind: "openai_compat"}, "k"); err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("openai_compat without base_url: %v", err)
	}
	for _, c := range []struct {
		model, effort, key string
	}{
		{"a/claude", "high", "anthropic"},
		{"o/gpt", "low", "openai"},
		{"c/deepseek", "medium", "openai-compat"},
		{"g/gemini-3-pro", "high", "google"},
		{"a/claude", "", ""},
		{"a/claude", "extreme", ""},
		{"missing/x", "high", ""},
	} {
		opts := r.CallOptions(c.model, c.effort)
		if c.key == "" {
			if opts != nil {
				t.Fatalf("%s %q: want no options, got %v", c.model, c.effort, opts)
			}
			continue
		}
		if _, ok := opts[c.key]; !ok || len(opts) != 1 {
			t.Fatalf("%s %q: options %v, want key %s", c.model, c.effort, opts, c.key)
		}
	}
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
