// Package agent is the chat core (DESIGN §6.3): a provider registry
// (kinds: anthropic, openai, openaicompat; populated only from user
// config), model catalog and the turn runner that streams fantasy agent
// output into the UI and persists messages.
package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

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
	MaxOutput     int64   `json:"max_output,omitempty"`      // reply token limit; 0 = zemi's default for the kind
	CostPer1MIn   float64 `json:"cost_per_1m_in,omitempty"`  // USD per million prompt tokens
	CostPer1MOut  float64 `json:"cost_per_1m_out,omitempty"` // USD per million completion tokens
	// prompt cache prices, billed apart from the prompt tokens
	CostPer1MInCached  float64 `json:"cost_per_1m_in_cached,omitempty"`  // USD per million tokens written to the cache
	CostPer1MOutCached float64 `json:"cost_per_1m_out_cached,omitempty"` // USD per million tokens read from it
	TextOnly           bool    `json:"text_only,omitempty"`              // the model refuses image parts
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
	// mu guards both: user config registers on the interpreter goroutine
	// while turns, subagents and the UI read.
	mu    sync.RWMutex
	specs []Spec
	keys  map[string]string
}

// NewRegistry returns an empty registry. zemi ships no providers: every
// provider, its key and its model catalog come from user config
// (zemi.provider.add); without one the agent fails closed with "no
// models registered".
func NewRegistry() *Registry {
	return &Registry{keys: map[string]string{}}
}

// Register adds a provider spec and its API key. Registering an id again
// replaces it: config is declarative and every reload replays the file, so
// raising here would abort init.lua on the second run and drop everything
// after it. Only user config may register (the Lua boundary enforces it,
// DESIGN §6.3).
func (r *Registry) Register(spec Spec, apiKey string) error {
	if spec.ID == "" || spec.Kind == "" {
		return errors.New("provider needs id and kind")
	}
	if spec.Kind == "openaicompat" {
		spec.Kind = KindOpenAICompat
	}
	if spec.Kind != KindAnthropic && spec.Kind != KindOpenAI && spec.Kind != KindOpenAICompat {
		return fmt.Errorf("unknown provider kind %q (want %s, %s or %s)", spec.Kind, KindAnthropic, KindOpenAI, KindOpenAICompat)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced := false
	for i, s := range r.specs {
		if s.ID == spec.ID {
			r.specs[i], replaced = spec, true
			break
		}
	}
	if !replaced {
		r.specs = append(r.specs, spec)
	}
	if apiKey != "" {
		r.keys[spec.ID] = apiKey
	} else {
		delete(r.keys, spec.ID)
	}
	return nil
}

// List returns all provider specs.
func (r *Registry) List() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.specs)
}

// Lookup finds a spec by provider id.
func (r *Registry) Lookup(providerID string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	_, ok := r.Lookup(providerID)
	return ok && r.apiKey(providerID) != ""
}

func (r *Registry) apiKey(providerID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys[providerID]
}

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

// Resolve turns "provider/model" into a fantasy language model; a missing
// key surfaces as an error at send time.
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
		return languageModel(spec.BaseURL, r.apiKey(spec.ID), model, anthropic.New, anthropic.WithBaseURL, anthropic.WithAPIKey)
	case KindOpenAI:
		return languageModel(spec.BaseURL, r.apiKey(spec.ID), model, openai.New, openai.WithBaseURL, openai.WithAPIKey)
	case KindOpenAICompat:
		return languageModel(spec.BaseURL, r.apiKey(spec.ID), model, openaicompat.New, openaicompat.WithBaseURL, openaicompat.WithAPIKey)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", spec.Kind)
	}
}

func languageModel[O any](baseURL, key, model string, newProvider func(...O) (fantasy.Provider, error), withBaseURL, withAPIKey func(string) O) (fantasy.LanguageModel, error) {
	var opts []O
	if baseURL != "" {
		opts = append(opts, withBaseURL(baseURL))
	}
	if key != "" {
		opts = append(opts, withAPIKey(key))
	}
	p, err := newProvider(opts...)
	if err != nil {
		return nil, err
	}
	return p.LanguageModel(context.Background(), model)
}
