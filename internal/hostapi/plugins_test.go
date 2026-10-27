package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestPluginsStep boots plugins laid out under installed/ and local/, then
// user init.lua, and reads what ran, the step's error, the zemi.state keys,
// the /health row and the skills, agents, commands and workflows found.
func TestPluginsStep(t *testing.T) {
	tool := func(name string) string {
		return `zemi.tool.add({ name = "` + name + `", description = "d", execute = function() return "" end })`
	}
	// res adds a skill, an agent, a command and a workflow named name under root
	res := func(files map[string]string, root, name string) map[string]string {
		// the config dir keeps its workflows beside init.lua; a plugin's Lua,
		// workflows included, lives under .zemi
		wf := root + "workflows/"
		if root != "" {
			wf = root + plugin.CodeDir + "/workflows/"
		}
		maps.Copy(files, map[string]string{
			root + "skills/" + name + "/SKILL.md": "---\nname: " + name + "\ndescription: d\n---\nbody",
			root + "agents/" + name + ".md":       "---\ndescription: d\n---\nbe brief",
			root + "commands/" + name + ".md":     "do it",
			wf + name + ".lua":                    "-- d\nreturn 1",
		})
		return files
	}
	for _, c := range []struct {
		name    string
		plugins map[string]string // under the plugins root
		user    string            // init.lua
		cfg     map[string]string // more files under the config home
		seed    map[string]string // zemi.state keys set before boot
		want    []string
		not     []string
	}{
		{
			name: "sorted by name, disabled skipped",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua": `print("a")`, "local/b/.zemi/init.lua": `print("b")`,
				"installed/c/.zemi/init.lua": `print("c")`, "disabled": "c\n",
			},
			want: []string{"a\nb\n", "health: 2 plugins ready\n"},
			not:  []string{"c\n"},
		},
		{
			name:    "a local plugin named like an installed one",
			plugins: map[string]string{"installed/x/.zemi/init.lua": `print("installed")`, "local/x/.zemi/init.lua": `print("local")`},
			want:    []string{"installed\n", "plugin x: an installed plugin has this name"},
			not:     []string{"local\n"},
		},
		{
			name:    "a failing plugin does not stop the rest",
			plugins: map[string]string{"local/a/.zemi/init.lua": `error("broken")`, "local/b/.zemi/init.lua": `print("b")`},
			want:    []string{"plugin a: ", "broken", "b\n", "health: 2 plugins: 1 failed, 1 ready"},
		},
		{
			name: "a uses name outside the vocabulary refuses the plugin",
			plugins: map[string]string{
				"local/a/.zemi/init.lua":    `print("a ran")`,
				"local/a/.zemi/plugin.json": `{"uses": ["http", "fs.read"]}`,
			},
			want: []string{
				`plugin a: uses "fs.read" is not a capability; the valid ones are agent, fs.write, http, job, model, perm, tool, workflow`,
				"health: 1 plugin: 1 failed, 0 ready",
			},
			not: []string{"a ran"},
		},
		{
			name: "a manifest that does not parse is recorded and declares nothing",
			plugins: map[string]string{
				"local/a/.zemi/init.lua":    `print("a ran")`,
				"local/a/.zemi/plugin.json": `{"uses": ["http"`,
			},
			want: []string{"a ran", "error: plugin a: "},
		},
		{name: "none, and the row still shows", want: []string{"health: none on"}},
		{
			name: "a plugin in the layout before .zemi is refused, a library-only one loads",
			plugins: map[string]string{
				"local/a/init.lua":             `print("a ran")`,
				"local/b/.zemi/lua/b/init.lua": `return {}`,
			},
			want: []string{
				"plugin a: init.lua is at the plugin root, where zemi no longer loads it: move init.lua, lua/, tests/ and workflows/ under .zemi/",
				"health: 2 plugins: 1 failed, 1 ready",
			},
			not: []string{"a ran", "plugin b: "},
		},
		{
			name: "two plugins collide on a command, a tool, a health row and a completion source",
			plugins: map[string]string{
				"local/a/.zemi/init.lua": `zemi.command.add("dup", function() end, {}) zemi.command.add("dup", function() end, {}) ` + tool("dup_tool") +
					` zemi.health.add("dup", function() return "" end) zemi.composer.add({ id = "dup", trigger = "#", items = function() return {} end })`,
				"local/b/.zemi/init.lua": `zemi.command.add("dup", function() end, {})`,
				"local/c/.zemi/init.lua": tool("dup_tool"),
				"local/d/.zemi/init.lua": `zemi.health.add("dup", function() return "" end)`,
				"local/e/.zemi/init.lua": `zemi.composer.add({ id = "dup", trigger = "#", items = function() return {} end })`,
			},
			want: []string{
				"plugin b: ", "command /dup is registered by plugin a, not b", "plugin c: ", `tool "dup_tool" is registered by plugin a, not c`,
				"plugin d: ", "health row dup is registered by plugin a, not d", "plugin e: ", "completion source dup is registered by plugin a, not e",
			},
			not: []string{"plugin a: "},
		},
		{
			name: "a broken file hides no other plugin's of its name",
			plugins: res(map[string]string{
				"local/a/agents/x.md": "---\n---\nno description", "local/a/commands/x.md": "",
			}, "local/b/", "x"),
			want: []string{"agent x plugin:b\n", "command x plugin:b\n"},
			not:  []string{"already provided"},
		},
		{
			name: "state and principal are per plugin, a gone plugin's state goes",
			plugins: map[string]string{
				"local/a/.zemi/init.lua":    `zemi.state.set("k", "a") local ok = pcall(zemi.job.start, { cmd = "true" }) print("a job", ok)`,
				"local/b/.zemi/init.lua":    `zemi.state.set("k", "b") print(zemi.state.get("k")) local ok = pcall(zemi.job.start, { cmd = "true" }) print("b job", ok)`,
				"local/b/.zemi/plugin.json": `{"uses": ["job"]}`,
			},
			seed: map[string]string{"plugin:gone:k": "1", "plugin:a:old": "1"},
			want: []string{"plugin:a:k\n", "plugin:a:old\n", "plugin:b:k\n", "b\n", "a job false", "b job true"},
			not:  []string{"plugin:gone:k", "permission ask required"},
		},
		{
			name:    "a plugin installed again under the name does not get the old one's state",
			plugins: map[string]string{"installed/a/.zemi/init.lua": `print(zemi.state.get("k"))`, "installed/a/.git/zemi-install": "new\n"},
			seed:    map[string]string{"plugin:a": "1", "plugin:a:k": "1"},
			want:    []string{"nil\n", "plugin:a\n"},
			not:     []string{"plugin:a:k"},
		},
		{
			name: "a kindless row from before marketplaces keeps a copy's state",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua": `print(zemi.state.get("k"))`,
				"marketplaces.json":          `{"installs":{"a":{"marketplace":"m","entry":"a","version":"1.0.0"}}}`,
			},
			seed: map[string]string{"plugin:a:k": "1"},
			want: []string{"1\n", "plugin:a:k\n"},
			not:  []string{`plugin:a "`},
		},
		{
			name: "a copy's backfilled row adopts its id and keeps the state",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua": `print(zemi.state.get("k"))`,
				"marketplaces.json":          `{"installs":{"a":{"marketplace":"m","entry":"a","kind":"copy","id":"copy-1"}}}`,
			},
			seed: map[string]string{"plugin:a:k": "1"},
			want: []string{"1\n", `plugin:a "copy-1"`},
		},
		{
			name: "a git install whose marketplace row is gone keeps its state",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua":    `print(zemi.state.get("k"))`,
				"installed/a/.git/zemi-install": "clone-1\n",
			},
			seed: map[string]string{"plugin:a": `"clone-1"`, "plugin:a:k": "1"},
			want: []string{"1\n", "plugin:a:k\n"},
		},
		{
			name: "a copy installed again under the name does not get the old one's state",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua": `print(zemi.state.get("k"))`,
				"marketplaces.json":          `{"installs":{"a":{"marketplace":"m","entry":"a","kind":"copy","id":"new-1"}}}`,
			},
			seed: map[string]string{"plugin:a": `"old-1"`, "plugin:a:k": "1"},
			want: []string{"nil\n", `plugin:a "new-1"`},
			not:  []string{"plugin:a:k"},
		},
		{
			name:    "a failed scan keeps every plugin's state",
			plugins: map[string]string{"local/a/.zemi/init.lua": "", "disabled/x": ""},
			seed:    map[string]string{"plugin:a:saved": "1"},
			want:    []string{"is a directory", "plugin:a:saved\n"},
		},
		{
			name: "a setup() that init.lua calls runs as the plugin, through a tail call and into another plugin",
			plugins: map[string]string{
				"local/p/.zemi/lua/p/init.lua": `return {
					setup = function()
						print(tostring(pcall(zemi.config.set, "agent.effort", "low")))
						zemi.state.set("ran", true)
						require("b").put("via-setup")
						zemi.keymap.set("ctrl+g", function() require("b").put("via-keymap") end)
					end,
					tail = function(f) return f("default") end,
				}`,
				"local/b/.zemi/lua/b/init.lua": `return { put = function(k) zemi.state.set(k, 1) end }`,
			},
			user: `require("p").setup() print(pcall(require("p").tail, function(m) zemi.config.set("agent.effort", m) end))`,
			want: []string{"true\n", "plugin:p:ran\n", "plugin:b:via-setup\n", "plugin:b:via-keymap\n"},
		},
		{
			name: "require searches every tree, a plugin's own code dir first, and one file is one instance",
			plugins: map[string]string{
				"installed/a/.zemi/init.lua": `require("shared").x = "a"
					print(pcall(require, "../../../local/off/.zemi/lua/off/init"))`,
				"installed/a/.zemi/lua/b/init.lua": `return { setup = function() print("stolen " .. tostring(zemi.state.get("token"))) end }`,
				"local/b/.zemi/init.lua":           `zemi.state.set("token", "secret")`,
				"local/b/.zemi/lua/b/init.lua": `return { setup = function()
					print("setup " .. zemi.state.get("token") .. " " .. require("b.util").who .. " " .. tostring(require("shared").x))
				end }`,
				"local/b/.zemi/lua/b/util.lua":     `return { who = "plugin" }`,
				"local/off/.zemi/lua/off/init.lua": `print("off ran")`,
				"disabled":                         "off\n",
			},
			cfg:  map[string]string{"lua/b/util.lua": `return { who = "user" }`, "lua/shared.lua": `return {}`},
			user: `require("b").setup()`,
			want: []string{"outside plugin a", "setup secret plugin a\n"},
			not:  []string{"stolen", "off ran"},
		},
		{
			name: "a plugin's skills, agents, commands and workflows load, a disabled plugin's do not, the user's win",
			plugins: res(res(res(map[string]string{"installed/a/skills/s/zemi-skill.lua": `print("hook")`, "installed/a/.git/config": "", "disabled": "off\n"},
				"installed/a/", "s"), "installed/a/", "mine"), "local/off/", "gone"),
			cfg: res(map[string]string{}, "", "mine"),
			want: []string{
				"skill s plugin:a true\n", "skill mine user true\n", "agent s plugin:a\n", "agent mine user\n",
				"command s plugin:a\n", "command mine user\n", "workflow s plugin:a\n", "workflow mine user\n",
				"provides: command /s · skill s · agent s · workflow s\n",
			},
			not: []string{"gone", "hook", "mine plugin", "error: plugins", "error: skills"},
		},
		{
			name:    "two plugins ship one name: the first by name keeps it and /errors names both",
			plugins: res(res(map[string]string{}, "installed/b/", "x"), "local/a/", "x"),
			want: []string{
				"skill x plugin:a true\n", "agent x plugin:a\n", "command x plugin:a\n", "workflow x plugin:a\n",
				"error: plugins: agent x is already provided by plugin a, not b\n",
				"error: plugins: workflow x is already provided by plugin a, not b\n",
				"error: skills: <plugins>/installed/b/skills/x: skill x is already in <plugins>/local/a/skills/x\n",
				"error: plugins: command x is already provided by plugin a, not b\n",
			},
			not: []string{"plugin:b"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// the project apart from zemi's dirs, so no read settles as a project read
			dirs := config.Dirs{ProjectDir: filepath.Join(tmp, "proj"), ConfigHome: filepath.Join(tmp, "cfg"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
			files := map[string]string{filepath.Join(dirs.ConfigHome, "init.lua"): c.user, filepath.Join(dirs.ProjectDir, ".keep"): ""}
			for p, src := range c.cfg {
				files[filepath.Join(dirs.ConfigHome, p)] = src
			}
			for p, src := range c.plugins {
				files[filepath.Join(dirs.PluginsRoot(), p)] = src
			}
			for p, src := range files {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{
				{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
				{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
			}})
			var out strings.Builder
			r.SetPrintHook(func(line string) { fmt.Fprintf(&out, "%s\n", line) })
			h := NewHost(r, log, config.NewStore(), dirs)
			for k, v := range c.seed {
				if err := h.GlobalDB.SetKV(k, v); err != nil {
					t.Fatal(err)
				}
			}
			step := h.PluginsStep()
			run := step.Run
			step.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
				fmt.Fprintf(&out, "step: %v\n", run(ctx, r, g))
				return nil
			}
			if err := r.Boot(t.Context(), []lua.BootStep{
				h.NamespacesStep(), step,
				lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser),
				h.SkillsUserHooksStep(), h.MarkdownCommandsStep(),
			}); err != nil {
				t.Fatal(err)
			}
			if b := h.Keymaps.Lookup("", "ctrl+g"); b != nil {
				if err := h.RunRHS(t.Context(), b); err != nil {
					t.Fatal(err)
				}
			}
			keys, _ := h.GlobalDB.KeysKV("")
			for _, k := range keys {
				fmt.Fprintf(&out, "%s\n", k)
				// an install id key's value: what the sweep adopted or replaced
				if name, ok := strings.CutPrefix(k, "plugin:"); ok && !strings.Contains(name, ":") {
					v, _ := h.GlobalDB.GetKV(k)
					fmt.Fprintf(&out, "%s %s\n", k, v)
				}
			}
			for _, s := range h.skillsReg.list() {
				if s.Source != skill.Builtin {
					fmt.Fprintf(&out, "skill %s %s %v\n", s.Name, h.sourceOf(string(s.Source), s.Dir), h.skillsReg.isEnabled(s.Name))
				}
			}
			for _, d := range h.agentDefs() {
				if d.Source != "builtin" {
					fmt.Fprintf(&out, "agent %s %s\n", d.Name, h.sourceOf(d.Source, d.Path))
				}
			}
			for _, c := range h.Commands.List() {
				if strings.HasSuffix(c.Path, ".md") {
					fmt.Fprintf(&out, "command %s %s\n", c.Name, h.sourceOf(string(c.Origin), c.Path))
				}
			}
			for _, w := range h.workflows() {
				if w.Source != "builtin" {
					fmt.Fprintf(&out, "workflow %s %s\n", w.Name, h.sourceOf(w.Source, w.Path))
				}
			}
			fmt.Fprintf(&out, "provides: %s\n", strings.Join(h.pluginProvides()["a"], " · "))
			h.errs.mu.Lock()
			for _, e := range h.errs.entries {
				fmt.Fprintf(&out, "error: %s: %s\n", e.source(), strings.ReplaceAll(e.msg, h.pluginsRoot(), "<plugins>"))
			}
			h.errs.mu.Unlock()
			for _, row := range h.goHealthRows() {
				if row.Name == "plugins" {
					fmt.Fprintf(&out, "health: %s\n", row.Detail)
				}
			}
			got := out.String()
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in:\n%s", n, got)
				}
			}
		})
	}
}

// TestClaudeSkillNameCollisions: a Claude plugin's skills, commands and
// agents load under their bare names, and only a name something else holds
// falls back to the plugin prefix. A renamed skill's /skills row shows
// base@plugin, and +attach names both forms.
func TestClaudeSkillNameCollisions(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := config.Dirs{ProjectDir: filepath.Join(tmp, "proj"), ConfigHome: filepath.Join(tmp, "cfg"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
	writeSkill(t, filepath.Join(dirs.ConfigHome, "skills"), "x", "the user's x", "")
	plugin := func(name string, files map[string]string) {
		root := filepath.Join(dirs.PluginsRoot(), "installed", name)
		files[filepath.Join(root, ".claude-plugin", "plugin.json")] = `{"name":"` + name + `"}`
		for p, src := range files {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	plugin("fix", map[string]string{
		filepath.Join(dirs.PluginsRoot(), "installed", "fix", "skills", "x", "SKILL.md"):    "---\ndescription: fix's x\n---\nbody",
		filepath.Join(dirs.PluginsRoot(), "installed", "fix", "skills", "solo", "SKILL.md"): "---\ndescription: a unique one\n---\nbody",
		filepath.Join(dirs.PluginsRoot(), "installed", "fix", "commands", "pr.md"):          "---\ndescription: draft it\n---\ndo it",
		filepath.Join(dirs.PluginsRoot(), "installed", "fix", "agents", "x.md"):             "---\ndescription: reviews\n---\nbe brief",
	})
	plugin("other", map[string]string{
		filepath.Join(dirs.PluginsRoot(), "installed", "other", "skills", "x", "SKILL.md"): "---\ndescription: other's x\n---\nbody",
		filepath.Join(dirs.PluginsRoot(), "installed", "other", "agents", "x.md"):          "---\ndescription: other agent\n---\nbe brief",
	})
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{
		{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
		{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
	}})
	h := NewHost(r, log, config.NewStore(), dirs)
	if err := r.Boot(t.Context(), []lua.BootStep{
		h.NamespacesStep(), h.PluginsStep(), h.SkillsUserHooksStep(), h.MarkdownCommandsStep(),
	}); err != nil {
		t.Fatal(err)
	}

	skills := map[string]skill.Skill{}
	for _, s := range h.skillsReg.list() {
		if s.Source == skill.User || s.Source == skill.Plugin {
			skills[s.Name] = s
		}
	}
	for name, desc := range map[string]string{"x": "the user's x", "fix-x": "fix's x", "other-x": "other's x", "solo": "a unique one", "pr": "draft it"} {
		if got, ok := skills[name]; !ok || got.Description != desc {
			t.Errorf("skill %s = %+v, want %q", name, got, desc)
		}
	}
	// the unique plugin skill answers to its bare name everywhere
	mine, _ := h.skillsIndexPrompt()
	if !strings.Contains(mine, "\n- solo: a unique one\n") {
		t.Errorf("index: %q", mine)
	}
	if _, ok := h.skillByName("solo"); !ok {
		t.Error("+solo does not resolve")
	}
	// a renamed skill's row shows base@plugin, its actions keep the real name
	rows := map[string]string{}
	for _, it := range h.skillItems() {
		rows[it.Run] = it.Text
	}
	for run, text := range map[string]string{
		"skills toggle x":       "x",
		"skills toggle fix-x":   "x@fix",
		"skills toggle other-x": "x@other",
		"skills toggle solo":    "solo",
	} {
		if rows[run] != text {
			t.Errorf("row %q = %q, want %q", run, rows[run], text)
		}
	}
	// the command loads bare as both a /command and a skill row
	if h.Commands.Lookup("pr") == nil || h.Commands.Lookup("fix-pr") != nil {
		t.Error("the plugin command did not load as /pr")
	}
	agents := map[string]bool{}
	for _, d := range h.agentDefs() {
		agents[d.Name] = true
	}
	if !agents["x"] || !agents["other-x"] {
		t.Errorf("agents: %v", agents)
	}
	var b strings.Builder
	h.attachSkill(&b, "fix-x")
	if !strings.Contains(b.String(), "with +fix-x (x from the fix plugin) in the message above") {
		t.Errorf("renamed attach note: %s", b.String())
	}
	b.Reset()
	h.attachSkill(&b, "solo")
	if !strings.Contains(b.String(), "with +solo in the message above") || strings.Contains(b.String(), "from the") {
		t.Errorf("bare attach note: %s", b.String())
	}
}

// TestPluginSettings: a plugin's zemi.config.add keys layer like the builtin
// ones (default < saved pick < init.lua < this run) across restarts and
// reloads, leave with their plugin without breaking init.lua's lines for
// them, and only the plugin registers its own, while it loads.
func TestPluginSettings(t *testing.T) {
	const (
		key    = "plugins.br.interval"
		reg    = `zemi.config.add("interval", { kind = "number", default = 30000, desc = "How often the branch is read, in ms." })`
		regStr = `zemi.config.add("interval", { kind = "string", default = "x", desc = "d" })`
		off    = "off" // the plugin turned off; "-" deletes it
	)
	type phase struct {
		plugin, user string // local/br/init.lua and the user's init.lua
		restart      bool   // a new zemi over the same dirs, else a reload
		pick         any    // a /config write after it
		after        string // user Lua run after it
	}
	for _, c := range []struct {
		name      string
		phases    []phase
		want, not []string
	}{
		{
			name: "a registered key reads its default and lists in schema()", phases: []phase{{plugin: reg, user: `
				for _, s in ipairs(zemi.config.schema()) do if s.path == "plugins.br.interval" then print(s.desc) end end`}},
			want: []string{"value 30000 default", "section Plugin br\n", "how often the branch is read, in ms\n"},
		},
		{
			name: "the plugin reads and watches its own key bare, user code does not",
			phases: []phase{{
				plugin: reg + `
					zemi.config.watch("interval", function(v) print("watched", v) end)
					zemi.command.add("rd", function() print("got", zemi.config.get("interval")) end, {})`,
				after: `zemi.config.set("plugins.br.interval", 10000) zemi.command.run("rd") print("bare", zemi.config.get("interval"))`,
			}},
			want: []string{"watched 10000\n", "got 10000\n", "bare nil\n"},
		},
		{
			name:   "init.lua sets it",
			phases: []phase{{plugin: reg, user: `zemi.config.set("plugins.br.interval", 10000)`}},
			want:   []string{"value 10000 init.lua"},
		},
		{
			name:   "a runtime pick outlives a restart",
			phases: []phase{{plugin: reg, pick: 84.5}, {plugin: reg, restart: true}},
			want:   []string{"value 84.5 saved", "config:plugins.br.interval\n"},
		},
		{
			name: "a replay with init.lua unchanged keeps the pick",
			phases: []phase{
				{plugin: reg, user: `zemi.config.set("plugins.br.interval", 10000)`, pick: 5000},
				{plugin: reg, user: `zemi.config.set("plugins.br.interval", 10000)`},
			},
			want: []string{"value 5000 this run"},
		},
		{
			name:   "a replay after the kind changed drops the pick",
			phases: []phase{{plugin: reg, pick: 5000}, {plugin: regStr}},
			want:   []string{"value x default"},
			not:    []string{"config:plugins.br.interval"},
		},
		{
			name: "a plugin turned off takes its key, skips init.lua's set and watch and keeps the pick",
			phases: []phase{
				{plugin: reg, user: `zemi.config.set("plugins.br.interval", 10000)`, pick: 5000},
				{plugin: off, user: `zemi.config.set("plugins.br.interval", 10000) print("watch", zemi.config.watch("plugins.br.interval", print))`},
				{plugin: off, user: `zemi.config.set("plugins.br.interval", 10000) print("watch", zemi.config.watch("plugins.br.interval", print))`, restart: true},
			},
			want: []string{"watch 1\n", "value <nil>", "watch\n", "config:plugins.br.interval\n"},
			not:  []string{"error"},
		},
		{
			name:   "a deleted plugin's picks go",
			phases: []phase{{plugin: reg, pick: 5000}, {plugin: "-", restart: true}},
			not:    []string{"config:plugins.br.interval"},
		},
		{
			name:   "a key a loaded plugin did not register raises",
			phases: []phase{{plugin: reg, user: `print(pcall(zemi.config.set, "plugins.br.intervl", 1))`}},
			want:   []string{"false", "unknown config key: plugins.br.intervl"},
		},
		{
			name: "only a plugin registers, its own keys, while it loads, a default the kind and choices take",
			phases: []phase{{
				plugin: `print(pcall(zemi.config.add, { path = "plugins.br.x", kind = "number", default = 1, desc = "d" }))
					print(pcall(zemi.config.add, "ui.theme", { kind = "string", default = "x", desc = "d" }))
					print(pcall(zemi.config.add, "Interval", { kind = "string", default = "x", desc = "d" }))
					print(pcall(zemi.config.add, "units", { kind = "string", default = "kelvin", choices = { "metric" }, desc = "d" }))
					print(pcall(zemi.config.add, "n", { kind = "number", default = "1", desc = "d" }))
					print(pcall(zemi.config.add, "nan", { kind = "number", default = 0/0, desc = "d" }))
					print(pcall(zemi.config.add, "i", { kind = "int", default = 1, desc = "d" }))
					zemi.command.add("late", function() print(pcall(zemi.config.add, "late", { kind = "number", default = 1, desc = "d" })) end, {})`,
				user:  `print(pcall(zemi.config.add, "x", { kind = "number", default = 1, desc = "d" }))`,
				after: `zemi.command.run("late")`,
			}},
			want: []string{
				"bad argument #1 to (anonymous) (string expected, got table)",
				`zemi.config.add: key "ui.theme" must be a lowercase letter`, `key "Interval" must be`,
				"plugins.br.units expects metric", "plugins.br.n expects number", "plugins.br.nan expects number",
				"kind must be one of string, number, bool, list or duration",
				"zemi.config.add: register settings while the config loads",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dirs := config.Dirs{ProjectDir: filepath.Join(tmp, "proj"), ConfigHome: filepath.Join(tmp, "cfg"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
			write := func(p, src string) {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			var h *Host
			var steps []lua.BootStep
			for _, p := range c.phases {
				disabled := filepath.Join(dirs.PluginsRoot(), "disabled")
				switch p.plugin {
				case "-":
					_ = os.RemoveAll(filepath.Join(dirs.PluginsRoot(), "local", "br"))
				case off:
					write(disabled, "br\n")
				default:
					_ = os.Remove(disabled)
					write(filepath.Join(dirs.PluginsRoot(), "local", "br", plugin.InitFile), p.plugin)
				}
				write(filepath.Join(dirs.ConfigHome, "init.lua"), p.user)
				if h == nil || p.restart {
					if h != nil {
						h.Close()
					}
					log := slog.New(slog.DiscardHandler)
					r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{
						{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
						{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
					}})
					r.SetPrintHook(func(line string) { fmt.Fprintf(&out, "%s\n", line) })
					h = NewHost(r, log, config.NewStore(), dirs)
					t.Cleanup(h.Close)
					steps = []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(), h.PluginsStep(), lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser)}
					if err := r.Boot(t.Context(), steps); err != nil {
						t.Fatal(err)
					}
					h.KeepPicks.Store(true)
				} else if err := h.ForceReload(t.Context(), steps); err != nil {
					t.Fatal(err)
				}
				if p.pick != nil {
					if err := h.SetConfig(key, p.pick); err != nil {
						t.Fatal(err)
					}
				}
				runUser(t, h.RT, p.after)
			}
			v, _ := h.Settings.Get(key)
			fmt.Fprintf(&out, "value %v %s\n", cmp.Or(fmt.Sprint(v), "<nil>"), h.cfgSource(key))
			for _, it := range h.configItems("plugins") {
				if it.Text == key {
					fmt.Fprintf(&out, "section %s\n", it.Section)
				}
			}
			keys, _ := h.GlobalDB.KeysKV("config:")
			for _, k := range keys {
				fmt.Fprintf(&out, "%s\n", k)
			}
			h.errs.mu.Lock()
			for _, e := range h.errs.entries {
				fmt.Fprintf(&out, "error: %s: %s\n", e.source(), e.msg)
			}
			h.errs.mu.Unlock()
			got := out.String()
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in:\n%s", n, got)
				}
			}
		})
	}
}

// TestPluginCapVocabulary pins the gate's subject mapping to the manifest's
// vocabulary in both directions, so a word one adds and the other does not
// cannot drift.
func TestPluginCapVocabulary(t *testing.T) {
	for _, c := range pluginCaps {
		probe := c.prefix
		if strings.HasSuffix(probe, ".") {
			probe += "x"
		}
		if got := pluginCap(probe); got != c.cap {
			t.Errorf("pluginCap(%q) = %q, want its row's %q", probe, got, c.cap)
		}
		if !slices.Contains(plugin.Uses, c.cap) {
			t.Errorf("%q maps to %q, which is no capability in plugin.Uses", c.prefix, c.cap)
		}
	}
	for _, c := range []struct {
		subject string
		cap     string
	}{
		{"lua.job.start", "job"},
		{"lua.agent.send", "agent"},
		{"lua.agent.run", "agent"},
		{"lua.workflow.run", "workflow"},
		{"lua.fs.read", ""},  // a read is not declarable
		{"zemi.env.get", ""}, // nor is the plugin's own env
	} {
		if got := pluginCap(c.subject); got != c.cap {
			t.Errorf("pluginCap(%q) = %q, want %q", c.subject, got, c.cap)
		}
	}
	// every word the manifest may declare is one a subject maps to, or one
	// the gate's other halves check ("perm" gates a permission.request
	// handler and zemi.perm.ask, not a call subject)
	mapped := map[string]bool{"job": true, "perm": true}
	for _, c := range pluginCaps {
		mapped[c.cap] = true
	}
	for _, u := range plugin.Uses {
		if !mapped[u] {
			t.Errorf("plugin.Uses names %q and no subject maps to it", u)
		}
	}
}

// pluginFixture writes the local plugin a of the plugins root: its
// .zemi/init.lua and, when uses is not empty, the manifest declaring that
// uses array. It returns the plugin's root.
func pluginFixture(t *testing.T, h *Host, uses string) string {
	t.Helper()
	root := filepath.Join(h.pluginsRoot(), "local", "a")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, plugin.InitFile), "-- the test plugin\n")
	if uses != "" {
		write(filepath.Join(root, plugin.ManifestFile), `{"uses": `+uses+`}`)
	}
	return root
}

// newTestHostPlugin is newTestHost with the plugin a of pluginFixture booted
// through the plugins step, so a call its manifest declares is what settles it.
func newTestHostPlugin(t *testing.T, uses string) (*Host, *lua.Runtime) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	dirs := config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir(), StateHome: t.TempDir()}
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
	h := NewHost(r, log, config.NewStore(), dirs)
	h.ReleaseBuilder() // tests stub the model and boot no config
	t.Cleanup(h.Close)
	pluginFixture(t, h, uses)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.PluginsStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// TestPluginUses: a plugin calls what its own manifest declares and nothing
// else, so enabling it grants the declared set. No rule widens that, a rule of
// the user's still narrows it, a subject no capability covers keeps its
// default, and every write of a read-only turn asks.
func TestPluginUses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	for _, c := range []struct {
		name string
		uses string // the manifest's uses array; "" for a plugin with none
		stub bool   // h.modelFn answers "pong"
		send bool   // the call starts a turn of the agent
		call string // %project% is the project dir, %plugin% the plugin's own root
		err  string // the substring the call raises; "" when it goes through
	}{
		{
			name: "a declared call settles", uses: `["http"]`,
			call: fmt.Sprintf(`local res, err = zemi.http.get(%q):wait()
				assert(res ~= nil and res.body == "ok", tostring(err))`, srv.URL+"/x"),
		},
		{
			name: "an undeclared call is refused, not asked",
			call: fmt.Sprintf(`zemi.http.get(%q)`, srv.URL+"/x"),
			err:  `plugin a declares no "http"; add "http" to uses in .zemi/plugin.json`,
		},
		{
			name: "a plugin that declares nothing cannot call a model",
			call: `zemi.model.ask({ prompt = "ping" })`,
			err:  `plugin a declares no "model"`,
		},
		{
			name: "a declared model call settles", uses: `["model"]`, stub: true,
			call: `local text, err = zemi.model.ask({ prompt = "ping" }):wait()
				assert(text == "pong", tostring(err))`,
		},
		{
			name: "a job settles with job declared", uses: `["job"]`,
			call: `assert(zemi.job.start({ cmd = "true" }):wait(5000).code == 0, "the job")`,
		},
		{
			name: "a declared agent send settles", uses: `["agent"]`, stub: true, send: true,
			call: `assert(zemi.agent.send("hi"), "the send")`,
		},
		{
			name: "an undeclared agent send is refused",
			call: `zemi.agent.send("hi")`,
			err:  `plugin a declares no "agent"`,
		},
		{
			name: "a declared agent run settles", uses: `["agent"]`, stub: true,
			call: `local text, err = zemi.agent.run({ prompt = "ping" }):wait()
				assert(text == "pong", tostring(err))`,
		},
		{
			name: "an undeclared agent run is refused",
			call: `zemi.agent.run({ prompt = "ping" })`,
			err:  `plugin a declares no "agent"`,
		},
		{
			name: "a declared workflow run settles", uses: `["workflow"]`, stub: true,
			call: `local res, err = zemi.workflow.run({ script = "return 'pong'" }):wait()
				assert(res == "pong", tostring(err))`,
		},
		{
			name: "an undeclared workflow run is refused",
			call: `zemi.workflow.run({ script = "return 1" })`,
			err:  `plugin a declares no "workflow"`,
		},
		{
			name: "a declared write settles", uses: `["fs.write"]`,
			call: `assert(zemi.fs.write("%project%/notes.txt", "x") == true, "the project write")`,
		},
		{
			name: "a subject no capability covers keeps its default",
			call: `assert(zemi.fs.read("%plugin%/.zemi/init.lua"):find("the test plugin"), "the project read")`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// the plugin root sits inside the project, as it does for a zemi
			// in $HOME, so a write to its own code is not refused at
			// resolution and it is the gate that refuses it
			dir := t.TempDir()
			dirs := config.Dirs{ProjectDir: dir, DataHome: filepath.Join(dir, "data"), StateHome: filepath.Join(dir, "state")}
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
			h := NewHost(r, log, config.NewStore(), dirs)
			t.Cleanup(h.Close)
			own := pluginFixture(t, h, c.uses)
			if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.PluginsStep()}); err != nil {
				t.Fatal(err)
			}
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			if c.stub {
				seedSession(t, h)
				replyStub(t, h, "pong")
			}
			err := r.ExecuteChunk(t.Context(), strings.NewReplacer("%project%", dir, "%plugin%", own).Replace(c.call), filepath.Join(own, ".zemi", "init.lua"), lua.OriginPlugin)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("the call did not settle: %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("err = %v, want %q", err, c.err)
			}
			// refused or settled, never a prompt: a Lua call has no overlay
			if len(ui.asked) != 0 {
				t.Errorf("the call asked the user: %+v", ui.asked)
			}
			// a send starts a turn: let it log and end before the
			// state dir goes (a late write fails TempDir's cleanup)
			if c.send {
				waitFor(t, func() error {
					// a plugin send is attached text only: its echo row is the
					// log's record of what the model read
					rows, err := h.DB.LogKindBack(kindEcho, 50)
					if err != nil || !slices.ContainsFunc(rows, func(r store.LogEntry) bool { return rowTool(r) == "attach" }) {
						return errors.New("no message yet")
					}
					return nil
				})
				waitIdle(t, h)
			}
		})
	}
}

// TestPluginUsesLinkedInstall re-proves the gate on a marketplace install: an
// installed plugin is a symlink into its marketplace material, and the gate,
// require and state must name the plugin from the material its code resolves
// to, not the link.
func TestPluginUsesLinkedInstall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	for _, c := range []struct {
		name, uses string
		want       string // in the boot error; "" for a clean boot
	}{
		{name: "an undeclared call fires the gate", want: `plugin a declares no "http"`},
		{name: "a declared call boots", uses: `["http"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dirs := config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir(), StateHome: t.TempDir()}
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
			h := NewHost(r, log, config.NewStore(), dirs)
			t.Cleanup(h.Close)
			mat := filepath.Join(dirs.PluginsRoot(), "marketplaces", "m", "p")
			manifest := "{}"
			if c.uses != "" {
				manifest = `{"uses": ` + c.uses + `}`
			}
			files := map[string]string{
				filepath.Join(mat, plugin.InitFile): `require("./extra").ran = true
					zemi.state.set("k", 1)
					zemi.http.get("` + srv.URL + `/x")`,
				filepath.Join(mat, ".zemi", "extra.lua"): `return {}`,
				filepath.Join(mat, plugin.ManifestFile):  manifest,
			}
			for file, body := range files {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "installed"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(mat, filepath.Join(dirs.PluginsRoot(), "installed", "a")); err != nil {
				t.Fatal(err)
			}
			err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.PluginsStep()})
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("Boot err = %v, want it to name %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Boot: %v", err)
			}
			if v, _ := h.GlobalDB.GetKV("plugin:a:k"); v != "1" {
				t.Errorf("state key plugin:a:k = %s", v)
			}
			if got := h.PluginOf(filepath.Join(mat, plugin.InitFile)); got != "a" {
				t.Errorf("PluginOf(%s) = %q, want a", mat, got)
			}
		})
	}
}

// TestPluginSidecarGrant: a linked install whose material carries no
// manifest of its own loads the marketplace entry its row carries (the
// sidecar), so its mcp servers and hooks grant. A row looked up by the
// material's directory name instead of the install name is never found, and
// the plugin silently grants none of it.
func TestPluginSidecarGrant(t *testing.T) {
	t.Parallel()
	const entry = `{"name": "Sub One",
		"mcpServers": {"srv": {"command": "srv"}},
		"hooks": {"PostToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "fmt.sh"}]}]}}`
	for _, c := range []struct {
		name      string
		manifest  string // the sidecar: the entry manifest the row carries
		wantSrv   bool
		wantHooks int
	}{
		{
			name: "the entry manifest in the row grants hooks and mcp", manifest: entry,
			wantSrv: true, wantHooks: 1,
		},
		{name: "a row without a manifest grants none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dirs := config.Dirs{ProjectDir: filepath.Join(tmp, "proj"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
			material := filepath.Join(dirs.PluginsRoot(), "marketplaces", "m", "sub")
			row := `{"installs": {"a": {"marketplace": "m", "entry": "Sub One", "kind": "link", "id": "row-1"`
			if c.manifest != "" {
				row += `, "manifest": ` + c.manifest
			}
			files := map[string]string{
				filepath.Join(dirs.PluginsRoot(), "marketplaces.json"): row + `}}}`,
			}
			for p, src := range files {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// the material holds neither manifest: the entry in the row is
			// the only manifest the install has
			if err := os.MkdirAll(material, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "installed"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(material, filepath.Join(dirs.PluginsRoot(), "installed", "a")); err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
			h := NewHost(r, log, config.NewStore(), dirs)
			h.Inert = true
			t.Cleanup(h.Close)
			if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.PluginsStep()}); err != nil {
				t.Fatal(err)
			}
			h.mcp.mu.Lock()
			srv := h.mcp.servers["a-srv"]
			h.mcp.mu.Unlock()
			if (srv != nil) != c.wantSrv {
				t.Fatalf("mcp server a-srv present = %v, want %v", srv != nil, c.wantSrv)
			}
			if srv != nil {
				if srv.spec.Command != "srv" {
					t.Errorf("a-srv command = %q, want srv", srv.spec.Command)
				}
				if srv.spec.Env["CLAUDE_PLUGIN_ROOT"] != material || srv.spec.Env["CLAUDE_PLUGIN_DATA"] != h.pluginDataDir("a") {
					t.Errorf("a-srv env = %v, want root %s and data %s", srv.spec.Env, material, h.pluginDataDir("a"))
				}
				if !h.isClaudePlugin(material) {
					t.Error("the material is not granted as a Claude layout")
				}
			}
			hooks := h.hookReg.handlers(EventToolPost)
			if len(hooks) != c.wantHooks {
				t.Fatalf("PostToolUse hooks = %d, want %d", len(hooks), c.wantHooks)
			}
			if c.wantHooks == 1 && (hooks[0].command != "fmt.sh" || hooks[0].root != material) {
				t.Errorf("hook = %+v, want fmt.sh at %s", hooks[0], material)
			}
		})
	}
}

// TestPluginOfGrantRoots: a material nested in another plugin's attributes
// to its own plugin, by longest root first, and a root perm.RealPath cannot
// resolve ("" in the grant) attributes to nothing, not to every path.
func TestPluginOfGrantRoots(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := config.Dirs{ProjectDir: filepath.Join(tmp, "proj"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
	mkt := filepath.Join(dirs.PluginsRoot(), "marketplaces", "m")
	files := map[string]string{
		filepath.Join(mkt, ".zemi", "init.lua"):                 "-- outer\n",
		filepath.Join(mkt, "outer.lua"):                         "-- the outer plugin's file\n",
		filepath.Join(mkt, "plugins", "b", ".zemi", "init.lua"): "-- inner\n",
		filepath.Join(mkt, "plugins", "b", "inner.lua"):         "-- the inner plugin's file\n",
	}
	for p, src := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"a": mkt,
		"b": filepath.Join(mkt, "plugins", "b"),
	} {
		if err := os.Symlink(target, filepath.Join(dirs.PluginsRoot(), "installed", name)); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
	h := NewHost(r, log, config.NewStore(), dirs)
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.PluginsStep()}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path, want string }{
		// the grant is name-sorted, a's root first: a prefix-first match
		// would attribute the inner plugin's file to a
		{"a nested material names its own plugin", filepath.Join(mkt, "plugins", "b", "inner.lua"), "b"},
		{"the outer material names the outer plugin", filepath.Join(mkt, "outer.lua"), "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := h.PluginOf(c.path); got != c.want {
				t.Errorf("PluginOf(%s) = %q, want %q", c.path, got, c.want)
			}
		})
	}
	// the grant as perm.RealPath's failure leaves it: "" first, where a
	// bare prefix test claims every absolute path
	h.pluginGrant.Store(&pluginGrant{roots: []string{"", mkt}, names: []string{"looped", "a"}})
	for _, c := range []struct{ name, path, want string }{
		{"an unresolvable root attributes nothing, not every absolute path", filepath.Join(tmp, "elsewhere", "x"), ""},
		{"an unresolvable root does not shadow a resolvable one", filepath.Join(mkt, "outer.lua"), "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := h.PluginOf(c.path); got != c.want {
				t.Errorf("PluginOf(%s) = %q, want %q", c.path, got, c.want)
			}
		})
	}
	// the Claude-layout expansion reads the same grant: the nested
	// material expands against its own root, an unresolvable one against none
	inner := filepath.Join(mkt, "plugins", "b")
	h.pluginGrant.Store(&pluginGrant{
		roots:  []string{mkt, inner, ""},
		names:  []string{"a", "b", "looped"},
		claude: set.Of(mkt, inner, ""),
	})
	for _, c := range []struct{ name, path, want string }{
		{"the inner material expands against its own root", filepath.Join(inner, "cmd", "x.sh"), inner},
		{"the outer material expands against the outer root", filepath.Join(mkt, "cmd", "x.sh"), mkt},
		{"an unresolvable claude root expands against nothing", filepath.Join(tmp, "elsewhere", "cmd", "x.sh"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := h.claudeRootOf(c.path); got != c.want {
				t.Errorf("claudeRootOf(%s) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// TestPluginToolCall: the model's call of a Lua tool a plugin registered is
// gated like any tool: it asks in ask mode, runs in allow mode, and a
// read-only run may call none of them.
func TestPluginToolCall(t *testing.T) {
	for _, c := range []struct {
		name     string
		ask      bool
		readOnly bool
		asked    bool
		denied   string // a refusal the result names, "" when it settles
	}{
		{name: "an ask policy asks", ask: true, asked: true},
		{name: "no policy runs"},
		{name: "a read-only run may not call it", readOnly: true, denied: "read-only agent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHostPlugin(t, `["tool"]`)
			ui := &fakeUI{askAllowed: true, askBy: tools.ByUser}
			h.SetUIHooks(ui)
			if err := chunkAs(t.Context(), h, r, lua.OriginPlugin, `zemi.tool.add({ name = "ping", description = "d", execute = function() return "pong" end })`); err != nil {
				t.Fatal(err)
			}
			if c.ask {
				askEverything(t, r)
			}
			pipe := h.PipelineFor()
			if c.readOnly {
				pipe.ReadOnly = func() bool { return true }
			}
			resp, err := h.luaFantasyTools(pipe)[0].Run(t.Context(), fantasy.ToolCall{Input: "{}"})
			if c.denied != "" {
				if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.denied) {
					t.Fatalf("resp = %+v, %v; want %q", resp, err, c.denied)
				}
				return
			}
			if err != nil || resp.Content != "pong" {
				t.Fatalf("resp = %+v, %v; want pong", resp, err)
			}
			if got := len(ui.asked) > 0; got != c.asked {
				t.Errorf("asked = %v, want %v: %+v", got, c.asked, ui.asked)
			}
		})
	}
}

// TestPickPluginsLinks walks the loader's link rule: an installed link into
// the plugins tree runs, one pointing out does not, a dangling one names
// itself, and a local link gets its own advice.
func TestPickPluginsLinks(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "plugins")
	inside := filepath.Join(root, "marketplaces", "m", "plugins", "one")
	os.MkdirAll(inside, 0o755)
	os.WriteFile(filepath.Join(inside, plugin.InitFile), nil, 0o644)
	outside := filepath.Join(tmp, "elsewhere")
	os.MkdirAll(outside, 0o755)

	link := func(name, target string) {
		t.Helper()
		os.MkdirAll(filepath.Join(root, "installed"), 0o755)
		if err := os.Symlink(target, filepath.Join(root, "installed", name)); err != nil {
			t.Fatal(err)
		}
	}
	link("in", inside)
	link("out", outside)
	link("gone", filepath.Join(root, "marketplaces", "m", "plugins", "deleted"))
	os.MkdirAll(filepath.Join(root, "local"), 0o755)
	os.Symlink(outside, filepath.Join(root, "local", "loc"))
	os.Symlink(inside, filepath.Join(root, "local", "loc-in"))

	for _, c := range []struct {
		name string
		p    plugin.Plugin
		want string // in the refusal, empty for a run
	}{
		{name: "an installed link inside the tree runs", p: plugin.Plugin{Enabled: true, Name: "in", Dir: filepath.Join(root, "installed", "in")}},
		{name: "an installed link out of the tree is refused", p: plugin.Plugin{Enabled: true, Name: "out", Dir: filepath.Join(root, "installed", "out")}, want: "a link out of the plugins tree"},
		{name: "a dangling link names itself", p: plugin.Plugin{Enabled: true, Name: "gone", Dir: filepath.Join(root, "installed", "gone")}, want: "a dangling link"},
		{name: "a local link out of the tree gets local advice", p: plugin.Plugin{Enabled: true, Name: "loc", Dir: filepath.Join(root, "local", "loc"), Local: true}, want: "keep it in " + filepath.Join(root, "local")},
		{name: "a local link inside the tree runs", p: plugin.Plugin{Enabled: true, Name: "loc-in", Dir: filepath.Join(root, "local", "loc-in"), Local: true}},
	} {
		run, refused := pickPlugins([]plugin.Plugin{c.p}, root)
		if c.want == "" {
			if len(run) != 1 || len(refused) != 0 {
				t.Errorf("%s: run %v, refused %+v, want it to run", c.name, run, refused)
			}
			continue
		}
		if len(run) != 0 || len(refused) != 1 || !strings.Contains(refused[0].err.Error(), c.want) {
			t.Errorf("%s: run %v, refused %+v, want %q", c.name, run, refused, c.want)
		}
	}
}
