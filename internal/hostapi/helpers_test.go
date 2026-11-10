package hostapi

import (
	"log/slog"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/runtime"
)

// dumpItems renders rows as name:value words in name order, keys and
// fields nested: the fixture form the list tests read a row by.
func dumpItems(items []OverlayItem) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = dumpItem(it)
	}
	return "[" + strings.Join(out, " ") + "]"
}

// dumpFields renders a view's fields as [name value] words.
func dumpFields(fields []Field) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = "[" + f.Name + " " + f.Value + "]"
	}
	return strings.Join(out, " ")
}

// dumpItem is one row in dumpItems' form.
func dumpItem(it OverlayItem) string {
	var words []string
	add := func(k, v string) {
		if v != "" {
			words = append(words, k+":"+v)
		}
	}
	add("copy", it.Copy)
	if it.Danger {
		add("danger", "true")
	}
	add("desc", it.Desc)
	add("detail", it.Detail)
	if len(it.Fields) > 0 {
		add("fields", dumpFields(it.Fields))
	}
	add("fill", it.Fill)
	add("id", it.ID)
	add("tag", it.Tag)
	if len(it.Keys) > 0 {
		add("keys", "["+strings.Join(dumpKeys(it.Keys), " ")+"]")
	}
	if it.Locked != nil {
		locked := "false"
		if *it.Locked {
			locked = "true"
		}
		add("locked", locked)
	}
	add("mark", string(it.Mark))
	if it.On != nil {
		onv := "false"
		if *it.On {
			onv = "true"
		}
		add("on", onv)
	}
	add("preview", it.Preview)
	add("run", it.Run)
	add("section", it.Section)
	if it.Tail {
		add("tail", "true")
	}
	add("text", it.Text)
	add("verb", string(it.Verb))
	return strings.Join(words, " ")
}

// dumpKeys renders a row's keys as name:value words in name order.
func dumpKeys(keys []ItemKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		var words []string
		add := func(name, v string) {
			if v != "" {
				words = append(words, name+":"+v)
			}
		}
		if k.Danger {
			add("danger", "true")
		}
		add("desc", k.Desc)
		add("fill", k.Fill)
		add("id", k.ID)
		add("key", k.Key)
		add("run", k.Run)
		add("verb", string(k.Verb))
		out[i] = strings.TrimSpace(strings.Join(words, " "))
	}
	return out
}

// newWindow boots one host with its runtime and the tui modules loaded, its
// turns on a reply stub, returned for sendTurns.
func newWindow(t *testing.T, stateHome, projectDir string) (*Host, *replyStubModel) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: runtime.FS})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		StateHome:  stateHome,
		DataHome:   t.TempDir(),
		ProjectDir: projectDir,
	})
	t.Cleanup(h.Close)
	h.ReleaseBuilder()
	h.SetUIHooks(&fakeUI{})
	m := replyStub(t, h, "ok")
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, m
}

// at walks the object keys of v, failing the test where one is missing.
func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %T is not an object", k, v)
		}
		v = obj[k]
	}
	return v
}

// textPart is part i of a message's content, which these views write as text.
func textPart(t *testing.T, parts []fantasy.MessagePart, i int) fantasy.TextPart {
	t.Helper()
	p, ok := parts[i].(fantasy.TextPart)
	if !ok {
		t.Fatalf("part %d is %T, want text", i, parts[i])
	}
	return p
}

// fakeHooks is the fake UI the host under test reports to.
func fakeHooks(t *testing.T, h *Host) *fakeUI {
	t.Helper()
	ui, ok := h.hooks().(*fakeUI)
	if !ok {
		t.Fatalf("hooks = %T, want the fake UI", h.hooks())
	}
	return ui
}
