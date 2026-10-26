// Package agent is the chat core (DESIGN §6.3): a provider registry
// (kinds: anthropic, openai, openai_compat, google, openrouter, vercel,
// system_one; seeded from the builtin catwalk catalog, then user config),
// model catalog and the turn runner that streams fantasy agent output into
// the UI and persists messages.
package agent

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/openai-go/option"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/set"
)

// ModelID is a model id: a provider, a slash, then the model's own name, as
// config and /model write it. It is parsed once, here, so no caller cuts it.
type ModelID string

// Split returns the provider and the model's own name; ok is false for an id
// with no slash, which names no model.
func (id ModelID) Split() (provider, model string, ok bool) {
	return strings.Cut(string(id), "/")
}

// Provider is the provider half, "" when the id carries none.
func (id ModelID) Provider() string { p, _, _ := id.Split(); return p }

// Model is the model half, the whole id when it carries no slash.
func (id ModelID) Model() string { _, m, _ := id.Split(); return m }

// Model is one entry of a provider's catalog. ContextWindow is optional:
// the token budget the context percent and auto compaction divide by.
type Model struct {
	ID            string
	Display       string
	ContextWindow int64
	MaxOutput     int64   // reply token limit; 0 = zemi's default for the kind
	CostPer1MIn   float64 // USD per million prompt tokens
	CostPer1MOut  float64 // USD per million completion tokens
	// prompt cache prices, billed apart from the prompt tokens
	CostPer1MCacheWrite float64 // USD per million tokens written to the cache
	CostPer1MCacheRead  float64 // USD per million tokens read from it
	TextOnly            bool    // the model refuses image parts
	// Decisions marks a model that answers classification through its
	// provider's own protocol (a SYSTEM_ONE one), never a chat turn.
	Decisions bool
	Efforts   []string // a builtin's reasoning levels; empty takes no effort
	Effort    string   // a builtin's level when /effort is unset
}

// Kind is a provider kind: which fantasy implementation talks to the
// provider.
type Kind string

const (
	KindAnthropic    Kind = "anthropic"
	KindOpenAI       Kind = "openai"
	KindOpenAICompat Kind = "openai_compat"
	KindGoogle       Kind = "google"
	KindOpenRouter   Kind = "openrouter"
	KindVercel       Kind = "vercel"
	// KindSystemOne is no fantasy provider: a classifier endpoint zemi POSTs a
	// state and questions to (classify.Protocol).
	KindSystemOne Kind = "system_one"
)

// Kinds is every kind Resolve knows.
var Kinds = []Kind{KindAnthropic, KindOpenAI, KindOpenAICompat, KindGoogle, KindOpenRouter, KindVercel, KindSystemOne}

// Spec describes one provider: kind selects the fantasy implementation.
// Headers ride every request (a gateway's attribution or team header); they
// are as secret as the key, so nothing echoes them back.
type Spec struct {
	ID      string
	Kind    Kind
	BaseURL string
	Headers map[string]string
	// Body holds extra request-body fields merged into every request
	// (service_tier, a gateway's routing block, a vendor's thinking flag).
	// Only KindsWithBody carry it.
	Body map[string]any
	// ObjectMode pins how this provider answers an object call (a schema in,
	// a value out): "" leaves the kind's own ladder (native JSON, a forced
	// tool call, the schema in the prompt). Only KindsWithObjectMode take it.
	ObjectMode fantasy.ObjectMode
	Models     []Model
	// Builtin marks a catalog spec; Register clears it, so a user spec
	// under a builtin id survives DropBuiltin.
	Builtin      bool
	KeyEnv       string // the env var a builtin's key came from, so a rejected key names it
	DefaultModel string // the model Default picks
}

// Model finds this spec's catalog entry for a model id.
func (s Spec) Model(id string) (Model, bool) {
	i := slices.IndexFunc(s.Models, func(m Model) bool { return m.ID == id })
	if i < 0 {
		return Model{}, false
	}
	return s.Models[i], true
}

// Catalog is the builtin providers with their env vars resolved: a
// provider whose key is exported is a Spec with its key in Keys, one
// without is Missing with the spec it would run as, so /health can name
// the variable to export and a pasted key can activate the provider whole.
type Catalog struct {
	Specs   []Spec
	Keys    map[string]string
	Missing []MissingKey
	Info    CatalogInfo
}

// CatalogInfo says which catwalk catalog the builtin providers come from.
type CatalogInfo struct {
	Version           string    // the bundled catwalk module version
	Fetched           time.Time // when the saved catalog was fetched; zero for the bundled one
	Err               error     // why the saved catalog did not load, the bundled one in its place
	Providers, Models int       // what zemi supports of it, keys exported or not
}

func (i CatalogInfo) String() string {
	switch {
	case !i.Fetched.IsZero():
		return "updated " + i.Fetched.Local().Format(time.DateOnly) + " from " + strings.TrimPrefix(CatalogURL, "https://")
	case i.Version == "":
		return "bundled"
	}
	return "bundled (catwalk " + i.Version + ")"
}

// Summary counts what zemi supports of the catalog.
func (i CatalogInfo) Summary() string {
	n := func(k int, what string) string {
		if k != 1 {
			what += "s"
		}
		return strconv.Itoa(k) + " " + what
	}
	return n(i.Providers, "provider") + ", " + n(i.Models, "model")
}

// MissingKey is a builtin provider whose key env var is not exported. Spec
// is what it would run as, so a key pasted into the TUI can activate the
// provider whole.
type MissingKey struct {
	ID, Env string
	Spec    Spec
}

// builtinKinds maps catwalk types to zemi kinds. NOTE: azure, bedrock,
// google-vertex and copilot are left out: they need more than one key
// (endpoint and API version, an AWS credential chain, ADC, OAuth) and a
// fantasy package each. openrouter and vercel get their own kinds: every
// kind reads cached tokens from the usage, but only theirs turn the
// Anthropic cache breakpoints into the cache_control Claude behind them
// needs.
var builtinKinds = map[catwalk.Type]Kind{
	catwalk.TypeAnthropic:    KindAnthropic,
	catwalk.TypeOpenAI:       KindOpenAI,
	catwalk.TypeGoogle:       KindGoogle,
	catwalk.TypeOpenAICompat: KindOpenAICompat,
	catwalk.TypeOpenRouter:   KindOpenRouter,
	catwalk.TypeVercel:       KindVercel,
}

// Builtin converts a catwalk catalog, resolving its "$NAME" key and
// endpoint references through getenv, the only place Go itself reads a key
// env var. The catalog's order is kept (anthropic, openai, gemini first,
// then the rest), which is the order Default prefers. Catalog
// default_headers are dropped: they attribute requests to crush, and the
// only header zemi sends that the user did not write is SessionHeaders'.
// NOTE: model options (temperature, top_p) are not carried.
func Builtin(providers []catwalk.Provider, getenv func(string) string) Catalog {
	env := func(ref string) (name, value string) {
		name, ok := strings.CutPrefix(ref, "$")
		if !ok {
			return "", ref
		}
		return name, getenv(name)
	}
	c := Catalog{Keys: map[string]string{}}
	for _, p := range providers {
		kind, ok := builtinKinds[p.Type]
		if !ok || p.APIKey == "" || !bundledRef(p.APIKey) || !bundledRef(p.APIEndpoint) {
			continue
		}
		// the feed is untrusted (bundledRef skips its key refs too): an id
		// that is not a plain word never reaches a keychain account or a
		// key file
		if !plainID(string(p.ID)) {
			continue
		}
		c.Info.Providers++
		c.Info.Models += len(p.Models)
		name, key := env(p.APIKey)
		_, base := env(p.APIEndpoint)
		// Reset skips Register's guard, and the fantasy default is
		// api.openai.com, which would get the key
		if kind == KindOpenAICompat && base == "" {
			continue
		}
		spec := Spec{
			ID: string(p.ID), Kind: kind, BaseURL: base, Builtin: true, KeyEnv: name,
			DefaultModel: p.DefaultLargeModelID,
		}
		for _, m := range p.Models {
			// catwalk's legacy names: "in cached" is the cache write price,
			// "out cached" the cache read price
			spec.Models = append(spec.Models, Model{
				ID: m.ID, Display: m.Name, ContextWindow: m.ContextWindow, MaxOutput: m.DefaultMaxTokens,
				CostPer1MIn: m.CostPer1MIn, CostPer1MOut: m.CostPer1MOut, CostPer1MCacheWrite: m.CostPer1MInCached, CostPer1MCacheRead: m.CostPer1MOutCached,
				TextOnly: !m.SupportsImages, Efforts: m.ReasoningLevels, Effort: m.DefaultReasoningEffort,
			})
		}
		if key == "" {
			c.Missing = append(c.Missing, MissingKey{ID: string(p.ID), Env: name, Spec: spec})
			continue
		}
		c.Specs = append(c.Specs, spec)
		c.Keys[spec.ID] = key
	}
	return c
}

// plainID says whether id may name a provider: it becomes a keychain
// account, a command argument and a file in the data dir, so it stays a
// plain word.
func plainID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\ `) && !strings.Contains(id, "..") &&
		!strings.ContainsFunc(id, unicode.IsControl)
}

// bundledRefs are the "$NAME" references the bundled catalog makes.
var bundledRefs = sync.OnceValue(func() set.Set[string] {
	refs := set.Set[string]{}
	for _, p := range embedded.GetAll() {
		refs.Add(p.APIKey)
		refs.Add(p.APIEndpoint)
	}
	return refs
})

// bundledRef holds for a literal value and for a variable the bundled
// catalog reads, so a fetched catalog cannot pick which secret zemi sends.
// NOTE: a provider the feed adds with a variable of its own waits for the
// zemi release that bundles it.
func bundledRef(ref string) bool { return !strings.HasPrefix(ref, "$") || bundledRefs().Has(ref) }

// CatalogURL is where UpdateCatalog fetches the catwalk catalog.
const CatalogURL = "https://catwalk.charm.land"

// catalogFile is the fetched catalog, kept in zemi's data dir, not the
// project: it decides where each builtin key is sent.
const catalogFile = "catwalk.json"

type savedCatalog struct {
	Catwalk   string             `json:"catwalk"` // the bundled version it was fetched under
	Fetched   time.Time          `json:"fetched"`
	Providers []catwalk.Provider `json:"providers"`
}

// bundledVersion is the catwalk module this binary embeds, "" when it
// carries no build info.
func bundledVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "charm.land/catwalk" {
				return cmp.Or(d.Replace, d).Version
			}
		}
	}
	return ""
}

// LoadCatalog converts the catalog UpdateCatalog saved in dir, else the
// bundled one. A catalog saved under another bundled version is ignored,
// so a zemi upgrade brings its newer bundle back; one that does not load
// is Info.Err.
func LoadCatalog(dir string, getenv func(string) string) Catalog {
	ps, info := loadCatalog(dir)
	c := Builtin(ps, getenv)
	if c.Info.Providers == 0 && !info.Fetched.IsZero() {
		c = Builtin(embedded.GetAll(), getenv)
		info.Fetched, info.Err = time.Time{}, errors.New("no provider zemi supports")
	}
	c.Info.Version, c.Info.Fetched, c.Info.Err = info.Version, info.Fetched, info.Err
	return c
}

func loadCatalog(dir string) ([]catwalk.Provider, CatalogInfo) {
	info := CatalogInfo{Version: bundledVersion()}
	var s savedCatalog
	data, err := os.ReadFile(filepath.Join(dir, catalogFile))
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		info.Err = err
	case s.Catwalk != info.Version && info.Version != "":
	default:
		info.Fetched = s.Fetched
		return s.Providers, info
	}
	return embedded.GetAll(), info
}

// UpdateCatalog fetches the catwalk catalog from url, saves it in dir for
// later boots and returns it converted; changed is false when the saved
// one was current. A failed update leaves the saved file as it was.
func UpdateCatalog(ctx context.Context, url, dir string, getenv func(string) string) (c Catalog, changed bool, err error) {
	ps, info := loadCatalog(dir)
	// the server tags its indented JSON and the client returns no etag, so
	// the catalog in use is tagged the same way
	body, err := json.Marshal(ps, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return Catalog{}, false, err
	}
	fresh, err := catwalk.NewWithURL(url).GetProviders(ctx, catwalk.Etag(body))
	changed = err == nil
	if errors.Is(err, catwalk.ErrNotModified) {
		fresh, err = ps, nil
	}
	if err != nil {
		return Catalog{}, false, err
	}
	c = Builtin(fresh, getenv)
	if c.Info.Providers == 0 {
		return Catalog{}, false, errors.New("the fetched catalog has no provider zemi supports")
	}
	c.Info.Version, c.Info.Fetched = info.Version, time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(savedCatalog{Catwalk: c.Info.Version, Fetched: c.Info.Fetched, Providers: fresh}, json.Deterministic(true))
	if err != nil {
		return Catalog{}, false, err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, catalogFile), data); err != nil {
		return Catalog{}, false, err
	}
	return c, changed, nil
}

// KindsWithBody are the kinds whose requests carry Spec.Body: the ExtraBody
// CallOptions sets, plus openai, whose Resolve case sets it as SDK JSON.
var KindsWithBody = slices.DeleteFunc(slices.Clone(Kinds), func(k Kind) bool { return !k.sendsBody() })

// ObjectModes are what object_mode takes, in fantasy's words.
var ObjectModes = []fantasy.ObjectMode{fantasy.ObjectModeAuto, fantasy.ObjectModeJSON, fantasy.ObjectModeTool, fantasy.ObjectModeText}

// KindsWithObjectMode are the kinds that take Spec.ObjectMode.
var KindsWithObjectMode = slices.DeleteFunc(slices.Clone(Kinds), func(k Kind) bool { return !k.takesObjectMode() })

// sendsBody says whether this kind's requests carry Spec.Body. The switch has
// no default on purpose: the exhaustive linter makes a new kind state its side,
// since the wiring (CallOptions, Resolve's openai case) cannot be asked.
func (k Kind) sendsBody() bool {
	switch k {
	case KindAnthropic, KindOpenAI, KindOpenAICompat, KindOpenRouter, KindVercel:
		return true
	case KindGoogle, KindSystemOne:
		return false
	}
	return false
}

// takesObjectMode says whether this kind's provider has a WithObjectMode
// option, which is how Resolve passes Spec.ObjectMode on.
func (k Kind) takesObjectMode() bool {
	switch k {
	case KindOpenAI, KindOpenAICompat, KindGoogle, KindOpenRouter, KindVercel:
		return true
	case KindAnthropic, KindSystemOne:
		return false
	}
	return false
}

// Registry holds provider specs; ids are unique. API keys live in a side
// map keyed by provider id and are never exposed through List or Lookup.
// It also keeps the catalog's Missing list, minus what config deleted, so
// a hint never names a provider init.lua turned off.
type Registry struct {
	// mu guards all five: user config registers on the interpreter
	// goroutine while turns, subagents and the UI read.
	mu      sync.RWMutex
	specs   []Spec
	keys    map[string]string
	stored  map[string]bool // id -> the keychain took the paste, else the file did
	plugins map[string]bool // id registered by a plugin: never holds a stored key
	missing []MissingKey
}

// Reset replaces every spec and key with the catalog, so a provider config
// stopped adding disappears on reload like any other registration. NOTE: a
// turn running through a reload sees only the builtins until init.lua
// replays, so a step on a user provider may bill at 0 and read no context
// window, and a failed replay leaves only the builtins until init.lua is fixed.
func (r *Registry) Reset(c Catalog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = slices.Clone(c.Specs)
	r.keys = maps.Clone(c.Keys)
	r.stored = nil
	r.plugins = nil
	r.missing = slices.Clone(c.Missing)
}

// Missing lists the builtin providers waiting for a key that config did
// not delete or drop.
func (r *Registry) Missing() []MissingKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.missing)
}

// Delete removes a provider and its key; an absent id is a no-op, since
// init.lua must survive a machine where that builtin's key is not exported.
// It reports whether the provider was there.
func (r *Registry) Delete(providerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	had := len(r.specs) + len(r.missing)
	r.specs = slices.DeleteFunc(r.specs, func(s Spec) bool { return s.ID == providerID })
	r.missing = slices.DeleteFunc(r.missing, func(m MissingKey) bool { return m.ID == providerID })
	delete(r.keys, providerID)
	delete(r.stored, providerID)
	delete(r.plugins, providerID)
	return len(r.specs)+len(r.missing) < had
}

// DropBuiltin removes every catalog spec user config did not replace, and
// the Missing list with them.
func (r *Registry) DropBuiltin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.missing = nil
	r.specs = slices.DeleteFunc(r.specs, func(s Spec) bool {
		if s.Builtin {
			delete(r.keys, s.ID)
			delete(r.stored, s.ID)
		}
		return s.Builtin
	})
}

// Default is the "provider/model" models.default runs when config names none: the
// default model of the first spec the user added that has one, else of the
// first such builtin (the catalog's order); "" when none has a model. A
// user spec wins because the user typed it, while a builtin's key may be
// exported for other tools.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, builtin := range []bool{false, true} {
		for _, spec := range r.specs {
			if spec.Builtin != builtin {
				continue
			}
			model := spec.DefaultModel
			if model == "" && len(spec.Models) > 0 {
				model = spec.Models[0].ID
			}
			if model != "" {
				return spec.ID + "/" + model
			}
		}
	}
	return ""
}

// Register adds a provider spec and its API key. Registering an id again
// replaces it: config is declarative and every reload replays the file, so
// raising here would abort init.lua on the second run and drop everything
// after it; a builtin id is replaced whole, key included, so a gateway
// base_url works (DESIGN §6.3: any caller may register; the key is never
// exposed back to Lua). fromPlugin marks a plugin's registration: it never
// holds a stored key, whose requests would ride its endpoint unasked.
func (r *Registry) Register(spec Spec, apiKey string, fromPlugin bool) error {
	spec.Builtin = false
	if spec.ID == "" || spec.Kind == "" {
		return errors.New("provider needs id and kind")
	}
	// the id names a keychain account, a command argument and a file in the
	// data dir, so it stays a plain word
	if !plainID(spec.ID) {
		return fmt.Errorf("provider id %q: a plain name, no slashes, spaces or dot runs", spec.ID)
	}
	if !slices.Contains(Kinds, spec.Kind) {
		names := make([]string, len(Kinds))
		for i, k := range Kinds {
			names[i] = string(k)
		}
		return fmt.Errorf("unknown provider kind %q (one of %s)", spec.Kind, strings.Join(names, ", "))
	}
	// the fantasy default is api.openai.com, which would get the key
	if spec.Kind == KindOpenAICompat && spec.BaseURL == "" {
		return errors.New("base_url is required for OPENAI_COMPAT")
	}
	if spec.Kind == KindSystemOne && spec.BaseURL == "" {
		return errors.New("base_url is required for SYSTEM_ONE")
	}
	if spec.Kind == KindOpenRouter && spec.BaseURL != "" {
		return errors.New("OPENROUTER has a fixed base_url; a proxy in front of it is OPENAI_COMPAT")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys = map[string]string{}
	}
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
	r.missing = slices.DeleteFunc(r.missing, func(m MissingKey) bool { return m.ID == spec.ID })
	delete(r.stored, spec.ID)
	if fromPlugin {
		if r.plugins == nil {
			r.plugins = map[string]bool{}
		}
		r.plugins[spec.ID] = true
	} else {
		delete(r.plugins, spec.ID)
	}
	if apiKey != "" {
		r.keys[spec.ID] = apiKey
	} else {
		delete(r.keys, spec.ID)
	}
	return nil
}

// IsPlugin reports whether a plugin registered id's spec.
func (r *Registry) IsPlugin(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[id]
}

// ErrStoredRefused says the provider's key is one config or the environment
// already answered for; the caller that has the spec names which.
// ErrStoredPlugin is its plugin case, worded once here.
var (
	ErrStoredRefused = errors.New("the provider's key is not zemi's to set")
	ErrStoredPlugin  = fmt.Errorf("%w: a plugin's provider", ErrStoredRefused)
)

// ApplyStored activates a pasted key: a missing builtin gains its catalog
// spec whole, and a keyless or already-stored spec gains the key. chain says
// the keychain took the write, else the 0600 file did - where the key lives
// is what the keys screen says about it. A key config or the environment
// already answers for is refused - config wins, and a builtin's env var
// outranks the chain, so the hints that name it stay true - and so is a
// plugin's spec, whose endpoint a stored key would ride unasked. A stored
// key is zemi's own: replaceable by a later paste, removable without
// touching config.
func (r *Registry) ApplyStored(id, key string, chain bool) error {
	if key == "" {
		return errors.New("empty key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.storable(id); err != nil {
		return err
	}
	if i := slices.IndexFunc(r.missing, func(m MissingKey) bool { return m.ID == id }); i >= 0 {
		r.specs = append(r.specs, r.missing[i].Spec)
		r.missing = slices.DeleteFunc(r.missing, func(m MissingKey) bool { return m.ID == id })
		r.keys[id] = key
		r.setStored(id, chain)
		return nil
	}
	s := slices.IndexFunc(r.specs, func(s Spec) bool { return s.ID == id })
	if s < 0 {
		return fmt.Errorf("provider %q is not registered", id)
	}
	r.keys[id] = key
	r.setStored(id, chain)
	return nil
}

// CheckStored reports whether a paste may act on id without touching the
// stores, so a refused one never overwrites the key it holds.
func (r *Registry) CheckStored(id string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storable(id)
}

// storable is the paste gate: a plugin's provider never holds a stored key,
// and a key config or the environment answers for outranks one.
func (r *Registry) storable(id string) error {
	if r.plugins[id] {
		return ErrStoredPlugin
	}
	if r.keys[id] != "" && !r.storedHere(id) {
		return fmt.Errorf("%w: %s", ErrStoredRefused, id)
	}
	return nil
}

// RemoveStored drops a stored key: the provider is keyless again, and a
// builtin returns to the Missing list with its catalog spec, so the no-key
// hints name it. False when no stored key is held.
func (r *Registry) RemoveStored(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.stored[id]; !ok {
		return false
	}
	delete(r.stored, id)
	delete(r.keys, id)
	if i := slices.IndexFunc(r.specs, func(s Spec) bool { return s.ID == id }); i >= 0 && r.specs[i].Builtin {
		r.missing = append(r.missing, MissingKey{ID: id, Env: r.specs[i].KeyEnv, Spec: r.specs[i]})
		r.specs = slices.DeleteFunc(r.specs, func(s Spec) bool { return s.ID == id })
	}
	return true
}

// Stored reports whether id holds a pasted key and whether the keychain
// took it, else the 0600 file did - where the key lives is what the keys
// screen says about it.
func (r *Registry) Stored(id string) (inChain, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inChain, ok = r.stored[id]
	return inChain, ok
}

func (r *Registry) setStored(id string, chain bool) {
	if r.stored == nil {
		r.stored = map[string]bool{}
	}
	r.stored[id] = chain
}

// storedHere reports whether id's key is a stored one, not config's or the
// environment's.
func (r *Registry) storedHere(id string) bool {
	_, ok := r.stored[id]
	return ok
}

// Decisions reports the spec and the catalog entry an id names when that model
// answers classification through its provider's own protocol: a decisions
// model is never resolved as a chat model.
func (r *Registry) Decisions(id ModelID) (Spec, Model, bool) {
	providerID, model, _ := id.Split()
	spec, ok := r.Lookup(providerID)
	if !ok {
		return Spec{}, Model{}, false
	}
	m, ok := spec.Model(model)
	if !ok || !m.Decisions {
		return Spec{}, Model{}, false
	}
	return spec, m, true
}

// LogprobOptions is the function that adds a request for the next token's
// probabilities, its top k of them, to a call's own provider options; nil when
// this kind surfaces none. Only openai and openai_compat do: openrouter and
// vercel override fantasy's usage function with their own metadata type, which
// carries no logprobs, and anthropic, google and the openai responses models
// return none at all. A gateway in front of a kind that does answers the same
// way, since it is the same provider.
//
// It amends the kind's own options struct rather than replacing it, so a
// gateway's body fields (routing, a cache setting) ride the request as they do
// on every other call.
func (r *Registry) LogprobOptions(id ModelID) func(fantasy.ProviderOptions, int64) fantasy.ProviderOptions {
	providerID, model, _ := id.Split()
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil
	}
	switch spec.Kind {
	case KindOpenAI:
		if openai.IsResponsesModel(model) { // it drops logprobs itself, with a warning
			return nil
		}
		return func(base fantasy.ProviderOptions, k int64) fantasy.ProviderOptions {
			o, _ := base[openai.Name].(*openai.ProviderOptions)
			o = amend(o)
			o.LogProbs, o.TopLogProbs = new(true), new(k)
			return mergeOptions(base, openai.NewProviderOptions(o))
		}
	case KindOpenAICompat:
		// its own options carry no top-k field, so both go in the body
		return func(base fantasy.ProviderOptions, k int64) fantasy.ProviderOptions {
			o, _ := base[openaicompat.Name].(*openaicompat.ProviderOptions)
			o = amend(o)
			o.ExtraBody = maps.Clone(o.ExtraBody)
			if o.ExtraBody == nil {
				o.ExtraBody = map[string]any{}
			}
			o.ExtraBody["logprobs"], o.ExtraBody["top_logprobs"] = true, k
			return mergeOptions(base, openaicompat.NewProviderOptions(o))
		}
	case KindAnthropic, KindGoogle, KindOpenRouter, KindVercel, KindSystemOne:
		// their providers return no token probabilities (openrouter and vercel
		// build their own metadata), or run no chat call at all
	}
	return nil
}

// amend is o as a copy nothing shares with the call's own options; a fresh one
// when the call carried none of that kind.
func amend[T any](o *T) *T {
	if o == nil {
		return new(T)
	}
	clone := *o
	return &clone
}

// mergeOptions is a and b's provider options together, b's own provider winning
// where both carry one.
func mergeOptions(a, b fantasy.ProviderOptions) fantasy.ProviderOptions {
	out := fantasy.ProviderOptions{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// Key is a provider's API key, for the code that speaks a provider's own
// protocol instead of fantasy's (a decisions model's classify.Protocol).
func (r *Registry) Key(providerID string) string { return r.apiKey(providerID) }

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

// HasKey reports whether the provider has an API key. Values are never
// exposed.
func (r *Registry) HasKey(providerID string) bool { return r.apiKey(providerID) != "" }

func (r *Registry) apiKey(providerID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys[providerID]
}

// Effort is a reasoning effort level /effort offers ("" = the model's
// default).
type Effort string

var Efforts = []Effort{"low", "medium", "high"}

// Effort is the reasoning level an id runs at for the /effort pick ("" =
// unset): a catalog model gets its default level when unset, else the level
// it lists nearest the pick, and none when it lists none; any other model
// gets the pick.
func (r *Registry) Effort(id ModelID, pick Effort) string {
	if !slices.Contains(Efforts, pick) {
		pick = ""
	}
	providerID, model, _ := id.Split()
	spec, _ := r.Lookup(providerID)
	m, found := spec.Model(model)
	switch {
	case !spec.Builtin || !found:
		return string(pick)
	case len(m.Efforts) == 0:
		return ""
	case pick == "":
		return m.Effort
	}
	return nearestEffort(m.Efforts, string(pick))
}

// CallOptions are the provider options a turn runs with: the reasoning
// effort ("" sends none; see Effort) and the spec's extra body fields, in
// the model's provider kind, and for an OpenAI Responses model that reasons
// a reasoning summary; nil when none applies. A model that does not support
// an option fails its turn with the provider's error. OpenAI gets sessionID
// as prompt_cache_key: it routes on a hash of the prompt's first tokens and
// the key, and every session's prompt starts alike, so without one parallel
// sessions and subagents pile onto one cache shard and miss.
func (r *Registry) CallOptions(id ModelID, effort, sessionID string) fantasy.ProviderOptions {
	providerID, model, _ := id.Split()
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil
	}
	var re *openai.ReasoningEffort
	if effort != "" {
		re = new(openai.ReasoningEffort(effort))
	}
	if spec.Kind == KindOpenAI {
		var key *string
		if sessionID != "" {
			key = &sessionID
		}
		switch {
		case openai.IsResponsesModel(model):
			// fantasy drops both reasoning fields for a model it knows does not reason
			return openai.NewResponsesProviderOptions(&openai.ResponsesProviderOptions{ReasoningEffort: re, ReasoningSummary: new("auto"), PromptCacheKey: key})
		case re != nil || key != nil:
			return openai.NewProviderOptions(&openai.ProviderOptions{ReasoningEffort: re, PromptCacheKey: key})
		}
		return nil
	}
	if re == nil && len(spec.Body) == 0 {
		return nil
	}
	switch spec.Kind {
	case KindAnthropic:
		o := &anthropic.ProviderOptions{ExtraBody: spec.Body}
		if re != nil {
			o.Effort = new(anthropic.Effort(effort))
		}
		return anthropic.NewProviderOptions(o)
	case KindOpenAICompat:
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{ReasoningEffort: re, ExtraBody: spec.Body})
	case KindOpenRouter:
		o := &openrouter.ProviderOptions{ExtraBody: spec.Body}
		if re != nil {
			o.Reasoning = &openrouter.ReasoningOptions{Effort: new(openrouter.ReasoningEffort(effort))}
		}
		return openrouter.NewProviderOptions(o)
	case KindVercel:
		o := &vercel.ProviderOptions{ExtraBody: spec.Body}
		if re != nil {
			o.Reasoning = &vercel.ReasoningOptions{Effort: new(vercel.ReasoningEffort(effort))}
		}
		return vercel.NewProviderOptions(o)
	case KindGoogle:
		if re != nil {
			level := strings.ToUpper(effort)
			return fantasy.ProviderOptions{google.Name: &google.ProviderOptions{ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level}}}
		}
	case KindOpenAI: // answered above; the exhaustive linter wants the case
	case KindSystemOne: // a decisions provider runs no chat call
	}
	return nil
}

// SessionHeaders are the per-call headers of a session's requests:
// x-session-affinity, which routers such as Fireworks use to send a
// session to the replica that holds its prompt cache. The value is the
// session's opaque random id; nil for "". NOTE: only a provider or router
// that routes on it gains anything; the rest ignore it.
func SessionHeaders(sessionID string) map[string]string {
	if sessionID == "" {
		return nil
	}
	return map[string]string{"x-session-affinity": sessionID}
}

// effortRank orders the catalog's reasoning levels.
var effortRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// nearestEffort is the listed level closest to effort, the lower on a tie
// (lists are ascending); "" when the model lists none.
func nearestEffort(levels []string, effort string) string {
	if len(levels) == 0 {
		return ""
	}
	dist := func(l string) int {
		d := slices.Index(effortRank, l) - slices.Index(effortRank, effort)
		return max(d, -d)
	}
	return slices.MinFunc(levels, func(a, b string) int { return dist(a) - dist(b) })
}

// Resolve turns "provider/model" into a fantasy language model; a missing
// key surfaces as an error at send time.
func (r *Registry) Resolve(id ModelID) (fantasy.LanguageModel, error) {
	providerID, model, ok := id.Split()
	if !ok {
		return nil, fmt.Errorf("model id must be provider/model, got %q", id)
	}
	spec, ok := r.Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}
	key := r.apiKey(spec.ID)
	switch spec.Kind {
	case KindAnthropic:
		return languageModel(spec, key, model, anthropic.New, anthropic.WithBaseURL, anthropic.WithAPIKey, anthropic.WithHeaders,
			func(c *http.Client) anthropic.Option { return anthropic.WithHTTPClient(c) }, nil)
	case KindOpenAI:
		// fantasy sends only the models openai.IsResponsesModel accepts to
		// /responses, the rest to /chat/completions
		// fantasy's openai options have no extra body, the SDK sets it
		responses := func(o ...openai.Option) (fantasy.Provider, error) {
			o = append(o, openai.WithUseResponsesAPI())
			for _, k := range slices.Sorted(maps.Keys(spec.Body)) {
				o = append(o, openai.WithSDKOptions(option.WithJSONSet(k, spec.Body[k])))
			}
			return openai.New(o...)
		}
		return languageModel(spec, key, model, responses, openai.WithBaseURL, openai.WithAPIKey, openai.WithHeaders,
			func(c *http.Client) openai.Option { return openai.WithHTTPClient(c) }, openai.WithObjectMode)
	case KindOpenAICompat:
		return languageModel(spec, key, model, openaicompat.New, openaicompat.WithBaseURL, openaicompat.WithAPIKey, openaicompat.WithHeaders,
			func(c *http.Client) openaicompat.Option { return openaicompat.WithHTTPClient(c) }, openaicompat.WithObjectMode)
	case KindGoogle:
		return languageModel(spec, key, model, google.New, google.WithBaseURL, google.WithGeminiAPIKey, google.WithHeaders,
			func(c *http.Client) google.Option { return google.WithHTTPClient(c) }, google.WithObjectMode)
	case KindOpenRouter:
		// its URL is fixed; Register refuses a base_url, and a builtin's is the same URL
		return languageModel(spec, key, model, openrouter.New, nil, openrouter.WithAPIKey, openrouter.WithHeaders,
			func(c *http.Client) openrouter.Option { return openrouter.WithHTTPClient(c) }, openrouter.WithObjectMode)
	case KindVercel:
		return languageModel(spec, key, model, vercel.New, vercel.WithBaseURL, vercel.WithAPIKey, vercel.WithHeaders,
			func(c *http.Client) vercel.Option { return vercel.WithHTTPClient(c) }, vercel.WithObjectMode)
	case KindSystemOne:
		return nil, fmt.Errorf("%s is a decisions model: the %s protocol answers classification, and no chat turn runs on it", id, KindSystemOne)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", spec.Kind)
	}
}

// ModelHTTP bounds the wait for response headers, for a chat call and a
// classify protocol call alike: a gateway that accepts the connection and then
// says nothing would otherwise hold the caller until they give up. Client.Timeout
// stays zero, so a long stream is never cut; a stalled one is idleModel's job.
var ModelHTTP = sync.OnceValue(func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: t}
})

func languageModel[O any](spec Spec, key, model string, newProvider func(...O) (fantasy.Provider, error),
	withBaseURL, withAPIKey func(string) O, withHeaders func(map[string]string) O, withClient func(*http.Client) O,
	withObjectMode func(fantasy.ObjectMode) O,
) (fantasy.LanguageModel, error) {
	opts := []O{withClient(ModelHTTP())}
	if spec.BaseURL != "" && withBaseURL != nil {
		opts = append(opts, withBaseURL(spec.BaseURL))
	}
	if key != "" {
		opts = append(opts, withAPIKey(key))
	}
	if len(spec.Headers) > 0 {
		opts = append(opts, withHeaders(spec.Headers))
	}
	if spec.ObjectMode != "" && withObjectMode != nil {
		opts = append(opts, withObjectMode(spec.ObjectMode))
	}
	p, err := newProvider(opts...)
	if err != nil {
		return nil, err
	}
	m, err := p.LanguageModel(context.Background(), model)
	if err != nil {
		return nil, err
	}
	return idleModel{LanguageModel: m, idle: modelIdleTimeout, provider: spec.ID, kind: spec.Kind}, nil
}

// ModelError tags a model failure with its provider, so the advice can
// name the key to check, and keeps fantasy from retrying a connection
// refused or a DNS failure: it retries every net.Error, and a server that
// is not there does not come up in five seconds.
type ModelError struct {
	Provider string
	Err      error
}

func (e ModelError) Error() string        { return e.Err.Error() }
func (e ModelError) Is(target error) bool { return errors.Is(e.Err, target) }
func (e ModelError) As(target any) bool {
	if _, ok := target.(*net.Error); ok && final(e.Err) {
		return false
	}
	return errors.As(e.Err, target)
}

func final(err error) bool {
	_, dns := errors.AsType[*net.DNSError](err)
	return dns || errors.Is(err, syscall.ECONNREFUSED)
}

// modelIdleTimeout is how long a model call may make no progress. The
// header timeout only counts until the status line arrives, so a gateway
// that accepts a stream and then stops sending would hold the turn until
// the user interrupts it.
// NOTE: one fixed ceiling, not a setting: a model that thinks longer than
// this without sending a part is cut off.
const modelIdleTimeout = 5 * time.Minute

var errModelIdle = fmt.Errorf("the model sent nothing for %s", modelIdleTimeout)

// idleModel bounds a call by idle time, resetting on every stream part, so
// a slow but live answer runs as long as it likes. Tools run between model
// calls, so this never covers a build or a test run.
type idleModel struct {
	fantasy.LanguageModel
	idle     time.Duration
	provider string
	kind     Kind
}

// usage brings Gemini's counts to the shape billing and ContextUsed read:
// its prompt count includes the cached tokens and its output count leaves
// out the thinking, which is billed as output.
func (m idleModel) usage(u fantasy.Usage) fantasy.Usage {
	if m.kind == KindGoogle {
		u.InputTokens = max(0, u.InputTokens-u.CacheReadTokens)
		u.OutputTokens += u.ReasoningTokens
	}
	return u
}

func (m idleModel) wrap(err error) error {
	if err == nil {
		return nil
	}
	return ModelError{Provider: m.provider, Err: err}
}

func (m idleModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.idle, errModelIdle)
	defer cancel()
	res, err := m.LanguageModel.Generate(ctx, call)
	if res != nil {
		res.Usage = m.usage(res.Usage)
	}
	return res, m.wrap(err)
}

// GenerateObject is bound by idle time like Generate: an object call is one
// request too, and a provider that stalls on it would hold the caller.
func (m idleModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.idle, errModelIdle)
	defer cancel()
	res, err := m.LanguageModel.GenerateObject(ctx, call)
	if res != nil {
		res.Usage = m.usage(res.Usage)
	}
	return res, m.wrap(err)
}

func (m idleModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(m.idle, func() { cancel(errModelIdle) })
	seq, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, m.wrap(err)
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
			if part.Type == fantasy.StreamPartTypeError {
				part.Error = m.wrap(part.Error)
			}
			if part.Type == fantasy.StreamPartTypeFinish {
				part.Usage = m.usage(part.Usage)
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
