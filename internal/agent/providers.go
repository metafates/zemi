// Package agent is the M1 chat core (DESIGN §6.3): a provider registry
// (kinds: anthropic, openai, openaicompat; populated only from user
// config), model catalog and the turn runner that streams fantasy agent
// output into the UI and persists messages.
package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	anthropic "charm.land/fantasy/providers/anthropic"
	openai "charm.land/fantasy/providers/openai"
	openaicompat "charm.land/fantasy/providers/openaicompat"
)

// Model is one entry of a provider's catalog. ContextWindow is optional:
// the token budget the statusline percent readout divides by; models
// without one fall back to agent.context_window config.
type Model struct {
	ID            string  `json:"id"`
	Display       string  `json:"display"`
	ContextWindow int64   `json:"context_window,omitempty"`
	MaxOutput     int64   `json:"max_output,omitempty"` // reply token limit; 0 = zemi's default for the kind
	CostPer1MIn   float64 `json:"cost_per_1m_in,omitempty"`  // USD per million prompt tokens
	CostPer1MOut  float64 `json:"cost_per_1m_out,omitempty"` // USD per million completion tokens
}

// Provider kinds: which fantasy implementation talks to the provider.
const (
	KindAnthropic    = "anthropic"
	KindOpenAI       = "openai"
	KindOpenAICompat = "openai_compat"
)

// Spec describes one provider: kind selects the fantasy implementation.
type Spec struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"` // KindAnthropic, KindOpenAI or KindOpenAICompat
	BaseURL string  `json:"base_url,omitempty"`
	Models  []Model `json:"models"`
}

// Registry holds provider specs; ids are unique. API keys live in a side
// map keyed by provider id and are never exposed through List or Lookup.
type Registry struct {
	specs []Spec
	keys  map[string]string
}

// NewRegistry returns an empty registry. zemi ships no providers: every
// provider, its key and its model catalog come from user config
// (zemi.provider.register); without one the agent fails closed with "no
// models registered".
func NewRegistry() *Registry {
	return &Registry{keys: map[string]string{}}
}

// Register adds a provider spec and its API key; a duplicate id is an
// error. Only user config may register (the Lua boundary enforces it,
// DESIGN §6.3).
func (r *Registry) Register(spec Spec, apiKey string) error {
	if spec.ID == "" || spec.Kind == "" {
		return fmt.Errorf("provider needs id and kind")
	}
	if spec.Kind == "openaicompat" {
		spec.Kind = KindOpenAICompat
	}
	if spec.Kind != KindAnthropic && spec.Kind != KindOpenAI && spec.Kind != KindOpenAICompat {
		return fmt.Errorf("unknown provider kind %q (want %s, %s or %s)", spec.Kind, KindAnthropic, KindOpenAI, KindOpenAICompat)
	}
	for _, s := range r.specs {
		if s.ID == spec.ID {
			return fmt.Errorf("provider %q already registered", spec.ID)
		}
	}
	r.specs = append(r.specs, spec)
	if apiKey != "" {
		r.keys[spec.ID] = apiKey
	}
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

// HasKey reports whether user config gave the provider an API key. Values
// are never exposed.
func (r *Registry) HasKey(providerID string) bool {
	spec, ok := r.Lookup(providerID)
	if !ok {
		return false
	}
	return r.apiKey(spec) != ""
}

func (r *Registry) apiKey(spec Spec) string { return r.keys[spec.ID] }

// Efforts are the reasoning effort levels zemi offers ("" = provider
// default).
var Efforts = []string{"low", "medium", "high"}

// EffortOptions translates a reasoning effort into the provider options of
// the model's provider kind; nil for the provider default or a kind without
// the knob. A model that does not support the option fails its turn with the
// provider's error.
func (r *Registry) EffortOptions(modelID, effort string) fantasy.ProviderOptions {
	if !slices.Contains(Efforts, effort) {
		return nil
	}
	providerID, _, _ := strings.Cut(modelID, "/")
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil
	}
	re := openai.ReasoningEffort(effort)
	switch spec.Kind {
	case KindAnthropic:
		e := anthropic.Effort(effort)
		return anthropic.NewProviderOptions(&anthropic.ProviderOptions{Effort: &e})
	case KindOpenAI:
		return openai.NewProviderOptions(&openai.ProviderOptions{ReasoningEffort: &re})
	case KindOpenAICompat:
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{ReasoningEffort: &re})
	}
	return nil
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
	case KindAnthropic:
		opts := []anthropic.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKey(spec); k != "" {
			opts = append(opts, anthropic.WithAPIKey(k))
		}
		p, err := anthropic.New(opts...)
		if err != nil {
			return nil, err
		}
		return p.LanguageModel(context.Background(), model)
	case KindOpenAI:
		opts := []openai.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKey(spec); k != "" {
			opts = append(opts, openai.WithAPIKey(k))
		}
		p, err := openai.New(opts...)
		if err != nil {
			return nil, err
		}
		return p.LanguageModel(context.Background(), model)
	case KindOpenAICompat:
		opts := []openaicompat.Option{}
		if spec.BaseURL != "" {
			opts = append(opts, openaicompat.WithBaseURL(spec.BaseURL))
		}
		if k := r.apiKey(spec); k != "" {
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
