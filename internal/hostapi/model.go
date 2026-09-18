// model.go resolves models and reads their catalog entries: image support,
// prices, context window and output cap.
package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
)

// resolveModel picks the agent model for a profile (tests stub modelFn).
func (h *Host) resolveModel(p *Profile) (fantasy.LanguageModel, error) {
	if h.modelFn != nil {
		return h.modelFn()
	}
	id := h.currentModelID(p)
	if id == "" {
		return nil, fmt.Errorf("no model configured in %s: uncomment a provider and models.main", filepath.Join(h.Dirs.ConfigHome, "init.lua"))
	}
	return h.Providers.Resolve(id)
}

// modelFor resolves the model the first set key names, the session's model
// when none is set.
func (h *Host) modelFor(keys ...string) (fantasy.LanguageModel, error) {
	for _, key := range keys {
		if id := h.Settings.Str(key); id != "" && h.modelFn == nil {
			return h.Providers.Resolve(id)
		}
	}
	return h.resolveModel(&Profile{Model: h.sessionModelID()})
}

// Tiers are the model tiers config names, in the order a picker shows them:
// small runs the short jobs (session titles, compaction summaries, the
// builtin web subagent), main everything else, and smart the main
// conversation, which orchestrates and delegates. A tier nothing set falls
// back to main.
var Tiers = []string{"small", "main", "smart"}

// tierModel resolves a tier name to the model id config gave it, falling
// back to main. It returns "" for a name that is not a tier, so callers can
// take a model reference that is either a tier or a provider/model id.
func (h *Host) tierModel(tier string) string {
	if !slices.Contains(Tiers, tier) {
		return ""
	}
	return cmp.Or(h.Settings.Str("models."+tier), h.Settings.Str("models.main"))
}

// modelRef resolves a model reference: a tier name, a provider/model id, or
// "" for neither.
func (h *Host) modelRef(ref string) string {
	if id := h.tierModel(ref); id != "" {
		return id
	}
	if slices.Contains(Tiers, ref) {
		return "" // a tier nothing configured, not a model id
	}
	return ref
}

// takesImages reports whether a model accepts image parts; unknown
// models are assumed to, until the model entry says images = false or the
// provider refused a request over them (imagesRejected).
func (h *Host) takesImages(id string) bool {
	if _, refused := imagesRejected.Load(id); refused {
		return false
	}
	m, ok := h.modelSpec(id)
	return !ok || !m.TextOnly
}

// imagesRejected remembers models whose provider refused a request over
// its image parts, so the turn resends without them and later ones never
// carry any (like capRejected, per process).
var imagesRejected sync.Map

// imagesRefused reports a provider 400 about image input; the wording is
// the provider's, so this is a text match like maxTokensRefused.
func imagesRefused(err error) bool {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe.StatusCode != 400 {
		return false
	}
	s := strings.ToLower(pe.Message)
	return strings.Contains(s, "multimodal") || strings.Contains(s, "image") || strings.Contains(s, "vision")
}

// modelSpec finds the catalog entry for a "provider/model" id.
func (h *Host) modelSpec(id string) (agent.Model, bool) {
	prov, model, ok := strings.Cut(id, "/")
	if !ok {
		return agent.Model{}, false
	}
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		return agent.Model{}, false
	}
	for _, m := range spec.Models {
		if m.ID == model {
			return m, true
		}
	}
	return agent.Model{}, false
}

// turnCost prices one turn's tokens from the model spec; 0 when the spec
// carries no prices. Cache writes and reads are priced apart: providers bill
// them apart and do not count them as prompt tokens.
func (h *Host) turnCost(modelID string, u fantasy.Usage) float64 {
	m, _ := h.modelSpec(modelID)
	return (float64(u.InputTokens)*m.CostPer1MIn +
		float64(u.OutputTokens)*m.CostPer1MOut +
		float64(u.CacheCreationTokens)*m.CostPer1MInCached +
		float64(u.CacheReadTokens)*m.CostPer1MOutCached) / 1e6
}

// contextWindow: agent.context_window, then the model spec, else 0
// (unknown: no percent readout, no auto compaction).
func (h *Host) contextWindow(modelID string) int64 {
	if n := h.Settings.Int("agent.context_window"); n > 0 {
		return int64(n)
	}
	m, _ := h.modelSpec(modelID)
	return m.ContextWindow
}

// capRejected remembers models whose provider refused zemi's default output
// cap: an older Anthropic model with a lower ceiling would otherwise fail
// every turn until the user set max_output by hand.
var capRejected sync.Map

// maxTokensRefused reports a provider error about the output cap zemi set.
// Anthropic's context overflow names max_tokens too; compaction fixes that one.
func maxTokensRefused(err error) bool {
	if err == nil || contextTooLarge(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "max_tokens") || strings.Contains(s, "max_output_tokens")
}

// maxOutput is the reply token limit: the model spec's, else 32000 for
// Anthropic (whose provider otherwise sends 4096 and cuts large writes)
// unless the model refused that cap, else 0 (the provider default).
func (h *Host) maxOutput(modelID string) int64 {
	if m, _ := h.modelSpec(modelID); m.MaxOutput > 0 {
		return m.MaxOutput
	}
	if _, refused := capRejected.Load(modelID); refused {
		return 0
	}
	prov, _, _ := strings.Cut(modelID, "/")
	if spec, ok := h.Providers.Lookup(prov); ok && spec.Kind == agent.KindAnthropic {
		return 32_000
	}
	return 0
}
