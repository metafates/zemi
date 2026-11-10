package hostapi

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
)

func seedKeys(h *Host) {
	h.Providers.Reset(agent.Catalog{
		Specs: []agent.Spec{
			{ID: "env", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "ENV_KEY", Models: []agent.Model{{ID: "m"}}},
		},
		Keys:    map[string]string{"env": "ek"},
		Missing: []agent.MissingKey{{ID: "wait", Env: "WAIT_KEY", Spec: agent.Spec{ID: "wait", Kind: agent.KindAnthropic, Builtin: true, KeyEnv: "WAIT_KEY", DefaultModel: "big", Models: []agent.Model{{ID: "big"}}}}},
	})
}

// TestPasteKey: a paste activates in the registry and lands in the store
// (the test keychain refuses, so the 0600 file); a key config or the
// environment already answers for is refused with the way back named.
func TestPasteKey(t *testing.T) {
	h, _ := newTestHost(t)
	seedKeys(h)
	if err := h.Providers.Register(agent.Spec{ID: "cfg", Kind: agent.KindAnthropic}, "ck", false); err != nil {
		t.Fatal(err)
	}
	if err := h.pasteKey("wait", "  kp \n"); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HasKey("wait") || !providersStored(h, "wait") {
		t.Fatal("the pasted key did not activate")
	}
	if key, _, ok := h.keyRef("wait").Get(); !ok || key != "kp" {
		t.Fatalf("stored %q ok=%v", key, ok)
	}
	if s, ok := h.Providers.Lookup("wait"); !ok || !s.Builtin {
		t.Fatalf("spec %+v builtin=%v", s, s.Builtin)
	}
	err := h.pasteKey("cfg", "kp")
	if err == nil || !strings.Contains(err.Error(), "api_key for cfg is set in init.lua") {
		t.Fatalf("config refusal: %v", err)
	}
	t.Setenv("ENV_KEY", "from-shell")
	err = h.pasteKey("env", "kp")
	if err == nil || !strings.Contains(err.Error(), "ENV_KEY is exported in your shell") {
		t.Fatalf("env refusal: %v", err)
	}
	if err := h.clearKey("wait"); err != nil {
		t.Fatal(err)
	}
	if h.Providers.HasKey("wait") || !h.missingProvider("wait") {
		t.Fatal("the key did not go back to waiting")
	}
	if err := h.clearKey("wait"); err == nil {
		t.Fatal("cleared a key nobody stored")
	}
}

// TestLoadStoredKeys: the pass fills a waiting builtin with the spec it
// waits with, and a second pass is a no-op.
func TestLoadStoredKeys(t *testing.T) {
	h, _ := newTestHost(t)
	seedKeys(h)
	if err := config.WriteFileAtomic(h.keyRef("wait").File, []byte("kp")); err != nil {
		t.Fatal(err)
	}
	h.loadStoredKeys()
	if !h.Providers.HasKey("wait") || !providersStored(h, "wait") || h.missingProvider("wait") {
		t.Fatal("the pass did not fill the waiting builtin")
	}
	if s, ok := h.Providers.Lookup("wait"); !ok || !s.Builtin || s.DefaultModel != "big" {
		t.Fatalf("spec %+v", s)
	}
	if err := h.Providers.ApplyStored("wait", "kp", true); err != nil {
		t.Fatalf("second pass: %v", err)
	}
}

// TestPasteOverlayShape pins the masked input's fields and that OnInput, the
// path off the command line, is what saves.
func TestPasteOverlayShape(t *testing.T) {
	h, _ := newTestHost(t)
	seedKeys(h)
	o := h.pasteKeyOverlay("wait")
	if !o.Secret || !o.Input || o.Run != "" || o.OnInput == nil || o.Note != "Saves in the OS keychain." {
		t.Fatalf("overlay %+v", o)
	}
	if err := o.OnInput("kp"); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HasKey("wait") {
		t.Fatal("OnInput did not save")
	}
}

// TestKeysRefuseUnknown: paste and clear name a provider that is not there.
func TestKeysRefuseUnknown(t *testing.T) {
	h, _ := newTestHost(t)
	seedKeys(h)
	if err := h.pasteKey("nope", "kp"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("paste: %v", err)
	}
	if err := h.clearKey("nope"); err == nil {
		t.Fatal("cleared an unknown provider")
	}
}

func providersStored(h *Host, id string) bool {
	_, ok := h.Providers.Stored(id)
	return ok
}
