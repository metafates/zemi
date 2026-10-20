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

const rolesProvider = `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "big" }, { id = "small" } } })
`

// TestRoleModel: an empty target follows models.chat, which follows the first
// provider's default; a profile's own model moves the conversation only and may
// name a target; a models.* key refuses a target name.
func TestRoleModel(t *testing.T) {
	for _, c := range []struct {
		name, config string
		want         map[string]string // slot -> model id, "" for one left out
		err          string
	}{
		{
			name: "nothing set runs the provider default",
			want: map[string]string{"conversation": "a/big", "chat": "a/big", "agents": "a/big", "titles": "a/big", "compact": "a/big", "memory": "a/big"},
		},
		{
			name:   "unset targets follow chat",
			config: `zemi.config.set("models.chat", "a/small")`,
			want:   map[string]string{"conversation": "a/small", "chat": "a/small", "agents": "a/small", "titles": "a/small", "compact": "a/small", "memory": "a/small"},
		},
		{
			name:   "a set target wins",
			config: `zemi.config.set("models.titles", "a/small") zemi.config.set("models.memory", "a/small")`,
			want:   map[string]string{"conversation": "a/big", "agents": "a/big", "titles": "a/small", "compact": "a/big", "memory": "a/small"},
		},
		{
			name:   "a profile's model moves the conversation only",
			config: `zemi.agent.profile.add("p", { model = "a/small" }) zemi.config.set("agent.profile", "p")`,
			want:   map[string]string{"conversation": "a/small", "agents": "a/big", "titles": "a/big", "compact": "a/big", "memory": "a/big", "p": "a/small"},
		},
		{
			name:   "a profile names a target",
			config: `zemi.config.set("models.titles", "a/small") zemi.agent.profile.add("p", { model = "titles" }) zemi.config.set("agent.profile", "p")`,
			want:   map[string]string{"conversation": "a/small", "agents": "a/big", "titles": "a/small", "compact": "a/big", "memory": "a/big", "p": "a/small"},
		},
		{
			name:   "a pick on a profile's own slot beats its model",
			config: `zemi.agent.profile.add("p", { model = "a/big" }) zemi.config.set("agent.profile", "p") zemi.config.set("models.p", "a/small")`,
			want:   map[string]string{"conversation": "a/small", "p": "a/small"},
		},
		{
			name:   "a defined default profile runs the conversation",
			config: `zemi.agent.profile.add("default", { model = "a/small" })`,
			want:   map[string]string{"conversation": "a/small", "default": "a/small", "titles": "a/big"},
		},
		{
			name:   "and the chat pick wins over its model",
			config: `zemi.agent.profile.add("default", { model = "a/small" }) zemi.config.set("models.chat", "a/big")`,
			want:   map[string]string{"conversation": "a/big", "default": "a/big", "chat": "a/big", "titles": "a/big"},
		},
		{
			name:   "a key refuses a target name",
			config: `zemi.config.set("models.titles", "chat")`,
			err:    `config key models.titles expects a provider/model id or ""`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHost(t)
			if c.err != "" {
				runExpectErr(t, r, rolesProvider+c.config, c.err)
				return
			}
			runUser(t, r, rolesProvider+c.config)
			p, _ := h.resolveProfile("")
			got := map[string]string{"conversation": h.currentModelID(p), "default": h.roleModel("default")}
			for _, n := range h.targetNames() {
				got[n] = h.roleModel(n)
			}
			for name, want := range c.want {
				if got[name] != want {
					t.Errorf("%s runs on %q, want %q (%v)", name, got[name], want, got)
				}
			}
		})
	}
}

// TestModelTargetNames: what zemi.model.add takes and what it refuses. A
// builtin role and default are reserved, a target's own model may name a
// builtin role or a provider/model id and nothing else, and a name another
// origin holds is refused until a more trusted one takes it back.
func TestModelTargetNames(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, rolesProvider+`
		zemi.config.set("models.titles", "a/small")
		zemi.model.add("clf", { desc = "the classifier", model = "titles" })`)
	if id := h.roleModel("clf"); id != "a/small" {
		t.Fatalf("clf runs on %q, want models.titles", id)
	}
	for _, c := range []struct{ name, src, want string }{
		{"a builtin role", `zemi.model.add("chat", { desc = "x" })`, `"chat" is a builtin role`},
		{"default", `zemi.model.add("default", { desc = "x" })`, `"default" is a shipped profile`},
		{"a dotted name", `zemi.model.add("a.b", { desc = "x" })`, `name "a.b" must be a lowercase letter`},
		{"no desc", `zemi.model.add("x", { model = "a/big" })`, "desc is what /model roles shows"},
		{"a typo'd key", `zemi.model.add("x", { desc = "x", model_id = "a/big" })`, `spec: unknown field "model_id"`},
		{"its own name as its model", `zemi.model.add("x", { desc = "x", model = "x" })`, `model "x" must name a builtin role (chat, agents, titles, compact, memory) or a provider/model id`},
		{"another target as its model", `zemi.model.add("x", { desc = "x", model = "clf" })`, `model "clf" must name a builtin role`},
		{"a profile named after a role", `zemi.agent.profile.add("titles", {})`, `"titles" is a builtin role`},
		{"a profile naming another target", `zemi.agent.profile.add("p", { model = "clf" })`, `model "clf" must name a builtin role`},
		{"a slot nothing registered", `zemi.model.del("nope")`, `"nope" is not registered`},
		{"a role", `zemi.model.del("titles")`, `"titles" is not registered`},
	} {
		if err := r.ExecuteChunk(t.Context(), c.src, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
	// a plugin registers its own slot; it may not take a user's name, and the
	// user may take the plugin's back
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("cli-slot", { desc = "the plugin's", model = "a/big" })`, "p.lua", lua.OriginPlugin); err != nil {
		t.Fatalf("a plugin registers a slot: %v", err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("clf", { desc = "mine now" })`, "p.lua", lua.OriginPlugin); err == nil || !strings.Contains(err.Error(), "model target clf is registered by user config") {
		t.Errorf("a plugin taking a user's name: %v", err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.model.del("clf")`, "p.lua", lua.OriginPlugin); err == nil || !strings.Contains(err.Error(), "registered by user config") {
		t.Errorf("a plugin dropping a user's slot: %v", err)
	}
	runUser(t, r, `zemi.model.add("cli-slot", { desc = "the user's now" }) zemi.model.del("clf")`)
	if _, ok := h.target("cli-slot"); !ok || h.Settings.Default("models.cli-slot") == nil {
		t.Error("a user did not take the plugin's slot, or its key went with it")
	}
	if _, ok := h.target("clf"); ok || h.Settings.Default("models.clf") != nil {
		t.Error("del kept the slot or its key")
	}
	// a profile is not a slot zemi.model.del drops: nothing removes a definition
	if err := r.ExecuteChunk(t.Context(), `zemi.agent.profile.add("p", {}) zemi.model.del("p")`, "test.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), `"p" is a profile`) {
		t.Errorf("del of a profile: %v", err)
	}
	// project code may not register a slot at all
	if err := r.ExecuteChunk(t.Context(), `zemi.model.add("proj", { desc = "x" })`, ".agents/p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "project code registers no model target") {
		t.Errorf("project code: %v", err)
	}
}

// TestModelTargetPick: a slot config registered is picked and kept like any
// other key: the pick outlives a restart, and a reload whose replay registers
// it again keeps it too.
func TestModelTargetPick(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	dirs := config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()}
	initLua := filepath.Join(dir, "init.lua")
	src := rolesProvider + `zemi.model.add("clf", { desc = "the classifier", model = "a/small" })`
	if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var h *Host
	steps := func() []lua.BootStep {
		return []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	}
	start := func() {
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
		h = NewHost(r, log, config.NewStore(), dirs)
		h.SetUIHooks(&fakeUI{})
		if err := r.Boot(t.Context(), steps()); err != nil {
			t.Fatal(err)
		}
	}
	start()
	if id := h.roleModel("clf"); id != "a/small" {
		t.Fatalf("clf runs on %q, want the model it was added with", id)
	}
	h.KeepPicks.Store(true)
	if err := h.SetModel("clf a/big"); err != nil {
		t.Fatal(err)
	}
	if err := h.ForceReload(t.Context(), steps()); err != nil {
		t.Fatal(err)
	}
	if id := h.roleModel("clf"); id != "a/big" {
		t.Errorf("after a reload clf runs on %q, want the pick", id)
	}
	start()
	if id := h.roleModel("clf"); id != "a/big" {
		t.Errorf("after a restart clf runs on %q, want the pick", id)
	}
	if got := h.cfgSource("models.clf"); got != "saved" {
		t.Errorf("the pick reads as %q", got)
	}
}

// TestModelTargetProfileKey: a profile re-registers its models.<name> key on
// every boot that defines it, re-applying the pick kept for the key; a reload
// without the profile drops the key with it.
func TestModelTargetProfileKey(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()})
	h.SetUIHooks(&fakeUI{})
	initLua := filepath.Join(dir, "init.lua")
	steps := func() []lua.BootStep {
		return []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	}
	reload := func(src string) {
		t.Helper()
		if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.ForceReload(t.Context(), steps()); err != nil {
			t.Fatal(err)
		}
	}
	// the pick needs KeepPicks, as /model under the TUI has
	h.KeepPicks.Store(true)
	reload(rolesProvider + `zemi.agent.profile.add("planner", { model = "a/small" })`)
	if id := h.roleModel("planner"); id != "a/small" {
		t.Fatalf("the profile runs on %q, want its model", id)
	}
	if err := h.SetModel("planner a/big"); err != nil {
		t.Fatal(err)
	}
	reload(rolesProvider) // the profile is gone; its key goes with it
	if slices.ContainsFunc(h.Settings.Schema(), func(s config.Setting) bool { return s.Path == "models.planner" }) {
		t.Error("the dead profile's key stayed in the schema")
	}
	reload(rolesProvider + `zemi.agent.profile.add("planner", { model = "a/small" })`)
	if id := h.roleModel("planner"); id != "a/big" {
		t.Errorf("the re-registered profile runs on %q, want the kept pick", id)
	}
}

// TestModelTargetHealth: /health checks every target, so an id the provider
// does not list is named there rather than failing the slot's first run. A
// classify slot holds a decisions model by design, which is no turn.
func TestModelTargetHealth(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, rolesProvider+
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

// TestModelTargetUI: /model roles lists a registered slot among the roles and
// the profiles after them, /model opens its list on what an empty key means,
// and the slot's model is pickable from a profile's row too.
func TestModelTargetUI(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, rolesProvider+`
		zemi.config.set("models.chat", "a/big")
		zemi.model.add("clf", { desc = "the classifier", model = "a/small" })
		zemi.agent.profile.add("cheap", { model = "a/small" })`)
	if err := h.OpenModelRoles(); err != nil {
		t.Fatal(err)
	}
	rows := ui.opened[0].Items
	var got []string
	for _, it := range rows {
		got = append(got, it.(map[string]any)["text"].(string))
	}
	if want := []string{"chat", "agents", "titles", "compact", "memory", "clf", "cheap"}; !slices.Equal(got, want) {
		t.Fatalf("/model roles lists %v, want %v", got, want)
	}
	if row := rows[5].(map[string]any); row["desc"] != "a/small · the classifier" {
		t.Errorf("clf row %v", row)
	}
	if row := rows[6].(map[string]any); row["desc"] != "a/small · every tool" || row["key"] != "a/small" {
		t.Errorf("cheap row %v (its spec model is what the row tags)", row)
	}
	// the active profile takes row one, and its own row is not repeated
	if err := h.SetProfile("cheap"); err != nil {
		t.Fatal(err)
	}
	if err := h.OpenModelRoles(); err != nil {
		t.Fatal(err)
	}
	rows = ui.opened[1].Items
	if row := rows[0].(map[string]any); row["text"] != "cheap" || row["key"] != nil {
		t.Errorf("row one %v", row)
	}
	var names []string
	for _, it := range rows {
		names = append(names, it.(map[string]any)["text"].(string))
	}
	if slices.Contains(names[1:], "cheap") {
		t.Errorf("the profile row one shows is repeated: %v", names)
	}
	// the slot's list opens on what an empty key means
	if err := h.OpenModelPicker("clf"); err != nil {
		t.Fatal(err)
	}
	head := ui.opened[2].Items[0].(map[string]any)
	if head["text"] != "Follow the registered model" || head["desc"] != "a/small" || head["key"] != "current" {
		t.Errorf("clf's first row %v", head)
	}
	if err := h.OpenModelPicker("cheap"); err != nil {
		t.Fatal(err)
	}
	head = ui.opened[len(ui.opened)-1].Items[0].(map[string]any)
	if head["text"] != "Follow the profile's model" || head["desc"] != "a/small" {
		t.Errorf("cheap's first row %v", head)
	}
	// a profile named default is row one under its own name, but what it opens
	// is the conversation's slot, chat
	runUser(t, r, `zemi.agent.profile.add("default", { model = "a/small" }) zemi.config.set("agent.profile", "default")`)
	if err := h.OpenModelRoles(); err != nil {
		t.Fatal(err)
	}
	if row := ui.opened[len(ui.opened)-1].Items[0].(map[string]any); row["text"] != "default" || row["run"] != "model chat" || row["key"] != nil {
		t.Errorf("row one under a defined default %v", row)
	}
	// and the list a bare /model opens marks the model the conversation runs
	// on, which is the active profile's slot
	if err := h.SetProfile("cheap"); err != nil {
		t.Fatal(err)
	}
	if err := h.OpenModelPicker(""); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if row := o.Items[o.Selected].(map[string]any); row["key"] != "current" || row["run"] != "model a/small" {
		t.Errorf("the bare list marks %v, want the conversation's model", row)
	}
}

// TestProviderObjectMode: object_mode pins how a provider answers an object
// call (§10); it is refused for the kinds that do not take one and for a value
// that is not a mode.
func TestProviderObjectMode(t *testing.T) {
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

// TestSetModelArgs: /model's grammar. The cases share one host, in order.
func TestSetModelArgs(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, rolesProvider+`zemi.config.set("models.chat", "a/big")`)
	for _, c := range []struct {
		args   string
		opened string // the overlay it opens
		key    string // else the key it writes
		want   string
		err    string
	}{
		{"", "model", "", "", ""},
		{"chat", "model chat", "", "", ""},
		{"roles", "model roles", "", "", ""},
		{"titles", "model titles", "", "", ""},
		{"a/small", "", "models.chat", "a/small", ""},
		{"titles a/big", "", "models.titles", "a/big", ""},
		{"titles chat", "", "models.titles", "", ""},
		{"titles nope/x", "", "", "", `provider "nope" is not registered`},
		{"fast a/big", "", "", "", "usage: /model"},
		{"titles a/big more", "", "", "", "usage: /model"},
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
		case h.Settings.Str(c.key) != c.want:
			t.Errorf("%q: %s = %q, want %q", c.args, c.key, h.Settings.Str(c.key), c.want)
		case len(ui.opened) != n+1 || ui.opened[n].ID != "model" || !ui.opened[n].Refresh:
			t.Errorf("%q refreshed %v, want an open /model", c.args, ui.opened[n:])
		}
	}
	// the roles list tags what follows chat, and zemi.model.list says what runs where
	roles := ui.opened[2].Items
	if k := roles[2].(map[string]any)["key"]; k != "follows chat" {
		t.Errorf("titles row key %v", k)
	}
	for _, c := range []struct{ line, want string }{
		{"/model ", "roles update chat agents titles compact memory a/big a/small"},
		{"/model titles ", "chat a/big a/small"},
		{"/model chat ", "a/big a/small"},
		{"/model a/big ", ""},
	} {
		// builtins an exported key turns on are left out
		got := slices.DeleteFunc(h.CompleteModel("", c.line), func(s string) bool { return strings.Contains(s, "/") && !strings.HasPrefix(s, "a/") })
		if strings.Join(got, " ") != c.want {
			t.Errorf("complete %q = %v, want %q", c.line, got, c.want)
		}
	}
	runUser(t, r, `zemi.config.set("models.memory", "a/big")
		for _, m in ipairs(zemi.model.list()) do
			local want = m.id == "a/big" and "memory" or "chat,agents,titles,compact"
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
