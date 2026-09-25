// model.go resolves models and reads their catalog entries: image support,
// prices, context window and output cap.
package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
)

// resolveModel picks the agent model for a profile (tests stub modelFn).
func (h *Host) resolveModel(p *Profile) (fantasy.LanguageModel, error) {
	if h.modelFn != nil {
		return h.modelFn()
	}
	id := h.currentModelID(p)
	if err := h.modelErr(id); err != nil {
		return nil, err
	}
	return h.Providers.Resolve(id)
}

// ErrNoModel is the send failure when config registered no model.
var ErrNoModel = errors.New("no model yet")

func (h *Host) initPath() string { return TildePath(filepath.Join(h.Dirs.ConfigHome, "init.lua")) }

func (h *Host) addProvider() string { return "add a provider to " + h.initPath() }

// noModelHint is the one sentence that says how to get a model: the first
// builtin provider's key to export, or where to add a provider.
func (h *Host) noModelHint() string {
	if missing := h.Providers.Missing(); len(missing) > 0 {
		return "export " + missing[0].Env + " or add a provider to init.lua"
	}
	return h.addProvider()
}

// modelErr says why id cannot run, before a provider is built: an SDK
// would otherwise fail with its own words ("run ant auth login"). /health
// and a failed send share it.
func (h *Host) modelErr(id string) error {
	if id == "" {
		hint := h.noModelHint()
		if len(h.Providers.Missing()) > 1 && h.hooks() != nil {
			hint += "; /model lists every key"
		}
		return fmt.Errorf("%w: %s", ErrNoModel, hint)
	}
	prov, _, _ := strings.Cut(id, "/")
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		// the id may come from a role, -m or a profile, so none is named
		missing := h.Providers.Missing()
		if i := slices.IndexFunc(missing, func(m agent.MissingKey) bool { return m.ID == prov }); i >= 0 {
			return fmt.Errorf("builtin provider %q needs its key: export %s", prov, missing[i].Env)
		}
		return fmt.Errorf("provider %q is not registered: %s", prov, h.addProvider())
	}
	// openai_compat may point at a local server that needs none
	if spec.Kind != agent.KindOpenAICompat && !h.Providers.HasKey(prov) {
		return fmt.Errorf("%s: api_key is empty in %s", prov, h.initPath())
	}
	return nil
}

// generate runs one tool-less, single-step call (summaries, titles, the
// reflector, zemi.model.ask) on ref, a role or a model id, and bills it to sid. The spec's body fields
// ride it (a gateway's routing, service_tier), the reasoning effort not: a
// short job need not think. It streams: Anthropic's SDK refuses a large
// max_tokens otherwise, and the idle timer covers only a stream. A turn's
// job passes retry, so the working rule counts down its provider retries.
func (h *Host) generate(ctx context.Context, sid, system, prompt string, maxOut *int64, retry func(error, time.Duration), ref string) (*fantasy.AgentResult, error) {
	id := h.modelRef(ref)
	model, err := h.resolveModel(&Profile{Model: id})
	if err != nil {
		return nil, err
	}
	call := fantasy.AgentStreamCall{
		Messages:        []fantasy.Message{agent.UserMessage(prompt)},
		MaxOutputTokens: maxOut,
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		ProviderOptions: h.Providers.CallOptions(id, ""),
	}
	if retry != nil {
		last := &agent.LastErr{LanguageModel: model}
		model = last
		call.OnRetry = func(_ *fantasy.ProviderError, delay time.Duration) { retry(last.Err, delay) }
		defer func() {
			h.agents.mu.Lock()
			h.agents.retryAt = time.Time{}
			h.agents.mu.Unlock()
		}()
	}
	res, err := fantasy.NewAgent(model, fantasy.WithSystemPrompt(system)).Stream(ctx, call)
	if err == nil {
		h.bill(sid, id, res.TotalUsage)
	}
	return res, err
}

// modelAsk is zemi.model.ask: one tool-less call on a role's model or an
// id, billed to the live session and run off the interpreter. It costs
// money, so plugin code needs a user rule for lua.model.ask and project code
// may not call it.
func (h *Host) modelAsk(L *glua.LState) int {
	spec := L.CheckTable(1)
	prompt := rawString(spec, "prompt")
	if prompt == "" {
		L.RaiseError("zemi.model.ask: prompt is required")
	}
	id := h.modelRef(cmp.Or(rawString(spec, "model"), "chat"))
	switch o := h.RT.CurrentOrigin(L); {
	case o.Rank() <= lua.OriginProject.Rank():
		L.RaiseError("zemi.model.ask: %s code may not call a model", o)
	case o == lua.OriginPlugin:
		h.luaGate(L, "lua.model.ask", id)
	}
	maxOut := int64(1024)
	if n, ok := spec.RawGetString("max_output").(glua.LNumber); ok && n > 0 {
		maxOut = int64(n)
	}
	system := rawString(spec, "system")
	onDone, _ := spec.RawGetString("on_done").(*glua.LFunction)
	sid, at := h.AgentSessionID(), h.callerOf(L)
	var text string
	var askErr error
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	stopID := h.stops.add(cancel) // a reload cancels it, like a job
	h.GoSafe("zemi.model.ask", func() {
		defer h.stops.remove(stopID)
		defer cancel()
		res, err := h.generate(ctx, sid, system, prompt, &maxOut, nil, id)
		if err == nil {
			text = concatText(res.Response.Content)
		}
		askErr = err
		close(done)
		switch {
		case onDone == nil:
		case err != nil:
			h.callLuaAsync(at, onDone, nil, err.Error())
		default:
			h.callLuaAsync(at, onDone, text)
		}
	})
	handle := L.NewTable()
	handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
		// the idle timer already ends a stalled stream; this bounds a slow one
		return waitHandle(L, "model:wait", done, 10*time.Minute, cancel, func(L *glua.LState, err error) []glua.LValue {
			if err == nil { // askErr is set only once done has closed
				err = askErr
			}
			if err != nil {
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			return []glua.LValue{glua.LString(text)}
		})
	})))
	handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
		cancel()
		return 0
	}))
	lua.GuardFuncs(L, handle, "model:", h.guardLua)
	L.Push(handle)
	return 1
}

// bill adds a model call's usage to session sid, and to the live tally when
// sid is still the open session: a background run or the reflector may
// outlive it.
func (h *Host) bill(sid, modelID string, u fantasy.Usage) {
	cost := h.turnCost(modelID, u)
	ag := h.agents
	ag.mu.Lock()
	if ag.sessionID == sid {
		ag.sessCost += cost
		ag.sessIn += billedInput(u)
		ag.sessOut += u.OutputTokens
		ag.cacheRead, ag.cacheBilled = ag.cacheRead+u.CacheReadTokens, ag.cacheBilled+billedInput(u)
	}
	ag.mu.Unlock()
	if sid != "" && h.DB != nil {
		_ = h.DB.AddUsage(sid, billedInput(u), u.OutputTokens, cost)
	}
}

// roles are the kinds of model work config names a model for, as
// models.<name>, in the order /model roles lists them.
var roles = []modelRole{
	{"chat", "the conversation"},
	{"agents", "subagents"},
	{"titles", "session titles"},
	{"compact", "summaries"},
	{"memory", "memory review"},
}

type modelRole struct{ Name, Desc string }

func isRole(name string) bool {
	return slices.ContainsFunc(roles, func(r modelRole) bool { return r.Name == name })
}

func roleNames() []string {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = r.Name
	}
	return names
}

// roleModel is the model id a role runs on: its key, else models.chat, else
// the first usable provider's default (nothing is written to config, so a
// reload cannot fight the fallback). "" when nothing can run.
func (h *Host) roleModel(role string) string {
	return cmp.Or(h.Settings.Str("models."+role), h.Settings.Str("models.chat"), h.Providers.Default())
}

// modelRef resolves a model reference, a role name or a provider/model id.
func (h *Host) modelRef(ref string) string {
	if isRole(ref) {
		return h.roleModel(ref)
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

// imageTooLarge is an imagesRefused refusal of one image's size, not of
// images: the model still takes them.
func imageTooLarge(err error) bool {
	var pe *fantasy.ProviderError
	if !imagesRefused(err) || !errors.As(err, &pe) {
		return false
	}
	s := strings.ToLower(pe.Message)
	return strings.Contains(s, "exceed") || strings.Contains(s, "maximum") || strings.Contains(s, "dimension") || strings.Contains(s, "too large")
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
