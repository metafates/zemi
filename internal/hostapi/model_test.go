package hostapi

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

const _rolesProvider = `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "big" }, { id = "small" } } })
`

// TestModelTargetNames: what zemi.model.add takes and what it refuses. A
// builtin role and default are reserved, a target's own model may name a
// builtin role or a provider/model id and nothing else, and a name another
// origin holds is the last writer's.
func TestModelTargetNames(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, _rolesProvider+`
		zemi.config.set("models.compact", "a/small")
		zemi.model.add("clf", { desc = "the classifier", model = "compact" })`)
	if id := h.roleModel("clf"); id != "a/small" {
		t.Fatalf("clf runs on %q, want models.compact", id)
	}
	for _, c := range []struct{ name, src, want string }{
		{"a builtin role", `zemi.model.add("compact", { desc = "x" })`, `"compact" is a builtin role`},
		{"default", `zemi.model.add("default", { desc = "x" })`, `"default" is a builtin role`},
		{"a dotted name", `zemi.model.add("a.b", { desc = "x" })`, `name "a.b" must be a lowercase letter`},
		{"no desc", `zemi.model.add("x", { model = "a/big" })`, "desc is what /model roles shows"},
		{"a typo'd key", `zemi.model.add("x", { desc = "x", model_id = "a/big" })`, `spec: unknown field "model_id"`},
		{"its own name as its model", `zemi.model.add("x", { desc = "x", model = "x" })`, `model "x" must name a builtin role (default, agents, compact) or a provider/model id`},
		{"another target as its model", `zemi.model.add("x", { desc = "x", model = "clf" })`, `model "clf" must name a builtin role`},
	} {
		if err := r.ExecuteChunk(t.Context(), c.src, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
	// a del of nothing is false, never an error
	runUser(t, r, `assert(zemi.model.del("nope") == false, "a slot nothing registered")
		assert(zemi.model.del("compact") == false, "a role")`)
	// a plugin registers its own slot, and takes a name the user holds
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("cli-slot", { desc = "the plugin's", model = "a/big" })`, "p.lua", lua.OriginPlugin); err != nil {
		t.Fatalf("a plugin registers a slot: %v", err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("clf", { desc = "the plugin's now" })`, "p.lua", lua.OriginPlugin); err != nil {
		t.Errorf("a plugin taking a user's name: %v", err)
	}
	runUser(t, r, `zemi.model.add("cli-slot", { desc = "the user's now" }) zemi.model.del("clf")`)
	if _, ok := h.target("cli-slot"); !ok || h.Settings.Default("models.cli-slot") == nil {
		t.Error("a user did not take the plugin's slot, or its key went with it")
	}
	if _, ok := h.target("clf"); ok || h.Settings.Default("models.clf") != nil {
		t.Error("del kept the slot or its key")
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("proj", { desc = "x" })`, ".agents/p.lua", lua.OriginProject); err != nil {
		t.Errorf("project code: %v", err)
	}
}

// TestModelTargetHealth: /health checks every target, so an id the provider
// does not list is named there rather than failing the slot's first run. A
// classify slot holds a decisions model by design, which is no turn.
func TestModelTargetHealth(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, _rolesProvider+
		`zemi.model.add("clf", { desc = "the classifier", model = "a/nope" })`+
		`zemi.provider.add({ id = "jev", kind = "system_one", base_url = "http://x", api_key = "k", models = { { id = "m", decisions = true } } })`+
		`zemi.model.add("judge", { desc = "settles asks", classify = true, model = "jev/m" })`)
	rows := h.HealthRows()
	i := slices.IndexFunc(rows, func(r HealthRow) bool { return r.Name == "clf model" })
	if i < 0 || rows[i].Level != HealthWarn || !strings.HasPrefix(rows[i].Detail, "a/nope: not in the provider's models list") {
		t.Fatalf("clf row in %+v", rows)
	}
	if j := slices.IndexFunc(rows, func(r HealthRow) bool { return r.Name == "judge model" }); j >= 0 {
		t.Fatalf("a classify slot's decisions model is checked as a chat one: %+v", rows[j])
	}
}

// TestModelTargetUI: /model roles lists a registered slot among the roles,
// /model opens its list on what an empty key means, and the conversation
// list's follow row is the way back to the provider's default.
func TestModelTargetUI(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, _rolesProvider+`
		zemi.config.set("models.default", "a/big")
		zemi.model.add("clf", { desc = "the classifier", model = "a/small" })`)
	if err := h.OpenModelRoles(); err != nil {
		t.Fatal(err)
	}
	rows := ui.opened[0].Items
	var got []string
	for _, it := range rows {
		got = append(got, it.Text)
	}
	if want := []string{"default", "agents", "compact", "clf"}; !slices.Equal(got, want) {
		t.Fatalf("/model roles lists %v, want %v", got, want)
	}
	if row := rows[3]; row.Desc != "a/small   the classifier" {
		t.Errorf("clf row %+v", row)
	}
	// the slot's list opens on what an empty key means
	if err := h.OpenModelPicker("clf"); err != nil {
		t.Fatal(err)
	}
	head := ui.opened[1].Items[0]
	if head.Text != "Follow the registered model" || head.Desc != "a/small" || head.Tag != "current" {
		t.Errorf("clf's first row %+v", head)
	}
	// the conversation list's head is the follow row, and picking it clears
	// models.default, which nothing else on the list offers
	if err := h.OpenModelPicker("default"); err != nil {
		t.Fatal(err)
	}
	head = ui.opened[2].Items[0]
	if head.Text != "Follow the first usable model" || head.Desc != "a/big" || head.Run != "model default default" {
		t.Errorf("default's first row %+v", head)
	}
	if err := h.SetModel("default default"); err != nil {
		t.Fatal(err)
	}
	if id := h.Settings.Str("models.default"); id != "" {
		t.Errorf("models.default = %q, want it cleared", id)
	}
}

// TestProviderObjectMode: object_mode pins how a provider answers an object
// call (§10); it is refused for the kinds that do not take one and for a value
// that is not a mode.
func TestProviderObjectMode(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({ id = "o", kind = "openai_compat", base_url = "http://x", object_mode = "text", models = { { id = "m" } } })`)
	spec, ok := h.Providers.Lookup("o")
	if !ok || spec.ObjectMode != fantasy.ObjectModeText {
		t.Errorf("spec = %+v, want object_mode text", spec)
	}
	for _, c := range []struct{ name, src, want string }{
		{"a kind that takes none", `zemi.provider.add({ id = "b", kind = "anthropic", object_mode = "tool" })`, "object_mode is taken only by the openai, openai_compat, google, openrouter, vercel kinds"},
		{"a mode that is not one", `zemi.provider.add({ id = "b", kind = "openai", object_mode = "json_mode" })`, `object_mode "json_mode": use auto, json, tool, text`},
		{"a typo", `zemi.provider.add({ id = "b", kind = "openai", object_mdoe = "auto" })`, `spec: unknown field "object_mdoe"`},
	} {
		if err := r.ExecuteChunk(t.Context(), c.src, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// TestProviderModelID: a models entry with no id is refused at the same
// boundary as the provider's own id, not registered as a blank /model row.
func TestProviderModelID(t *testing.T) {
	t.Parallel()
	_, r := newTestHost(t)
	for _, c := range []struct{ src, want string }{
		{`zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://x", models = { { display = "no id" } } })`, "models[1].id is required"},
		{`zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://x", models = { { id = "m" }, { id = "" } } })`, "models[2].id is required"},
	} {
		if _, err := runChunk(t, r, c.src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.src, err, c.want)
		}
	}
}

// TestSetModelArgs: /model's grammar. The cases share one host, in order.
func TestSetModelArgs(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, _rolesProvider+`zemi.config.set("models.default", "a/big")`)
	for _, c := range []struct {
		args   string
		opened string // the overlay it opens
		key    string // else the key it writes
		want   string
		err    string
	}{
		{"", "model", "", "", ""},
		{"default", "model default", "", "", ""},
		{"roles", "model roles", "", "", ""},
		{"compact", "model compact", "", "", ""},
		{"a/small", "", "models.default", "a/small", ""},
		{"compact a/big", "", "models.compact", "a/big", ""},
		{"compact default", "", "models.compact", "", ""},
		{"compact nope/x", "", "", "", `provider "nope" is not registered`},
		{"fast a/big", "", "", "", "usage: /model"},
		{"compact a/big more", "", "", "", "usage: /model"},
	} {
		n := len(ui.opened)
		err := h.SetModel(c.args)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v, want %q", c.args, err, c.err)
			}
		case err != nil:
			t.Errorf("%q: %v", c.args, err)
		case c.opened != "":
			if len(ui.opened) != n+1 || ui.opened[n].ID != c.opened {
				t.Errorf("%q opened %v, want %q", c.args, ui.opened[n:], c.opened)
			}
		case h.Settings.Str(config.Key(c.key)) != c.want:
			t.Errorf("%q: %s = %q, want %q", c.args, c.key, h.Settings.Str(config.Key(c.key)), c.want)
		case len(ui.opened) != n+1 || ui.opened[n].ID != "model" || !ui.opened[n].Refresh:
			t.Errorf("%q refreshed %v, want an open /model", c.args, ui.opened[n:])
		}
	}
	// the roles list tags what follows default, and zemi.model.list says what runs where
	roles := ui.opened[2].Items
	if k := roles[2].Tag; k != "follows default" {
		t.Errorf("compact row key %q", k)
	}
	for _, c := range []struct{ line, want string }{
		{"/model ", "roles update default agents compact a/big a/small"},
		{"/model compact ", "default a/big a/small"},
		{"/model default ", "a/big a/small"},
		{"/model a/big ", ""},
	} {
		// builtins an exported key turns on are left out
		got := slices.DeleteFunc(h.CompleteModel("", c.line), func(s string) bool { return strings.Contains(s, "/") && !strings.HasPrefix(s, "a/") })
		if strings.Join(got, " ") != c.want {
			t.Errorf("complete %q = %v, want %q", c.line, got, c.want)
		}
	}
	runUser(t, r, `zemi.config.set("models.compact", "a/big")
		for _, m in ipairs(zemi.model.list()) do
			local want = m.id == "a/big" and "compact" or "default,agents"
			assert(table.concat(m.roles, ",") == want, m.id .. ": " .. table.concat(m.roles, ","))
		end`)
}

// TestInertBoot: an inert boot loads what config registers and does none of
// its IO but reads: no MCP start, no job, no model call, no file sync.
func TestInertBoot(t *testing.T) {
	project, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "f.txt"), []byte("read"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: project, DataHome: data})
	t.Cleanup(h.Close)
	h.Inert = true
	h.modelFn = func() (fantasy.LanguageModel, error) {
		t.Error("an inert boot called a model")
		return nil, errors.New("no")
	}
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `
		zemi.mcp.add("loud", { command = "/nonexistent/loud" })
		assert(zemi.fs.read("f.txt") == "read")
		local ok, err = pcall(zemi.job.start, { cmd = "true" })
		assert(not ok and tostring(err):find("permission denied (inert)", 1, true), tostring(err))
		local sent, why = zemi.agent.send("hi")
		assert(sent == nil and why, "send went through")
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })
	`)
	if s := h.mcp.servers["loud"]; s == nil || s.status != StatusOff {
		t.Errorf("mcp server %+v, want registered and off", s)
	}
	if _, ok := h.Providers.Lookup("p"); !ok {
		t.Error("the provider after the refused calls is missing")
	}
	if _, err := os.Stat(h.TypesDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the types were synced: %v", err)
	}
}

// TestModelFavorites: f marks the model a row names in models.favorites, and
// the marked models head the picker in the key's own order.
func TestModelFavorites(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, _rolesProvider)
	h.toggleModelFavorite("a/small", "")
	favs := h.Settings.List(config.KeyModelsFavorites)
	if !slices.Equal(favs, []string{"a/small"}) {
		t.Fatalf("favorites = %v", favs)
	}
	// /model's own two rows lead the list; the models follow
	o := h.modelPicker("")
	first := o.Items[2]
	if first.Text != "small" || first.Section != "Favorites" {
		t.Fatalf("the list does not head with the favorite: %+v", o.Items)
	}
	// the key toggles, and marking another model keeps the first one first
	h.toggleModelFavorite("a/big", "")
	o = h.modelPicker("")
	if favs = h.Settings.List(config.KeyModelsFavorites); !slices.Equal(favs, []string{"a/small", "a/big"}) {
		t.Fatalf("favorites = %v", favs)
	}
	if a, b := o.Items[2].Text, o.Items[3].Text; a != "small" || b != "big" {
		t.Errorf("favorites order = %v, %v", a, b)
	}
	h.toggleModelFavorite("a/small", "")
	if favs = h.Settings.List(config.KeyModelsFavorites); !slices.Equal(favs, []string{"a/big"}) {
		t.Fatalf("unmarking left %v", favs)
	}
	// the key acts on a model row alone
	key := o.Keys[slices.IndexFunc(o.Keys, func(k OverlayKey) bool { return k.Key == "f" })]
	if !key.Applies(OverlayItem{ID: "a/big"}) || key.Applies(OverlayItem{Text: "Choose a model per role"}) {
		t.Error("f acts on a row that names no model")
	}
	// a favorite is a copy at the head: its provider's section keeps the row
	h.toggleModelFavorite("a/small", "")
	o = h.modelPicker("")
	var sections []string
	for _, it := range o.Items {
		if it.Text == "small" {
			sections = append(sections, it.Section)
		}
	}
	if !slices.Equal(sections, []string{"Favorites", "a  no API key"}) {
		t.Errorf("the favorite reads %v, want the head copy and the provider row", sections)
	}
}
