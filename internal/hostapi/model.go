// model.go resolves models and reads their catalog entries: image support,
// prices, context window and output cap.

package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// resolveModel picks the agent model for a profile (tests stub modelFn).
func (h *Host) resolveModel(p *Profile) (fantasy.LanguageModel, error) {
	if h.Inert {
		return nil, errors.New("an inert boot calls no model")
	}
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

func (h *Host) initPath() string { return perm.TildePath(filepath.Join(h.Dirs.ConfigHome, "init.lua")) }

func (h *Host) addProvider() string { return "add a provider to " + h.initPath() }

// NoModelHint is the one sentence that says how to get a model: the first
// builtin provider's key to export (read at start), or where to add a provider.
func (h *Host) NoModelHint() string {
	if missing := h.Providers.Missing(); len(missing) > 0 {
		return "export " + missing[0].Env + " and start zemi again, or add a provider to init.lua"
	}
	return h.addProvider()
}

// modelErr says why id cannot run, before a provider is built: an SDK
// would otherwise fail with its own words ("run ant auth login"). /health
// and a failed send share it.
func (h *Host) modelErr(id string) error {
	if id == "" {
		hint := h.NoModelHint()
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
			return fmt.Errorf("builtin provider %q needs its key: export %s and start zemi again", prov, missing[i].Env)
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
		ProviderOptions: h.Providers.CallOptions(id, "", sid),
		Headers:         agent.SessionHeaders(sid),
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
// money, and switching the plugin on is what consents to it; project code
// may not call it.
func (h *Host) modelAsk(L *glua.LState) int {
	spec := L.CheckTable(1)
	prompt := rawString(L, spec, "prompt")
	if prompt == "" {
		L.RaiseError("zemi.model.ask: prompt is required")
	}
	ref := cmp.Or(rawString(L, spec, "model"), "chat")
	id := h.modelRef(ref)
	system := rawString(L, spec, "system")
	var stub *stubReply
	switch o := h.RT.CurrentOrigin(L); {
	case o.Rank() <= lua.OriginProject.Rank():
		L.RaiseError("zemi.model.ask: %s code may not call a model", o)
	case o == lua.OriginPlugin:
		_, stub = h.luaGate(L, "lua.model.ask", id, func() map[string]any { return map[string]any{"model": ref, "prompt": prompt, "system": system} })
	}
	maxOut := int64(1024)
	if n, ok := opt[glua.LNumber](L, spec, "max_output"); ok && n > 0 {
		maxOut = int64(n)
	}
	onDone, _ := opt[*glua.LFunction](L, spec, "on_done")
	sid, at := h.AgentSessionID(), h.callerOf(L)
	ask := func(ctx context.Context) (string, error) {
		res, err := h.generate(ctx, sid, system, prompt, &maxOut, nil, id)
		if err != nil {
			return "", err
		}
		return concatText(res.Response.Content), nil
	}
	if stub != nil {
		// a test zemi's stub stands in for the provider
		ask = func(context.Context) (string, error) {
			text, _ := stub.fields["text"].(string)
			return text, stub.err
		}
	}
	var text string
	var askErr error
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	stopID := h.stops.add(cancel) // a reload cancels it, like a job
	h.GoSafe("zemi.model.ask", func() {
		defer h.stops.remove(stopID)
		defer cancel()
		var err error
		text, err = ask(ctx)
		askErr = err
		close(done)
		switch {
		case onDone == nil:
		case err != nil:
			h.callLuaAsync("zemi.model.ask on_done", at, onDone, nil, err.Error())
		default:
			h.callLuaAsync("zemi.model.ask on_done", at, onDone, text)
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
		_ = h.DB.AddUsage(sid, billedInput(u), u.OutputTokens, u.CacheReadTokens, billedInput(u), cost)
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

// planner is the shipped read-only profile: it is a model slot like a defined
// profile, but nothing registers it, so its shape is synthesized.
const planner = "planner"

// plannerDesc is what /profile and /model roles say the shipped planner is.
const plannerDesc = "read-only: plan and present for approval"

// modelTarget is a model slot config registered: the name /model picks by, the
// model it was registered with, and, for a profile, the shape its name runs as.
// A slot a plugin added has no shape: it is a model to run a job on, not a
// conversation to switch to.
type modelTarget struct {
	Name   string
	Desc   string
	Def    string
	Origin lua.Origin
	Path   string
	Prof   *Profile
}

// def is the model the target was registered with; a profile's is its own.
func (t *modelTarget) def() string {
	if t.Prof != nil {
		return t.Prof.Model
	}
	return t.Def
}

// target finds a registered slot. targetsMu is a leaf: roleModel reads it with
// ag.mu held (liveModelID), so nothing under it takes another lock.
func (h *Host) target(name string) (*modelTarget, bool) {
	h.agents.targetsMu.RLock()
	defer h.agents.targetsMu.RUnlock()
	t, ok := h.agents.targets[name]
	return t, ok
}

// targetDef is the model a slot was registered with, "" for one nothing
// registered.
func (h *Host) targetDef(name string) string {
	t, _ := h.target(name)
	if t == nil {
		return ""
	}
	return t.def()
}

// registeredTargets is what config registered, sorted by name: the slots
// zemi.model.add added first, then the profiles. default, the conversation's
// own slot, is in neither.
func (h *Host) registeredTargets() (plain, profiles []*modelTarget) {
	h.agents.targetsMu.RLock()
	defer h.agents.targetsMu.RUnlock()
	names := slices.Sorted(maps.Keys(h.agents.targets))
	plain = make([]*modelTarget, 0, len(names))
	for _, n := range names {
		t := h.agents.targets[n]
		switch {
		case n == "default":
		case t.Prof != nil:
			profiles = append(profiles, t)
		default:
			plain = append(plain, t)
		}
	}
	return plain, profiles
}

// targetNames is every slot /model sets by name: the builtin roles, the
// shipped planner, then what config registered, sorted. default is not one: it
// is the conversation's own slot, set as chat.
func (h *Host) targetNames() []string {
	plain, profiles := h.registeredTargets()
	names := append(roleNames(), planner)
	for _, t := range append(plain, profiles...) {
		names = append(names, t.Name)
	}
	return names
}

// isTarget reports whether name names a model slot. default is not one: /model
// sets the conversation's slot as chat.
func (h *Host) isTarget(name string) bool {
	if name == "" || name == "default" {
		return false
	}
	if isRole(name) || name == planner {
		return true
	}
	_, ok := h.target(name)
	return ok
}

// roleModel is the model id a slot runs on: its key, the model it was
// registered with, models.chat, then the first usable provider's default
// (nothing is written to config, so a reload cannot fight the fallback).
// default is the conversation's own slot, so models.chat wins over the shape it
// was registered with. "" when nothing can run.
func (h *Host) roleModel(name string) string {
	if name == "default" {
		return cmp.Or(h.Settings.Str("models.chat"), h.modelRef(h.targetDef(name)), h.Providers.Default())
	}
	if v := h.Settings.Str("models." + name); v != "" {
		return v
	}
	return cmp.Or(h.modelRef(h.targetDef(name)), h.Settings.Str("models.chat"), h.Providers.Default())
}

// modelRef resolves a model reference, a target name or a provider/model id. A
// builtin role name stays a name: it is resolved here on every read, so a
// profile registered with model = "titles" follows models.titles set or picked
// later.
func (h *Host) modelRef(ref string) string {
	if h.isTarget(ref) {
		return h.roleModel(ref)
	}
	return ref
}

// refuseTargetOrigin raises for project code: a model slot is registered by
// config or by a plugin that runs a model of its own, not by a project.
func (h *Host) refuseTargetOrigin(L *glua.LState, what string) {
	o := h.RT.CurrentOrigin(L)
	if o == lua.OriginUser || o == lua.OriginBuiltin || o == lua.OriginPlugin {
		return
	}
	L.RaiseError("zemi.%s: project code registers no model target", what)
}

// addTarget registers a model slot: the name rules, then models.<name>. A pick
// kept for that key is applied by the registration, which is the first thing
// that knows the key.
func (h *Host) addTarget(L *glua.LState, name string, t modelTarget) error {
	by, path := h.RT.CurrentOrigin(L), h.RT.CurrentPath(L)
	if err := h.checkTargetName(name, by, path, t.Prof != nil); err != nil {
		return err
	}
	if err := checkTargetModel(t.def()); err != nil {
		return err
	}
	t.Name, t.Origin, t.Path = name, by, path
	h.agents.targetsMu.Lock()
	h.agents.targets[name] = &t
	h.agents.targetsMu.Unlock()
	if name == "default" { // the conversation's own slot: no key of its own
		return nil
	}
	return h.addKey(config.Setting{Path: "models." + name, Kind: config.KindString, Default: "", Desc: targetDesc(name, t)})
}

// delTarget drops a slot zemi.model.add registered, with its key and any pick
// kept for it. A profile is not one: nothing removes a definition.
func (h *Host) delTarget(L *glua.LState, name string) error {
	by, path := h.RT.CurrentOrigin(L), h.RT.CurrentPath(L)
	t, ok := h.target(name)
	switch {
	case !ok:
		return fmt.Errorf("%q is not registered", name)
	case t.Prof != nil:
		return fmt.Errorf("%q is a profile", name)
	}
	if err := mayReplace(h.PluginOf, "model target "+name, t.Origin, t.Path, by, path); err != nil {
		return err
	}
	h.agents.targetsMu.Lock()
	delete(h.agents.targets, name)
	h.agents.targetsMu.Unlock()
	h.Settings.Del("models." + name)
	h.cfgMu.Lock()
	delete(h.pluginKeys, "models."+name)
	delete(h.cfgNow, "models."+name)
	delete(h.picked, "models."+name)
	h.cfgMu.Unlock()
	if h.GlobalDB != nil {
		_, _ = h.GlobalDB.DelKV("config:models." + name)
	}
	return nil
}

// targetName is a slot's name: its key is models.<name>, so it is a lowercase
// word, dashes for readability like an agent file's name.
var targetName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// checkTargetName refuses a slot name: a builtin role is reserved, as are the
// shipped planner and default (a profile may take those two: they are shapes,
// not ids), and so is a name another origin holds.
func (h *Host) checkTargetName(name string, by lua.Origin, byPath string, profile bool) error {
	switch {
	case !targetName.MatchString(name):
		return fmt.Errorf("name %q must be a lowercase letter, then up to 31 lowercase letters, digits, _ and -", name)
	case isRole(name):
		return fmt.Errorf("%q is a builtin role", name)
	case !profile && (name == planner || name == "default"):
		return fmt.Errorf("%q is a shipped profile", name)
	}
	old, ok := h.target(name)
	if !ok {
		return nil
	}
	return mayReplace(h.PluginOf, "model target "+name, old.Origin, old.Path, by, byPath)
}

// checkTargetModel refuses a slot's own model when the chain could not resolve
// it: only a builtin role name stays a live reference, and every other value is
// a provider/model id, so no slot's name nests under another's.
func checkTargetModel(def string) error {
	if def == "" || isRole(def) || strings.Contains(def, "/") {
		return nil
	}
	return fmt.Errorf("model %q must name a builtin role (%s) or a provider/model id", def, strings.Join(roleNames(), ", "))
}

// targetDesc is what /config shows for a slot's key.
func targetDesc(name string, t modelTarget) string {
	if t.Prof != nil {
		return "the model profile " + name + " runs on (provider/model; empty uses the one it was defined with, else models.chat)"
	}
	return t.Desc
}

// takesImages reports whether a model accepts image parts; unknown
// models are assumed to, until the model entry says vision = false or the
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
	pe, ok := errors.AsType[*fantasy.ProviderError](err)
	if !ok || pe.StatusCode != http.StatusBadRequest {
		return false
	}
	s := strings.ToLower(pe.Message)
	return strings.Contains(s, "multimodal") || strings.Contains(s, "image") || strings.Contains(s, "vision")
}

// imageTooLarge is an imagesRefused refusal of one image's size, not of
// images: the model still takes them.
func imageTooLarge(err error) bool {
	pe, ok := errors.AsType[*fantasy.ProviderError](err)
	if !ok || !imagesRefused(err) {
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
		float64(u.CacheCreationTokens)*m.CostPer1MCacheWrite +
		float64(u.CacheReadTokens)*m.CostPer1MCacheRead) / 1e6
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

func (h *Host) providerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.provider.add: only user config may register providers")
		}
		spec, err := providerSpecFromLua(L, 1)
		if err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
		}
		if err := h.Providers.Register(spec, rawString(L, L.CheckTable(1), "api_key")); err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
		}
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.provider.del: only user config may remove providers")
		}
		h.Providers.Delete(L.CheckString(1))
		return 0
	}))
	tb.RawSetString("kind", enumTable(L, agent.Kinds...))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, s := range h.Providers.List() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(s.ID))
			row.RawSetString("kind", glua.LString(s.Kind))
			row.RawSetString("base_url", glua.LString(s.BaseURL))
			row.RawSetString("builtin", glua.LBool(s.Builtin))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	return tb
}

// modelNumericKeys are a model entry's numeric fields: a quoted one, which
// Lua makes easy, is refused rather than read as 0.
var modelNumericKeys = []string{
	"context_window", "max_output", "cost_per_1m_in", "cost_per_1m_out", "cost_per_1m_cache_write", "cost_per_1m_cache_read",
}

func providerSpecFromLua(L *glua.LState, idx int) (agent.Spec, error) {
	tb, ok := L.Get(idx).(*glua.LTable)
	if !ok {
		return agent.Spec{}, errors.New("spec must be a table")
	}
	raw, err := lua.ToGo(tb)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("spec: %w", err)
	}
	fields, _ := raw.(map[string]any)
	if err := onlyKeys(fields, "spec", "id", "kind", "api_key", "base_url", "headers", "body", "models"); err != nil {
		return agent.Spec{}, err
	}
	spec := agent.Spec{
		ID:      rawString(L, tb, "id"),
		Kind:    agent.Kind(rawString(L, tb, "kind")),
		BaseURL: rawString(L, tb, "base_url"),
		Headers: rawStringMap(L, tb, "headers"),
	}
	switch b := fields["body"].(type) {
	case map[string]any:
		if !slices.Contains(agent.KindsWithBody, spec.Kind) {
			return agent.Spec{}, fmt.Errorf("body is sent only by the %s kinds", strings.Join(strs(agent.KindsWithBody), ", "))
		}
		spec.Body = b
	case []any:
		return agent.Spec{}, errors.New("body must be a table of named fields")
	}
	// an empty table marshals as a map
	models, ok := fields["models"].([]any)
	if m, isMap := fields["models"].(map[string]any); !ok && fields["models"] != nil && (!isMap || len(m) > 0) {
		return agent.Spec{}, errors.New("models must be a list of model entries")
	}
	for i, v := range models {
		at := fmt.Sprintf("models[%d]", i+1)
		e, ok := v.(map[string]any)
		if !ok {
			return agent.Spec{}, fmt.Errorf("%s must be a table", at)
		}
		if err := onlyKeys(e, at, append([]string{"id", "display", "vision"}, modelNumericKeys...)...); err != nil {
			return agent.Spec{}, err
		}
		nums := map[string]float64{}
		for _, k := range modelNumericKeys {
			switch v := e[k].(type) {
			case nil:
			case float64:
				nums[k] = max(v, 0)
			default:
				return agent.Spec{}, fmt.Errorf("%s.%s must be a number, not a %T", at, k, v)
			}
		}
		id, _ := e["id"].(string)
		display, _ := e["display"].(string)
		spec.Models = append(spec.Models, agent.Model{
			ID:                  id,
			Display:             display,
			ContextWindow:       int64(nums["context_window"]),
			MaxOutput:           int64(nums["max_output"]),
			CostPer1MIn:         nums["cost_per_1m_in"],
			CostPer1MOut:        nums["cost_per_1m_out"],
			CostPer1MCacheWrite: nums["cost_per_1m_cache_write"],
			CostPer1MCacheRead:  nums["cost_per_1m_cache_read"],
			TextOnly:            e["vision"] == false,
		})
	}
	if spec.ID == "" || spec.Kind == "" {
		return agent.Spec{}, errors.New("provider needs id and kind")
	}
	return spec, nil
}

func (h *Host) modelNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out, names := L.NewTable(), h.targetNames()
		ids := make([]string, len(names))
		for i, n := range names {
			ids[i] = h.roleModel(n)
		}
		for _, s := range h.Providers.List() {
			for _, m := range s.Models {
				id := s.ID + "/" + m.ID
				var on []string
				for i, n := range names {
					if ids[i] == id {
						on = append(on, n)
					}
				}
				row := L.NewTable()
				row.RawSetString("id", glua.LString(id))
				row.RawSetString("display", glua.LString(m.Display))
				row.RawSetString("has_key", glua.LBool(h.Providers.HasKey(s.ID)))
				row.RawSetString("roles", lua.FromGo(L, on))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "model.set")
		id := L.CheckString(1)
		if err := h.modelSetErr(id); err != nil {
			L.RaiseError("zemi.model.set: %v", err)
		}
		// the conversation's slot, so a call under a profile moves what the
		// conversation runs on
		path := "models." + h.ConversationTarget()
		if h.cfgReplayed(path, id) {
			return 0
		}
		fired, err := h.writeSetting(path, id)
		if err != nil {
			L.RaiseError("zemi.model.set: %v", err)
		}
		h.deliverFired(L, fired)
		h.warnUnlisted(id)
		return 0
	}))
	tb.RawSetString("ask", L.NewFunction(h.modelAsk))
	tb.RawSetString("role", enumTable(L, roleNames()...))
	tb.RawSetString("effort", enumTable(L, agent.Efforts...))

	// add(name, {desc, model}) is a model slot of its own, for code that runs a
	// backend job rather than a conversation of its own: a name /model can pick
	// (models.<name>), with the model it runs until someone picks.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		h.refuseTargetOrigin(L, "model.add")
		spec := L.CheckTable(2)
		t := modelTarget{Desc: listDesc(rawString(L, spec, "desc")), Def: rawString(L, spec, "model")}
		if t.Desc == "" {
			L.RaiseError("zemi.model.add: desc is what /model roles shows for the slot")
		}
		if err := h.addTarget(L, L.CheckString(1), t); err != nil {
			L.RaiseError("zemi.model.add: %v", err)
		}
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		h.refuseTargetOrigin(L, "model.del")
		if err := h.delTarget(L, L.CheckString(1)); err != nil {
			L.RaiseError("zemi.model.del: %v", err)
		}
		return 0
	}))
	tb.RawSetString("pick", L.NewFunction(func(L *glua.LState) int {
		role := L.OptString(1, "")
		if role != "" && !h.isTarget(role) {
			L.RaiseError("zemi.model.pick: unknown model target %q; targets: %s", role, strings.Join(h.targetNames(), ", "))
		}
		_ = h.OpenModelPicker(role) // headless there is nothing to pick with
		return 0
	}))
	return tb
}
