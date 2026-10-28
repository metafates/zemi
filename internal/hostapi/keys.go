// keys.go manages provider API keys: where each one lives, the /keys screen
// that says so, and the paste flow that stores one in the OS keychain. A
// key pasted here is zemi's own (Registry.ApplyStored): a key config or the
// environment already answers for outranks it, so the hints that name those
// stay true.

package hostapi

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/keyring"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// keyRef is where a provider's pasted key lives: the keychain under zemi's
// service, scoped to this tree's data home so a throwaway tree cannot touch
// the real install's key, a 0600 file beside the MCP grants where no chain
// answers.
func (h *Host) keyRef(providerID string) keyring.Ref {
	return keyring.Scoped(h.Dirs.DataHome, "key:"+providerID, filepath.Join(h.Dirs.DataHome, "keys", providerID))
}

// KeysCommand is the /keys command's body: the keys list alone, a provider's
// paste box on "paste <id>", a stored key removed on "clear <id>".
func (h *Host) KeysCommand(args string) error {
	if args = strings.TrimSpace(args); args == "" {
		ui, err := h.Screen()
		if err != nil {
			return err
		}
		ui.OpenOverlay(h.keysOverlay(""))
		return nil
	}
	prov, ok := strings.CutPrefix(args, "paste ")
	if !ok || strings.Contains(prov, " ") {
		if id, ok := strings.CutPrefix(args, "clear "); ok && !strings.Contains(id, " ") {
			return h.clearKey(id)
		}
		return Usagef("usage: /keys [paste <provider> | clear <provider>]")
	}
	if _, registered := h.Providers.Lookup(prov); !registered && !h.missingProvider(prov) {
		return Usagef("no provider %q", prov)
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.pasteKeyOverlay(prov))
	return nil
}

func (h *Host) keysCommands() []*Command {
	return []*Command{{
		Name: "keys", NArgs: NArgsAny, Desc: "manage provider API keys",
		GoFn: func(h *Host, args string, confirm bool) error { return h.KeysCommand(args) },
	}}
}

// missingProvider reports whether id is a builtin waiting for its key.
func (h *Host) missingProvider(id string) bool {
	return slices.ContainsFunc(h.Providers.Missing(), func(m agent.MissingKey) bool { return m.ID == id })
}

// missingKeyRow is the row a builtin without a key gets on both screens that
// list providers: enter pastes, y still copies the export line.
func missingKeyRow(m agent.MissingKey) OverlayItem {
	return OverlayItem{
		Text: m.ID, Section: "Needs a key",
		Desc: "export " + m.Env, Copy: "export " + m.Env + "=",
		Run: "keys paste " + m.ID, Verb: VerbPaste,
	}
}

// KeysOverlay is /keys' list: a row per provider with where its key lives,
// the ones waiting under their own section. provider, when named, is the row
// a rebuild selects.
func (h *Host) keysOverlay(provider string) Overlay {
	o := Overlay{ID: "keys", Title: "Keys", Origin: lua.OriginBuiltin, Empty: "No provider yet: " + h.addProvider() + "."}
	for _, s := range h.Providers.List() {
		o.Items = append(o.Items, h.keyRow(s))
	}
	for _, m := range h.Providers.Missing() {
		o.Items = append(o.Items, missingKeyRow(m))
	}
	if provider != "" {
		o.Selected = slices.IndexFunc(o.Items, func(it OverlayItem) bool {
			return it.ID == provider || it.Text == provider
		})
	}
	return o
}

// keyRow is one keyed or keyless provider: where its key lives, and the
// keys that act on it. A row with a view takes enter, so a paste rides its
// own key; a keyless one has no view and enter pastes.
func (h *Host) keyRow(s agent.Spec) OverlayItem {
	inChain, isStored := h.Providers.Stored(s.ID)
	desc, detail, pasteable := h.keySource(s, inChain, isStored)
	it := OverlayItem{ID: s.ID, Text: s.ID, Desc: desc, Detail: detail}
	if pasteable {
		it.Run, it.Verb = "keys paste "+s.ID, VerbPaste
	}
	if isStored {
		value := "OS keychain"
		if !inChain {
			value = perm.TildePath(h.keyRef(s.ID).File)
		}
		it.Fields = []Field{
			{Name: "kind", Value: string(s.Kind)},
			{Name: "base_url", Value: s.BaseURL},
			{Name: "stored", Value: value},
		}
		if s.KeyEnv != "" {
			it.Fields = append(it.Fields, Field{Name: "env", Value: s.KeyEnv})
		}
		it.Keys = []ItemKey{
			// p, not k: k moves the list (overlay.go's moveSel runs before a
			// row key is consulted), so k here was a dead key the footer named
			{Key: "p", Desc: "paste a key", Run: "keys paste " + s.ID, Verb: VerbOpen},
			{Key: "x", Desc: "remove the stored key", Run: "keys clear " + s.ID, Danger: true},
		}
	}
	return it
}

// keySource says where a provider's key lives, with the sentence its detail
// view shows, and whether a paste may act. detail is "" where the row has no
// view of its own - a keyless one pastes on enter - and where the desc alone
// says it.
func (h *Host) keySource(s agent.Spec, inChain, isStored bool) (desc, detail string, pasteable bool) {
	switch {
	case h.Providers.IsPlugin(s.ID) && !h.Providers.HasKey(s.ID):
		return "no key", "The plugin that registered " + s.ID + " brings its own key; a paste here is refused.", false
	case h.Providers.IsPlugin(s.ID):
		return "from the plugin", "The key comes from the plugin that registered " + s.ID + "; set it there.", false
	case !h.Providers.HasKey(s.ID):
		return "no key", "", true
	case isStored && !inChain:
		ref := h.keyRef(s.ID)
		return "in " + perm.TildePath(ref.File), "No keychain answered when the key was saved, so it lives in " + perm.TildePath(ref.File) + "." + h.keyWinnerNote(s), true
	case isStored:
		return "in the OS keychain", "zemi saved this key in the OS keychain." + h.keyWinnerNote(s), true
	case !s.Builtin:
		return "from init.lua", "The key comes from api_key in init.lua; edit the file to change it. Paste only manages keys zemi stores itself.", false
	default:
		return "from $" + s.KeyEnv, "The key comes from the " + s.KeyEnv + " environment variable; edit it in your shell to change it. Paste only manages keys zemi stores itself.", false
	}
}

// keyWinnerNote names what outranks a stored key, so the screen never lies
// about which one a turn uses.
func (h *Host) keyWinnerNote(s agent.Spec) string {
	if s.Builtin && s.KeyEnv != "" && os.Getenv(s.KeyEnv) != "" {
		return " The " + s.KeyEnv + " environment variable overrides it at the next start."
	}
	if s.Builtin {
		return " Exporting " + s.KeyEnv + " overrides it at the next start."
	}
	return " api_key in init.lua overrides it at the next start."
}

// pasteKey stores a pasted key: the store first (the keychain, the 0600
// file where no chain answers), then the registry, whose ApplyStored owns
// the refusal policy - a refused paste leaves nothing behind, and a key is
// one whole word.
func (h *Host) pasteKey(prov, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return Usagef("the key is empty")
	}
	if strings.ContainsAny(key, " \t\n\r") {
		return Usagef("a key has no spaces; check what you pasted")
	}
	if err := h.Providers.CheckStored(prov); err != nil {
		// refuse before anything is written, with the way back named
		if errors.Is(err, agent.ErrStoredPlugin) {
			return Usagef("a plugin registered %s; paste only fills providers of your own", prov)
		}
		spec, ok := h.Providers.Lookup(prov)
		if !ok {
			return Usagef("no provider %q", prov)
		}
		if !spec.Builtin {
			return Usagef("api_key for %s is set in init.lua; edit it there", prov)
		}
		if spec.KeyEnv != "" && os.Getenv(spec.KeyEnv) != "" {
			return Usagef("%s is exported in your shell; it wins - unset it, or export the new key there", spec.KeyEnv)
		}
		return Usagef("%s already has a key from config or the environment", prov)
	}
	inChain, err := h.keyRef(prov).Set(key)
	if err != nil {
		return err
	}
	if err := h.Providers.ApplyStored(prov, key, inChain); err != nil {
		_ = h.keyRef(prov).Delete() // the race it lost: nothing stored stays half of ours
		return err
	}
	if inChain {
		h.Notify(slog.LevelInfo, "Key saved", prov+", in the OS keychain")
	} else {
		h.Notify(slog.LevelInfo, "Key saved", prov+", in "+perm.TildePath(h.keyRef(prov).File)+" (no keychain)")
	}
	h.keysChanged(prov)
	return nil
}

// pasteKeyOverlay is the masked input a paste opens over the list that
// launched it.
func (h *Host) pasteKeyOverlay(prov string) Overlay {
	return Overlay{
		ID: "keys paste", Title: "Paste the " + prov + " API key", Origin: lua.OriginBuiltin,
		Input: true, Secret: true, Placeholder: "the API key", Note: "Saves in the OS keychain.",
		OnInput: func(text string) error { return h.pasteKey(prov, text) },
	}
}

// clearKey removes a stored key: the provider is keyless again, and the
// no-key hints name it. A key config or the environment answers for is not
// zemi's to remove.
func (h *Host) clearKey(prov string) error {
	if _, isStored := h.Providers.Stored(prov); !isStored {
		return Usagef("%s holds no key of zemi's", prov)
	}
	if err := h.keyRef(prov).Delete(); err != nil {
		return err
	}
	h.Providers.RemoveStored(prov)
	h.Notify(slog.LevelInfo, "Key removed", prov)
	h.keysChanged(prov)
	return nil
}

// loadStoredKeys fills keyless providers from the keychain: a key pasted in
// an earlier run outlives the process. One subprocess per entry, so it runs
// here, off the boot path, once at a time.
func (h *Host) loadStoredKeys() {
	if !h.storedKeysPass.CompareAndSwap(false, true) {
		return
	}
	defer h.storedKeysPass.Store(false)
	filled := false
	for _, m := range h.Providers.Missing() {
		filled = h.fillStored(m.ID, filled)
	}
	for _, s := range h.Providers.List() {
		// a plugin's spec never holds a store entry, so the read would be a
		// guaranteed miss; ApplyStored refuses one that slips through anyway
		if !h.Providers.HasKey(s.ID) && !h.Providers.IsPlugin(s.ID) {
			filled = h.fillStored(s.ID, filled)
		}
	}
	if filled {
		h.keysChanged("")
	}
}

func (h *Host) fillStored(id string, filled bool) bool {
	key, chain, ok := h.keyRef(id).Get()
	if !ok {
		return filled
	}
	// a refusal is the config-won race the pass runs beside, not news
	if err := h.Providers.ApplyStored(id, key, chain); err == nil {
		return true
	}
	return filled
}

// keysChanged redraws what a stored key moved: the model hint, an open
// /keys, and open model pickers. provider, when named, is the row the
// change came from: the rebuilt screens select it.
// NOTE: every key surface joins this probe list; RefreshOverlay builds
// nothing for one the user closed.
func (h *Host) keysChanged(provider string) {
	h.refresh()
	if ui := h.hooks(); ui != nil {
		p := provider
		ui.RefreshOverlay("keys", func() Overlay { return h.keysOverlay(p) })
		h.refreshModelPickers(p)
	}
}
