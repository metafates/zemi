// Package agent is the chat core (DESIGN §6.3): a provider registry
// (kinds: anthropic, openai, openai_compat, google; populated only from user
// config), model catalog and the turn runner that streams fantasy agent
// output into the UI and persists messages.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	anthropic "charm.land/fantasy/providers/anthropic"
	google "charm.land/fantasy/providers/google"
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
	KindGoogle       = "google"
)

// kinds is every kind Resolve knows.
var kinds = []string{KindAnthropic, KindOpenAI, KindOpenAICompat, KindGoogle}

// Spec describes one provider: kind selects the fantasy implementation.
// Headers ride every request (a gateway's attribution or team header); they
// are as secret as the key, so nothing echoes them back.
type Spec struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"` // one of kinds
	BaseURL string            `json:"base_url,omitempty"`
	Headers map[string]string `json:"-"`
	// Body holds extra request-body fields merged into every request
	// (service_tier, a gateway's routing block, a vendor's thinking flag).
	// Only KindsWithBody carry it.
	Body   map[string]any `json:"-"`
	Models []Model        `json:"models"`
}

// KindsWithBody are the kinds whose fantasy options take Spec.Body.
var KindsWithBody = []string{KindAnthropic, KindOpenAICompat}

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
	if !slices.Contains(kinds, spec.Kind) {
		return fmt.Errorf("unknown provider kind %q (want %s)", spec.Kind, strings.Join(kinds, ", "))
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

// CallOptions are the provider options a turn runs with: the reasoning
// effort and the spec's extra body fields, in the model's provider kind;
// nil when neither applies to the kind. A model that does not support an
// option fails its turn with the provider's error.
func (r *Registry) CallOptions(modelID, effort string) fantasy.ProviderOptions {
	providerID, _, _ := strings.Cut(modelID, "/")
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil
	}
	var re *openai.ReasoningEffort
	if slices.Contains(Efforts, effort) {
		e := openai.ReasoningEffort(effort)
		re = &e
	}
	if re == nil && len(spec.Body) == 0 {
		return nil
	}
	switch spec.Kind {
	case KindAnthropic:
		o := &anthropic.ProviderOptions{ExtraBody: spec.Body}
		if re != nil {
			e := anthropic.Effort(effort)
			o.Effort = &e
		}
		return anthropic.NewProviderOptions(o)
	case KindOpenAICompat:
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{ReasoningEffort: re, ExtraBody: spec.Body})
	case KindOpenAI:
		if re != nil {
			return openai.NewProviderOptions(&openai.ProviderOptions{ReasoningEffort: re})
		}
	case KindGoogle:
		if re != nil {
			level := strings.ToUpper(effort)
			return fantasy.ProviderOptions{google.Name: &google.ProviderOptions{ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level}}}
		}
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
	key := r.apiKey(spec.ID)
	switch spec.Kind {
	case KindAnthropic:
		return languageModel(spec, key, model, anthropic.New, anthropic.WithBaseURL, anthropic.WithAPIKey, anthropic.WithHeaders,
			func(c *http.Client) anthropic.Option { return anthropic.WithHTTPClient(c) })
	case KindOpenAI:
		return languageModel(spec, key, model, openai.New, openai.WithBaseURL, openai.WithAPIKey, openai.WithHeaders,
			func(c *http.Client) openai.Option { return openai.WithHTTPClient(c) })
	case KindOpenAICompat:
		return languageModel(spec, key, model, openaicompat.New, openaicompat.WithBaseURL, openaicompat.WithAPIKey, openaicompat.WithHeaders,
			func(c *http.Client) openaicompat.Option { return openaicompat.WithHTTPClient(c) })
	case KindGoogle:
		return languageModel(spec, key, model, google.New, google.WithBaseURL, google.WithGeminiAPIKey, google.WithHeaders,
			func(c *http.Client) google.Option { return google.WithHTTPClient(c) })
	default:
		return nil, fmt.Errorf("unknown provider kind %q", spec.Kind)
	}
}

// modelHTTP bounds the wait for response headers: a gateway that accepts the
// connection and then says nothing would otherwise hold the turn until the
// user interrupts it. Client.Timeout stays zero, so a long stream is never
// cut; a stalled one is idleModel's job.
var modelHTTP = sync.OnceValue(func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: t}
})

func languageModel[O any](spec Spec, key, model string, newProvider func(...O) (fantasy.Provider, error),
	withBaseURL, withAPIKey func(string) O, withHeaders func(map[string]string) O, withClient func(*http.Client) O,
) (fantasy.LanguageModel, error) {
	opts := []O{withClient(modelHTTP())}
	if spec.BaseURL != "" {
		opts = append(opts, withBaseURL(spec.BaseURL))
	}
	if key != "" {
		opts = append(opts, withAPIKey(key))
	}
	if len(spec.Headers) > 0 {
		opts = append(opts, withHeaders(spec.Headers))
	}
	p, err := newProvider(opts...)
	if err != nil {
		return nil, err
	}
	m, err := p.LanguageModel(context.Background(), model)
	if err != nil {
		return nil, err
	}
	return idleModel{LanguageModel: m, idle: modelIdleTimeout}, nil
}

// modelIdleTimeout is how long a model call may make no progress. The
// header timeout only counts until the status line arrives, so a gateway
// that accepts a stream and then stops sending would hold the turn until
// the user interrupts it, and headless nobody can.
// NOTE: one fixed ceiling, not a setting: a model that thinks longer than
// this without sending a part is cut off.
const modelIdleTimeout = 5 * time.Minute

var errModelIdle = fmt.Errorf("the model sent nothing for %s", modelIdleTimeout)

// idleModel bounds a call by idle time, resetting on every stream part, so
// a slow but live answer runs as long as it likes. Tools run between model
// calls, so this never covers a build or a test run.
type idleModel struct {
	fantasy.LanguageModel
	idle time.Duration
}

func (m idleModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.idle, errModelIdle)
	defer cancel()
	return m.LanguageModel.Generate(ctx, call)
}

func (m idleModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(m.idle, func() { cancel(errModelIdle) })
	seq, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		defer timer.Stop()
		defer cancel(nil)
		timedOut := func() bool { return errors.Is(context.Cause(ctx), errModelIdle) }
		sawErr := false
		for part := range seq {
			// the provider reports the cancel as its own error, so say what
			// really happened instead of "context canceled"
			if part.Type == fantasy.StreamPartTypeError && timedOut() {
				part.Error = errModelIdle
			}
			sawErr = sawErr || part.Type == fantasy.StreamPartTypeError
			if !yield(part) {
				return
			}
			timer.Reset(m.idle)
		}
		if !sawErr && timedOut() { // a provider that ends the stream silently
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errModelIdle})
		}
	}, nil
}
