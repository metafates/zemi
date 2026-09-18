package agent

import "testing"

func TestEffortOptions(t *testing.T) {
	r := NewRegistry()
	for _, s := range []Spec{{ID: "a", Kind: "anthropic"}, {ID: "o", Kind: "openai"}, {ID: "c", Kind: "openai_compat"}, {ID: "g", Kind: "google"}} {
		if err := r.Register(s, "k"); err != nil {
			t.Fatal(err)
		}
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
		opts := r.EffortOptions(c.model, c.effort)
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
