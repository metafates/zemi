package hostapi

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skills"
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
		maps.Copy(files, map[string]string{
			root + "skills/" + name + "/SKILL.md": "---\nname: " + name + "\ndescription: d\n---\nbody",
			root + "agents/" + name + ".md":       "---\ndescription: d\n---\nbe brief",
			root + "commands/" + name + ".md":     "do it",
			root + "workflows/" + name + ".lua":   "-- d\nreturn 1",
		})
		return files
	}
	for _, c := range []struct {
		name    string
		plugins map[string]string // under the plugins root
		user    string            // init.lua
		cfg     map[string]string // more files under the config home
		seed    []string          // zemi.state keys set before boot
		want    []string
		not     []string
	}{
		{
			name: "sorted by name, disabled skipped",
			plugins: map[string]string{
				"installed/a/init.lua": `print("a")`, "local/b/init.lua": `print("b")`,
				"installed/c/init.lua": `print("c")`, "disabled": "c\n",
			},
			want: []string{"plugin|a\nplugin|b\n", "health: 2 plugins\n"},
			not:  []string{"plugin|c"},
		},
		{
			name:    "a local plugin named like an installed one",
			plugins: map[string]string{"installed/x/init.lua": `print("installed")`, "local/x/init.lua": `print("local")`},
			want:    []string{"plugin|installed", "plugin x: an installed plugin has this name"},
			not:     []string{"plugin|local"},
		},
		{
			name:    "a failing plugin does not stop the rest",
			plugins: map[string]string{"local/a/init.lua": `error("broken")`, "local/b/init.lua": `print("b")`},
			want:    []string{"plugin a: ", "broken", "plugin|b", "health: 2 plugins · 1 failed"},
		},
		{
			name: "two plugins collide on a command and a tool",
			plugins: map[string]string{
				"local/a/init.lua": `zemi.command.add("dup", function() end, {}) zemi.command.add("dup", function() end, {}) ` + tool("dup_tool"),
				"local/b/init.lua": `zemi.command.add("dup", function() end, {})`,
				"local/c/init.lua": tool("dup_tool"),
			},
			want: []string{"plugin b: ", "command /dup is already registered by plugin a, not b", "plugin c: ", `tool "dup_tool" is already registered by plugin a, not c`},
			not:  []string{"plugin a: "},
		},
		{
			name: "state and principal are per plugin, a gone plugin's state goes",
			plugins: map[string]string{
				"local/a/init.lua": `zemi.state.set("k", "a") print(select(2, pcall(zemi.job.start, { cmd = "true" })))`,
				"local/b/init.lua": `zemi.state.set("k", "b") print(zemi.state.get("k")) print(select(2, pcall(zemi.job.start, { cmd = "true" })))`,
			},
			seed: []string{"plugin:gone:k", "plugin:a:old"},
			want: []string{"plugin:a:k\n", "plugin:a:old\n", "plugin:b:k\n", "plugin|b\n", "true: permission denied", "true: permission ask required"},
			not:  []string{"plugin:gone:k"},
		},
		{
			name:    "a failed scan keeps every plugin's state",
			plugins: map[string]string{"local/a/init.lua": "", "disabled/x": ""},
			seed:    []string{"plugin:a:saved"},
			want:    []string{"is a directory", "plugin:a:saved\n"},
		},
		{
			name: "a setup() that init.lua calls runs as the plugin, through a tail call and into another plugin",
			plugins: map[string]string{
				"local/p/lua/p/init.lua": `return {
					setup = function()
						print(tostring(pcall(zemi.config.set, "permissions.mode", "yolo")))
						require("b").put("via-setup")
						zemi.keymap.set("ctrl+g", function() require("b").put("via-keymap") end)
					end,
					tail = function(f) return f("yolo") end,
				}`,
				"local/b/lua/b/init.lua": `return { put = function(k) zemi.state.set(k, 1) end }`,
			},
			user: `require("p").setup() print(pcall(require("p").tail, function(m) zemi.permission.mode.set(m) end))`,
			want: []string{"plugin|false\n", "plugin:p:via-setup\n", "plugin:p:via-keymap\n", "user|false", "mode: auto\n"},
			not:  []string{"plugin:b:"},
		},
		{
			name: "a plugin's skills, agents, commands and workflows load, a disabled plugin's do not, the user's win",
			plugins: res(res(res(map[string]string{"installed/a/skills/s/zemi-skill.lua": `print("hook")`, "disabled": "off\n"},
				"installed/a/", "s"), "installed/a/", "mine"), "local/off/", "gone"),
			cfg: res(map[string]string{}, "", "mine"),
			want: []string{
				"skill s plugin a true\n", "skill mine user true\n", "agent s plugin a\n", "agent mine user\n",
				"command s plugin a\n", "command mine user\n", "workflow s plugin a\n", "workflow mine user\n",
				"read s allow ask\n", // the skill tool reads it; plugin Lua gets no jail
				"provides: command /s · skill s · agent s · workflow s\n",
			},
			not: []string{"gone", "plugin|hook", "mine plugin"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			log := logging.New(t.TempDir())
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{
				{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
				{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
			}})
			var out strings.Builder
			r.SetPrintHook(func(origin, line string) { fmt.Fprintf(&out, "%s|%s\n", origin, line) })
			h := NewHost(r, log, config.NewStore(), dirs)
			// scoped to one plugin, so a case can tell the principals apart
			h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Deny, Principal: "plugin:a"})
			for _, k := range c.seed {
				if err := h.GlobalDB.SetKV(k, "1"); err != nil {
					t.Fatal(err)
				}
			}
			step := h.PluginsStep()
			run := step.Run
			step.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
				fmt.Fprintf(&out, "step: %v\n", run(ctx, r, g))
				return nil
			}
			if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), step,
				lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser),
				h.SkillsUserHooksStep(), h.MarkdownCommandsStep()}); err != nil {
				t.Fatal(err)
			}
			if b := h.Keymaps.Lookup("", "ctrl+g"); b != nil {
				if err := h.RunRHS(context.Background(), b); err != nil {
					t.Fatal(err)
				}
			}
			keys, _ := h.GlobalDB.KeysKV("")
			for _, k := range keys {
				fmt.Fprintf(&out, "%s\n", k)
			}
			fmt.Fprintf(&out, "mode: %s\n", h.PermMode())
			for _, s := range h.skillsReg.list() {
				if s.Source != skills.Builtin {
					fmt.Fprintf(&out, "skill %s %s %v\n", s.Name, h.sourceOf(string(s.Source), s.Dir), h.skillsReg.isEnabled(s.Name))
				}
				if s.Source == skills.Plugin {
					md := filepath.Join(s.Dir, "SKILL.md")
					fmt.Fprintf(&out, "read %s %s %s\n", s.Name, h.fsDefault("fs.read", md), h.fsDefault("lua.fs.read", md))
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
			fmt.Fprintf(&out, "provides: %s\n", h.pluginProvides("a"))
			for _, row := range h.goHealthRows() {
				if row.name == "plugins" {
					fmt.Fprintf(&out, "health: %s\n", row.detail)
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
