package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestPluginsStep boots plugins laid out under installed/ and local/, then
// user init.lua, and reads what ran, the step's error, the zemi.state keys
// and the /health row.
func TestPluginsStep(t *testing.T) {
	tool := func(name string) string {
		return `zemi.tool.add({ name = "` + name + `", description = "d", execute = function() return "" end })`
	}
	for _, c := range []struct {
		name    string
		plugins map[string]string // under the plugins root
		user    string            // init.lua
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
			want: []string{"plugin b: ", "command /dup is already registered by plugin a", "plugin c: ", `tool "dup_tool" is already registered by plugin a`},
			not:  []string{"plugin a: "},
		},
		{
			name: "state and principal are per plugin, a gone plugin's state goes",
			plugins: map[string]string{
				"local/a/init.lua": `zemi.state.set("k", "a") print(select(2, pcall(zemi.job.start, { cmd = "true" })))`,
				"local/b/init.lua": `zemi.state.set("k", "b") print(zemi.state.get("k"))`,
			},
			seed: []string{"plugin:gone:k", "plugin:a:old"},
			want: []string{"plugin:a:k\n", "plugin:a:old\n", "plugin:b:k\n", "plugin|b\n", `principal = "plugin:a"`},
			not:  []string{"plugin:gone:k"},
		},
		{
			name: "a setup() that init.lua calls runs as the plugin",
			plugins: map[string]string{"local/p/lua/p/init.lua": `return { setup = function()
				print(tostring(pcall(zemi.config.set, "permissions.mode", "yolo")))
				zemi.keymap.set("ctrl+g", function() zemi.state.set("k", 1) end)
			end }`},
			user: `require("p").setup()`,
			want: []string{"plugin|false\n", "plugin:p:k\n"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dirs := config.Dirs{ProjectDir: tmp, ConfigHome: filepath.Join(tmp, "cfg"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
			files := map[string]string{filepath.Join(dirs.ConfigHome, "init.lua"): c.user}
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
				lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser)}); err != nil {
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
