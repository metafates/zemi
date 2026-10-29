// model.go resolves models and reads their catalog entries: image support,
// prices, context window and output cap.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/metafates/zemi/internal/store"
)

// resolveModel builds the model for a resolved id (tests stub modelFn).
func (h *Host) resolveModel(id string) (fantasy.LanguageModel, error) {
	if h.Inert {
		return nil, errors.New("an inert boot calls no model")
	}
	if h.modelFn != nil {
		return h.modelFn()
	}
	if err := h.modelErr(id); err != nil {
		return nil, err
	}
	return h.Providers.Resolve(agent.ModelID(id))
}

// ErrNoModel is the send failure when config registered no model.
var ErrNoModel = errors.New("no model yet")

// resolveTurnModel is resolveModel with one keychain attempt between the
// failure and the give-up: a send, a summary build or a detached run racing
// the stored-key pass fills the key instead of failing. Never
// classify: it resolves on the Lua actor, where nothing waits on a
// keychain subprocess.
func (h *Host) resolveTurnModel(id string) (fantasy.LanguageModel, error) {
	model, err := h.resolveModel(id)
	if err == nil {
		return model, nil
	}
	prov := agent.ModelID(id).Provider()
	_, registered := h.Providers.Lookup(prov)
	if registered && h.Providers.HasKey(prov) {
		return model, err // the failure is not a missing key
	}
	if !registered && !h.missingProvider(prov) {
		return model, err // an id nobody registered never gains a stored key
	}
	if !h.fillStored(prov, false) {
		return model, err
	}
	h.keysChanged(prov)
	return h.resolveModel(id)
}

func (h *Host) initPath() string { return perm.TildePath(filepath.Join(h.Dirs.ConfigHome, "init.lua")) }

func (h *Host) addProvider() string { return "add a provider to " + h.initPath() }

// NoModelHint is the one sentence that says how to get a model: the missing
// builtin keys to export (read at start), or the paste route the model picker
// offers. It fits the splash tip's two rows; past three names the generic
// noun, so the hint never becomes a wall.
func (h *Host) NoModelHint() string {
	if missing := h.Providers.Missing(); len(missing) > 0 {
		names := "a provider's API key"
		if len(missing) <= 3 {
			envs := make([]string, len(missing))
			for i, m := range missing {
				envs[i] = m.Env
			}
			names = strings.Join(envs, " or ")
		}
		return "export " + names + " and start zemi again, or press enter on a provider in /model to paste its key"
	}
	return h.addProvider()
}

// modelErr says why id cannot run a turn, before a provider is built: an SDK
// would otherwise fail with its own words ("run ant auth login"). /health
// and a failed send share it.
func (h *Host) modelErr(id string) error {
	if err := h.modelRunErr(id); err != nil {
		return err
	}
	if _, _, decisions := h.Providers.Decisions(agent.ModelID(id)); decisions {
		return fmt.Errorf("%s is a decisions model: zemi.classify.ask judges with it, and no turn runs on one", id)
	}
	return nil
}

// modelRunErr says why no call reaches id at all: nothing named, a provider
// nobody registered, or the key one needs. A decisions model passes here: it
// is what a classify call runs on.
func (h *Host) modelRunErr(id string) error {
	if id == "" {
		hint := h.NoModelHint()
		if len(h.Providers.Missing()) > 1 && h.hooks() != nil {
			hint += "; /model lists every key"
		}
		return fmt.Errorf("%w: %s", ErrNoModel, hint)
	}
	prov := agent.ModelID(id).Provider()
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		// the id may come from a role, -m or a slot, so none is named
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

// generate runs one tool-less, single-step call (a summary build, the goal
// check, zemi.model.ask) on ref, a role or a model id, and bills it to sid. The spec's body fields
// ride it (a gateway's routing, service_tier), the reasoning effort not: a
// short job need not think. It streams: Anthropic's SDK refuses a large
// max_tokens otherwise, and the idle timer covers only a stream. A turn's
// job passes retry, so the working rule counts down its provider retries.
func (h *Host) generate(ctx context.Context, sid, system, prompt string, maxOut *int64, retry func(error, time.Duration), ref string) (*fantasy.AgentResult, error) {
	id := h.modelRef(ref)
	model, err := h.resolveTurnModel(id)
	if err != nil {
		return nil, err
	}
	call := fantasy.AgentStreamCall{
		Messages:        []fantasy.Message{agent.UserMessage(prompt)},
		MaxOutputTokens: maxOut,
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		ProviderOptions: h.Providers.CallOptions(agent.ModelID(id), "", sid),
		Headers:         h.Providers.SessionHeaders(agent.ModelID(id), sid),
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

// generateObject runs one tool-less object call: the schema is handed to the
// provider, which is left to make the model fill it (its own ladder: native
// JSON, a forced tool call, the schema in the prompt), and fantasy decodes the
// reply against it, so nothing here parses text. The call is billed to sid like
// any other, including one whose reply did not fit the schema.
func (h *Host) generateObject(ctx context.Context, sid, system, prompt string, schema fantasy.Schema, maxOut *int64, temp *float64, ref string) (any, string, error) {
	id := h.modelRef(ref)
	model, err := h.resolveTurnModel(id)
	if err != nil {
		return nil, "", err
	}
	call := fantasy.ObjectCall{
		Prompt:          objectPrompt(system, prompt),
		Schema:          schema,
		MaxOutputTokens: maxOut,
		Temperature:     temp,
		ProviderOptions: h.Providers.CallOptions(agent.ModelID(id), "", sid),
		Headers:         h.Providers.SessionHeaders(agent.ModelID(id), sid),
	}
	res, err := model.GenerateObject(ctx, call)
	if err != nil {
		// a reply that arrived and did not fit the schema is the caller's to see
		none, ok := errors.AsType[*fantasy.NoObjectGeneratedError](err)
		if !ok {
			return nil, "", err
		}
		h.bill(sid, id, none.Usage)
		return nil, none.RawText, err
	}
	h.bill(sid, id, res.Usage)
	return res.Object, res.RawText, nil
}

// objectPrompt is a system line when the caller wrote one, then the prompt.
func objectPrompt(system, prompt string) fantasy.Prompt {
	if system == "" {
		return fantasy.Prompt{agent.UserMessage(prompt)}
	}
	return fantasy.Prompt{fantasy.NewSystemMessage(system), agent.UserMessage(prompt)}
}

// checkSchema reads a Lua schema table, which holds a JSON schema in the shape
// fantasy.Schema has: an object asks for properties, a scalar for a bare type.
// A field zemi does not know and a type that is not one are refused here, so a
// typo never travels to the provider as a schema that means something else.
func checkSchema(L *glua.LState, tb *glua.LTable) fantasy.Schema {
	raw, err := lua.ToGo(tb)
	if err != nil {
		L.RaiseError("zemi.model.ask: schema: %v", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		L.RaiseError("zemi.model.ask: schema: %v", err)
	}
	var s fantasy.Schema
	if err := json.Unmarshal(data, &s, json.RejectUnknownMembers(true)); err != nil {
		L.RaiseError("zemi.model.ask: schema: %v", err)
	}
	switch s.Type {
	case "object", "array", "string", "number", "integer", "boolean":
	default:
		L.RaiseError("zemi.model.ask: schema type %q must be object, array, string, number, integer or boolean", s.Type)
	}
	return s
}

// objectFromText is a test stub's stand-in for an object call, whose reply is
// the JSON a provider would have decoded.
func objectFromText(raw string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// modelAsk is zemi.model.ask: one tool-less call on a role's model or an
// id, billed to the live session and run off the interpreter. It costs
// money, and a plugin's manifest `uses` naming `model` is what consents to its
// call. A spec with a schema asks for an object instead: the
// provider decodes the reply into the schema's shape and the call returns
// that value, with the raw reply beside it.
func (h *Host) modelAsk(L *glua.LState) int {
	spec := L.CheckTable(1)
	prompt := rawString(L, spec, "prompt")
	if prompt == "" {
		L.RaiseError("zemi.model.ask: prompt is required")
	}
	ref := cmp.Or(rawString(L, spec, "model"), defaultRole)
	id := h.modelRef(ref)
	system := rawString(L, spec, "system")
	var schema *fantasy.Schema
	if tb, ok := opt[*glua.LTable](L, spec, "schema"); ok {
		s := checkSchema(L, tb)
		schema = &s
	}
	stub := h.gateAsk(L, luaModelAsk, id, func() map[string]any {
		return map[string]any{"model": ref, "prompt": prompt, "system": system}
	})
	maxOut := int64(1024)
	if n, ok := opt[glua.LNumber](L, spec, "max_output"); ok && n > 0 {
		maxOut = int64(n)
	}
	onDone, _ := opt[*glua.LFunction](L, spec, "on_done")
	sid, at := h.AgentSessionID(), h.callerOf(L)
	var (
		text, raw string
		obj       any
		askErr    error
	)
	ask := func(ctx context.Context) error {
		if schema != nil {
			obj, raw, askErr = h.generateObject(ctx, sid, system, prompt, *schema, &maxOut, nil, id)
			return askErr
		}
		res, err := h.generate(ctx, sid, system, prompt, &maxOut, nil, id)
		if err == nil {
			text = concatText(res.Response.Content)
		}
		return err
	}
	if stub != nil {
		// a test zemi's stub stands in for the provider
		ask = func(context.Context) error {
			reply, _ := stub.fields["text"].(string)
			raw, askErr = reply, nil
			if schema != nil {
				obj, askErr = objectFromText(reply)
			} else {
				text = reply
			}
			return errors.Join(askErr, stub.err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.asyncTask(L, asyncTaskSpec{
		subject: "zemi.model.ask", waitAPI: "model:wait", guard: "model:",
		ctx: ctx, cancel: cancel,
		// the idle timer already ends a stalled stream; this bounds a slow one
		timeout: 10 * time.Minute,
		stop:    cancel, withCancel: true, res: &askErr,
		body: func(ctx context.Context) {
			err := ask(ctx)
			askErr = err
			switch {
			case onDone == nil:
			case err != nil:
				h.callLuaAsync("zemi.model.ask on_done", at, onDone, nil, err.Error())
			case schema != nil:
				h.callLuaAsync("zemi.model.ask on_done", at, onDone, obj, raw)
			default:
				h.callLuaAsync("zemi.model.ask on_done", at, onDone, text)
			}
		},
		ret: func(L *glua.LState, err error) []glua.LValue {
			if err != nil { // a timeout or a cancel: the call still runs, nothing it holds is settled
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			// askErr and the reply are set only once done has closed
			switch err = askErr; {
			case err != nil && raw != "": // the reply it did not fit is the caller's to see
				return []glua.LValue{glua.LNil, glua.LString(err.Error() + "; the reply was: " + clipLine(raw, 200))}
			case err != nil:
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			case schema != nil:
				return []glua.LValue{lua.FromGo(L, obj), glua.LString(raw)}
			}
			return []glua.LValue{glua.LString(text)}
		},
	})
	return 1
}

// CompactNow forces one compaction batch off the UI goroutine: the merge
// blocks on the builder, and the view it lands may need node builds first,
// so it reports what this batch saved rather than a final size.
func (h *Host) CompactNow() {
	h.GoSafe("compact", func() {
		saved := h.chat.tree.Compact()
		if saved < 0 {
			return
		}
		if saved == 0 {
			h.Notify(slog.LevelInfo, "Compacted", "the view is already at its floor")
			return
		}
		h.Notify(slog.LevelInfo, "Compacted", fmtTokens(int64(saved))+" of view merged")
	})
}

// bill adds a model call's usage to session sid, and to the live tally when
// sid is still the open session: a background run may outlive it. A call on
// the "main" anchor is the conversation's own, builder bursts included, so
// it moves the tally too.
func (h *Host) bill(sid, modelID string, u fantasy.Usage) {
	cost := h.turnCost(modelID, u)
	h.Log.Debug("model call", "origin", "model", "model", modelID, "session", sid,
		"in", billedInput(u), "out", u.OutputTokens, "cache_read", u.CacheReadTokens, "cost", cost)
	ag := h.agents
	ag.mu.Lock()
	if sid == store.MainSessionID {
		ag.sessCost += cost
		ag.sessIn += billedInput(u)
		ag.sessOut += u.OutputTokens
		ag.cacheRead, ag.cacheBilled = ag.cacheRead+u.CacheReadTokens, ag.cacheBilled+billedInput(u)
	}
	ag.mu.Unlock()
	if h.DB != nil {
		err := h.DB.AddUsage(sid, billedInput(u), u.OutputTokens, u.CacheReadTokens, billedInput(u), cost)
		// the day buckets date the anchor row's accounting; a run's own bill
		// is its parent's already counted, never a second one
		if sid == store.MainSessionID {
			err = cmp.Or(err, h.DB.AddUsageDay(billedInput(u), u.OutputTokens, cost))
		}
		if err != nil {
			h.Log.Warn("usage not logged", "origin", "model", "session", sid, "error", err)
		}
	}
}

// roles are the kinds of model work config names a model for, as
// models.<name>, in the order /model roles lists them. default is the root:
// the conversation and every role left unset run on it.
var roles = []modelRole{
	{defaultRole, "the conversation, and every role left unset"},
	{agentsRole, "subagents"},
	{compactRole, "summaries"},
}

const (
	// defaultRole is the conversation's own slot, the root every role left
	// unset follows.
	defaultRole = "default"
	agentsRole  = "agents"  // subagents with no model of their own
	compactRole = "compact" // the summary builder
)

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

// modelTarget is a model slot config registered: the name /model picks by and
// the model it was registered with. A slot a plugin added is a model to run a
// job on, not a conversation to switch to.
type modelTarget struct {
	Name   string
	Desc   string
	Def    string
	Origin lua.Origin
	Path   string
	// Classify marks a slot zemi.model.add made for classification: /model
	// offers it the decisions models beside the chat ones, and nothing else
	// gets them, since a turn cannot run on one.
	Classify bool
}

// def is the model the target was registered with.
func (t *modelTarget) def() string {
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
// zemi.model.add added. default, the conversation's own slot, is not one.
func (h *Host) registeredTargets() []*modelTarget {
	h.agents.targetsMu.RLock()
	defer h.agents.targetsMu.RUnlock()
	names := slices.Sorted(maps.Keys(h.agents.targets))
	out := make([]*modelTarget, 0, len(names))
	for _, n := range names {
		if n == defaultRole {
			continue
		}
		out = append(out, h.agents.targets[n])
	}
	return out
}

// targetNames is every slot /model sets by name: the builtin roles, then what
// config registered, sorted. default is not one: it is the conversation's own
// slot, the root every other follows.
func (h *Host) targetNames() []string {
	names := roleNames()
	for _, t := range h.registeredTargets() {
		names = append(names, t.Name)
	}
	return names
}

// isTarget reports whether name names a model slot. default is one now: it is
// the root every slot follows.
func (h *Host) isTarget(name string) bool {
	if name == "" {
		return false
	}
	if isRole(name) {
		return true
	}
	_, ok := h.target(name)
	return ok
}

// roleModel is the model id a slot runs on: its key, the model it was
// registered with, models.default, then the first usable provider's default
// (nothing is written to config, so a reload cannot fight the fallback).
// default is the root: its key, then the provider default. "" when nothing
// can run.
func (h *Host) roleModel(name string) string {
	if name == defaultRole {
		return cmp.Or(h.Settings.Str(config.KeyModelsDefault), h.Providers.Default())
	}
	if v := h.Settings.Str(config.ModelKey(name)); v != "" {
		return v
	}
	return cmp.Or(h.modelRef(h.targetDef(name)), h.Settings.Str(config.KeyModelsDefault), h.Providers.Default())
}

// modelRef resolves a model reference, a target name or a provider/model id. A
// builtin role name stays a name: it is resolved here on every read, so a slot
// registered with model = "compact" follows models.compact set or picked
// later.
func (h *Host) modelRef(ref string) string {
	if h.isTarget(ref) {
		return h.roleModel(ref)
	}
	return ref
}

// addTarget registers a model slot: the name rules, then models.<name>. A pick
// kept for that key is applied by the registration, which is the first thing
// that knows the key.
func (h *Host) addTarget(L *glua.LState, name string, t modelTarget) error {
	by, path := h.RT.CurrentOrigin(L), h.RT.CurrentPath(L)
	if err := h.checkTargetName(name, by, path); err != nil {
		return err
	}
	if err := checkTargetModel(t.def()); err != nil {
		return err
	}
	t.Name, t.Origin, t.Path = name, by, path
	h.agents.targetsMu.Lock()
	h.agents.targets[name] = &t
	h.agents.targetsMu.Unlock()
	return h.addKey(config.Setting{Path: config.ModelKey(name), Kind: config.KindString, Default: "", Check: config.ModelRefCheck, Desc: t.Desc})
}

// delTarget drops a slot zemi.model.add registered, with its key and any pick
// kept for it; false when no slot has that name.
func (h *Host) delTarget(L *glua.LState, name string) (bool, error) {
	by, path := h.RT.CurrentOrigin(L), h.RT.CurrentPath(L)
	t, ok := h.target(name)
	if !ok {
		return false, nil
	}
	if err := mayReplace(h.PluginOf, "model target "+name, t.Origin, t.Path, by, path); err != nil {
		return false, err
	}
	h.agents.targetsMu.Lock()
	delete(h.agents.targets, name)
	h.agents.targetsMu.Unlock()
	h.Settings.Del(config.ModelKey(name))
	h.cfgMu.Lock()
	delete(h.pluginKeys, config.ModelKey(name))
	delete(h.cfgNow, config.ModelKey(name))
	h.forgetPick(config.ModelKey(name))
	h.cfgMu.Unlock()
	return true, nil
}

// targetName is a slot's name: its key is models.<name>, so it is a lowercase
// word, dashes for readability like an agent file's name.
var targetName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// checkTargetName refuses a slot name: a builtin role is reserved, and so is
// a name another origin holds.
func (h *Host) checkTargetName(name string, by lua.Origin, byPath string) error {
	switch {
	case !targetName.MatchString(name):
		return fmt.Errorf("name %q must be a lowercase letter, then up to 31 lowercase letters, digits, _ and -", name)
	case isRole(name):
		return fmt.Errorf("%q is a builtin role", name)
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

// takesAudio reports whether a model accepts audio parts; unknown models
// are assumed not to, since the catalog must say so.
func (h *Host) takesAudio(id string) bool {
	m, ok := h.modelSpec(id)
	return ok && m.Audio
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
	prov, model, ok := agent.ModelID(id).Split()
	if !ok {
		return agent.Model{}, false
	}
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		return agent.Model{}, false
	}
	return spec.Model(model)
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

// modelPriced reports whether the model declares prices: what the cost
// widget shows.
func (h *Host) modelPriced(id string) bool {
	m, _ := h.modelSpec(id)
	return m.CostPer1MIn > 0 || m.CostPer1MOut > 0
}

// contextWindow is the model spec's window, 0 when it names none (unknown:
// no percent readout, a subagent trims only after the provider refuses it).
func (h *Host) contextWindow(modelID string) int64 {
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
	prov := agent.ModelID(modelID).Provider()
	if spec, ok := h.Providers.Lookup(prov); ok && spec.Kind == agent.KindAnthropic {
		return 32_000
	}
	return 0
}

func (h *Host) providerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, err := providerSpecFromLua(L, 1)
		if err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
		}
		origin := h.RT.CurrentOrigin(L) == lua.OriginPlugin
		if err := h.Providers.Register(spec, rawString(L, L.CheckTable(1), "api_key"), origin); err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
		}
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LBool(h.Providers.Delete(L.CheckString(1))))
		return 1
	}))
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

// modelNumericKeys are a model entry's flat numeric fields: a quoted one,
// which Lua makes easy, is refused rather than read as 0.
var modelNumericKeys = []string{"context_window", "max_output", "max_attachments"}

// pricingKeys are a model entry's pricing table, USD per million tokens.
var pricingKeys = []string{"input", "output", "cache_write", "cache_read"}

// modelAPIs are the wire formats a model entry may name, catwalk's three.
var modelAPIs = []string{"completions", "responses", "messages"}

// modelCapabilities reads a model entry's capabilities table: the booleans
// catwalk's schema carries, vision and audio today. Vision is assumed on and
// audio off where the entry omits them, like the flat field it replaces was.
func modelCapabilities(e map[string]any, at string) (map[string]bool, error) {
	caps := map[string]bool{"vision": true, "audio": false}
	v, ok := e["capabilities"]
	if !ok || v == nil {
		return caps, nil
	}
	tb, isTable := v.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("%s.capabilities must be a table of booleans", at)
	}
	if err := onlyKeys(tb, at+".capabilities", "vision", "audio"); err != nil {
		return nil, err
	}
	for _, k := range []string{"vision", "audio"} {
		switch b := tb[k].(type) {
		case nil:
		case bool:
			caps[k] = b
		default:
			return nil, fmt.Errorf("%s.capabilities.%s must be a boolean, not a %T", at, k, tb[k])
		}
	}
	return caps, nil
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
	if err := onlyKeys(fields, "spec", "id", "kind", "api_key", "base_url", "headers", "body", "models", "object_mode", "rate_limit"); err != nil {
		return agent.Spec{}, err
	}
	spec := agent.Spec{
		ID:      rawString(L, tb, "id"),
		Kind:    agent.Kind(rawString(L, tb, "kind")),
		BaseURL: rawString(L, tb, "base_url"),
		Headers: rawStringMap(L, tb, "headers"),
	}
	rate, err := parseRateLimit(fields, "rate_limit")
	if err != nil {
		return agent.Spec{}, err
	}
	spec.RateLimit = rate
	switch mode := fantasy.ObjectMode(rawString(L, tb, "object_mode")); mode {
	case "":
	case fantasy.ObjectModeAuto, fantasy.ObjectModeJSON, fantasy.ObjectModeTool, fantasy.ObjectModeText:
		if !slices.Contains(agent.KindsWithObjectMode, spec.Kind) {
			return agent.Spec{}, fmt.Errorf("object_mode is taken only by the %s kinds", strings.Join(strs(agent.KindsWithObjectMode), ", "))
		}
		spec.ObjectMode = mode
	default:
		return agent.Spec{}, fmt.Errorf("object_mode %q: use %s", mode, strings.Join(strs(agent.ObjectModes), ", "))
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
	if !ok && fields["models"] != nil {
		if m, isMap := fields["models"].(map[string]any); !isMap || len(m) > 0 {
			return agent.Spec{}, errors.New("models must be a list of model entries")
		}
	}
	for i, v := range models {
		at := fmt.Sprintf("models[%d]", i+1)
		e, ok := v.(map[string]any)
		if !ok {
			return agent.Spec{}, fmt.Errorf("%s must be a table", at)
		}
		if err := onlyKeys(e, at, append([]string{"id", "display", "capabilities", "decisions", "rate_limit", "api", "pricing"}, modelNumericKeys...)...); err != nil {
			return agent.Spec{}, err
		}
		// the model's capabilities, catwalk's shape: a table of booleans,
		// vision assumed on and audio off where the entry omits them
		caps, err := modelCapabilities(e, at)
		if err != nil {
			return agent.Spec{}, err
		}
		decided, err := modelBool(e, at, "decisions")
		if err != nil {
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
		// the pricing table, catwalk's shape: four numbers that travel
		// together
		cost := map[string]float64{}
		if v, ok := e["pricing"]; ok && v != nil {
			pt, ok := v.(map[string]any)
			if !ok {
				return agent.Spec{}, fmt.Errorf("%s.pricing must be a table of numbers", at)
			}
			if err := onlyKeys(pt, at+".pricing", pricingKeys...); err != nil {
				return agent.Spec{}, err
			}
			for _, k := range pricingKeys {
				switch v := pt[k].(type) {
				case nil:
				case float64:
					cost[k] = max(v, 0)
				default:
					return agent.Spec{}, fmt.Errorf("%s.pricing.%s must be a number, not a %T", at, k, v)
				}
			}
		}
		api, ok := e["api"].(string)
		if e["api"] != nil && !ok {
			return agent.Spec{}, fmt.Errorf("%s.api must be a string, not a %T", at, e["api"])
		}
		if api != "" && !slices.Contains(modelAPIs, api) {
			return agent.Spec{}, fmt.Errorf("%s.api %q: use %s", at, api, strings.Join(modelAPIs, ", "))
		}
		id, ok := e["id"].(string)
		if !ok || id == "" {
			return agent.Spec{}, fmt.Errorf("%s.id is required", at)
		}
		display, _ := e["display"].(string)
		mrate, err := parseRateLimit(e, at+".rate_limit")
		if err != nil {
			return agent.Spec{}, err
		}
		spec.Models = append(spec.Models, agent.Model{
			ID:                  id,
			Display:             display,
			ContextWindow:       int64(nums["context_window"]),
			MaxOutput:           int64(nums["max_output"]),
			CostPer1MIn:         cost["input"],
			CostPer1MOut:        cost["output"],
			CostPer1MCacheWrite: cost["cache_write"],
			CostPer1MCacheRead:  cost["cache_read"],
			TextOnly:            !caps["vision"],
			Audio:               caps["audio"],
			MaxAttachments:      int(nums["max_attachments"]),
			API:                 api,
			Decisions:           decided,
			RateLimit:           mrate,
		})
	}
	switch {
	case decisions(spec) && spec.Kind != agent.KindSystemOne:
		return agent.Spec{}, errors.New("decisions is set only by the SYSTEM_ONE kind: a chat model is never a classifier")
	case spec.Kind == agent.KindSystemOne && !decisions(spec):
		return agent.Spec{}, errors.New("a SYSTEM_ONE provider needs a model with decisions = true; it answers nothing else")
	}
	if spec.ID == "" || spec.Kind == "" {
		return agent.Spec{}, errors.New("provider needs id and kind")
	}
	return spec, nil
}

// parseRateLimit reads a rate_limit field: a number is the requests
// shorthand for a 60s window, a table the full quota.
func parseRateLimit(fields map[string]any, at string) (agent.RateLimit, error) {
	switch v := fields["rate_limit"].(type) {
	case nil:
		return agent.RateLimit{}, nil
	case float64:
		if v < 1 {
			return agent.RateLimit{}, fmt.Errorf("%s must be at least 1 request per minute", at)
		}
		return agent.RateLimit{Requests: int(v), Window: 60}, nil
	case map[string]any:
		rl, err := rateLimitFromLua(v, at)
		if err != nil {
			return agent.RateLimit{}, err
		}
		if rl.Window == 0 {
			rl.Window = 60
		}
		return rl, nil
	default:
		return agent.RateLimit{}, fmt.Errorf("%s must be a number or a table, not a %T", at, v)
	}
}

// rateLimitFromLua reads the rate_limit table's quotas: requests and
// completion_tokens, per window seconds. Either quota alone is a pace.
func rateLimitFromLua(fields map[string]any, at string) (agent.RateLimit, error) {
	if err := onlyKeys(fields, at, "requests", "completion_tokens", "window"); err != nil {
		return agent.RateLimit{}, err
	}
	var rl agent.RateLimit
	for _, k := range []string{"requests", "completion_tokens", "window"} {
		switch v := fields[k].(type) {
		case nil:
		case float64:
			if v < 1 {
				return agent.RateLimit{}, fmt.Errorf("%s.%s must be at least 1", at, k)
			}
			switch k {
			case "requests":
				rl.Requests = int(v)
			case "completion_tokens":
				rl.CompletionTokens = int(v)
			case "window":
				rl.Window = int(v)
			}
		default:
			return agent.RateLimit{}, fmt.Errorf("%s.%s must be a number, not a %T", at, k, v)
		}
	}
	if !rl.Paced() {
		return agent.RateLimit{}, fmt.Errorf("%s needs requests or completion_tokens", at)
	}
	return rl, nil
}

// decisions reports whether a spec carries a decisions model.
func decisions(spec agent.Spec) bool {
	return slices.ContainsFunc(spec.Models, func(m agent.Model) bool { return m.Decisions })
}

// modelBool reads a model entry's boolean field: absent is false, and anything
// but a boolean is refused rather than read as one.
func modelBool(e map[string]any, at, key string) (bool, error) {
	switch v := e[key].(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	default:
		return false, fmt.Errorf("%s.%s must be a boolean, not a %T", at, key, v)
	}
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
		id := L.CheckString(1)
		if err := h.modelSetErr(defaultRole, id); err != nil {
			L.RaiseError("zemi.model.set: %v", err)
		}
		// the default slot: what the conversation and every unset role run on
		path := config.KeyModelsDefault
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

	// add(name, {desc, model}) is a model slot of its own, for code that runs a
	// backend job rather than a conversation of its own: a name /model can pick
	// (models.<name>), with the model it runs until someone picks.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(2)
		raw, err := lua.ToGo(spec)
		if err != nil {
			L.RaiseError("zemi.model.add: spec: %v", err)
		}
		fields, _ := raw.(map[string]any)
		// a typo'd key would register a slot with no model and no complaint
		if err := onlyKeys(fields, "spec", "desc", "model", "classify"); err != nil {
			L.RaiseError("zemi.model.add: %v", err)
		}
		t := modelTarget{
			Desc:     listDesc(rawString(L, spec, "desc")),
			Def:      rawString(L, spec, "model"),
			Classify: rawBool(L, spec, "classify"),
		}
		if t.Desc == "" {
			L.RaiseError("zemi.model.add: desc is what /model roles shows for the slot")
		}
		if err := h.addTarget(L, L.CheckString(1), t); err != nil {
			L.RaiseError("zemi.model.add: %v", err)
		}
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		ok, err := h.delTarget(L, L.CheckString(1))
		if err != nil {
			L.RaiseError("zemi.model.del: %v", err)
		}
		L.Push(glua.LBool(ok))
		return 1
	}))
	tb.RawSetString("pick", L.NewFunction(func(L *glua.LState) int {
		role := L.OptString(1, "")
		if role != "" && !h.isTarget(role) {
			L.RaiseError("zemi.model.pick: unknown model target %q; targets: %s", role, strings.Join(h.targetNames(), ", "))
		}
		_ = h.OpenModelPicker(role)
		return 0
	}))
	return tb
}
