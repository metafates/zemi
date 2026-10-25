package hostapi

import (
	"fmt"
	"testing"

	"charm.land/fantasy/providers/openaicompat"
)

// TestAutoTitle: the first user message renames the session with the model's
// short title, live and in the store, before that turn's reply lands; later
// turns keep it. The title call carries the provider's body fields like a
// turn does.
func TestAutoTitle(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.provider.add({ id = "c", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", body = { service_tier = "flex" }, models = { { id = "m" } } })
		zemi.config.set("models.default", "c/m")
	`)
	m := reflectStub(t, h, "Parser Fix")
	h.SetUIHooks(&fakeUI{})
	h.reflectMu.Lock() // no detached reflector in this test
	defer h.reflectMu.Unlock()
	first := "the parser drops the last token, can you look?"
	if err := h.AgentSend(first); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	waitFor(t, func() error {
		if title := h.Session().Title; title != "Parser Fix" {
			return fmt.Errorf("title = %q", title)
		}
		return nil
	})
	if s, err := h.DB.GetSession(h.AgentSessionID()); err != nil || s.Title != "Parser Fix" {
		t.Fatalf("stored title = %+v, %v", s, err)
	}
	if po, ok := (*m.lastOpts.Load())["openai-compat"].(*openaicompat.ProviderOptions); !ok || po.ExtraBody["service_tier"] != "flex" {
		t.Fatalf("title call options = %+v", m.lastOpts.Load())
	}
}
