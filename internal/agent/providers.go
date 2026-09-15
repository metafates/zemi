// Package agent is the M1 chat core (DESIGN §6.3): provider registry
// (anthropic, openai, openaicompat), model catalog and the turn runner
// that streams fantasy agent output into the UI and persists messages.
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"

	anthropic "charm.land/fantasy/providers/anthropic"
	openai "charm.land/fantasy/providers/openai"
	openaicompat "charm.land/fantasy/providers/openaicompat"
)

// Model is one entry of a provider's catalog.
type Model struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

// Spec describes one provider: kind selects the fantasy implementation,
// api_key_env names the environment variable resolved in Go, never in
// Lua (DESIGN §6.3).
type Spec struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // anthropic | openai | openaicompat
	BaseURL  string  `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Models   []Model `json:"models"`
}

// Registry holds provider specs; ids are unique.
type Registry struct {
	specs []Spec
}

// DefaultRegistry ships the anthropic and openai providers with small
// starter catalogs; model ids follow provider/model in callers.
func DefaultRegistry() *Registry {
	return &Registry{
		specs: []Spec{
			{
				ID: "anthropic", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY",
				Models: []Model{
					{ID: "claude-sonnet-4-5", Display: "Claude Sonnet 4.5"},
					{ID: "claude-haiku-4-5", Display: "Claude Haiku 4.5"},
				},
			},
			{
				ID: "openai", Kind: "openai", APIKeyEnv: "OPENAI_API_KEY",
				Models: []Model{
					{ID: "gpt-5", Display: "GPT-5"},
					{ID: "gpt-5-mini", Display: "GPT-5 mini"},
				},
			},
		},
	}
}

// Register adds a provider spec; a duplicate id is an error. Requires a
// user-owned registration origin (the Lua boundary enforces it, DESIGN
// §6.3).
func (r *Registry) Register(spec Spec) error {
	if spec.ID == "" || spec.Kind == "" {
		return fmt.Errorf("provider needs id and kind")
	}
	for _, s := range r.specs {
		if s.ID == spec.ID {
			return fmt.Errorf("provider %q already registered", spec.ID)
		}
	}
	r.specs = append(r.specs, spec)
	return nil
}

// List returns all provider specs.
func (r *Registry) List() []Spec {
	out := make([]Spec, len(r.specs))
	copy(out, r.specs)
	return out
}

// Lookup finds a spec by provider id.
func (r *Registry) Lookup(providerID string) (Spec, bool) {
	for _, s := range r.specs {
		if s.ID == providerID {
			return s, true
		}
	}
	return Spec{}, false
}

// apiKeys reports which providers have an api key available (for the
// picker to grey out unusable providers without leaking values).
func (r *Registry) apiKeys(spec Spec) string {
	env := spec.APIKeyEnv
	switch spec.Kind {
	case "anthropic":
		env = cmpOr(spec.APIKeyEnv, "ANTHROPIC_API_KEY")
	case "openai", "openaicompat":
		env = cmpOr(spec.APIKeyEnv, "OPENAI_API_KEY")
	}
	if env == "" {
		return ""
	}
	return os.Getenv(env)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Resolve turns "provider/model" into a fantasy language model. The api
// key is read from the named env var here; missing keys surface as an
// error at send time.
func (r *Registry) Resolve(modelID string) (fantasy.LanguageModel, error) {
	providerID, model, ok := strings.Cut(modelID, "/")
	if !ok {
		return nil, fmt.Errorf("model id must be provider/model, got %q", modelID)
	}
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}
	switch spec.Kind {
	case "anthropic":
		opts := []anthropic.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKeys(spec); k != "" {
			opts = append(opts, anthropic.WithAPIKey(k))
		}
		p, err := anthropic.New(opts...)
		if err != nil {
			return nil, err
		}
		return p.LanguageModel(context.Background(), model)
	case "openai":
		opts := []openai.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKeys(spec); k != "" {
			opts = append(opts, openai.WithAPIKey(k))
		}
		p, err := openai.New(opts...)
		if err != nil {
			return nil, err
		}
		return p.LanguageModel(context.Background(), model)
	case "openaicompat":
		opts := []openaicompat.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, openaicompat.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKeys(spec); k != "" {
			opts = append(opts, openaicompat.WithAPIKey(k))
		}
		p, err := openaicompat.New(opts...)
		if err != nil {
			return nil, err
		}
		return p.LanguageModel(context.Background(), model)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", spec.Kind)
	}
}
