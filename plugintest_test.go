package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/metafates/zemi/runtime"
)

// TestPluginsTest runs zemi plugins test on testdata/plugins/demo, whose
// test files hold the passing cases and one case per kind of failure, and
// on plugins a row writes; it reads the output and the exit status, and
// checks that nothing outside the run's temp dir changed.
func TestPluginsTest(t *testing.T) {
	demo, err := filepath.Abs("testdata/plugins/demo")
	if err != nil {
		t.Fatal(err)
	}
	// the user's config, which a test zemi must never load
	home := t.TempDir()
	t.Setenv("HOME", home)
	for env, dir := range map[string]string{"ZEMI_CONFIG_HOME": "config", "ZEMI_DATA_HOME": "data", "ZEMI_STATE_HOME": "state", "ZEMI_CACHE_HOME": "cache"} {
		t.Setenv(env, filepath.Join(home, dir))
	}
	for path, body := range map[string]string{
		"config/init.lua":                   `zemi.command.add("from-init", function() end)`,
		".agents/skills/from-home/SKILL.md": "---\nname: from-home\ndescription: x\n---\nx\n",
		"data/plugins/local/named/init.lua": "zemi.command.add('named', function() end)\n",
		"data/plugins/local/named/tests/n_test.lua": `local test = zemi.test
test("found by name", function() assert(test.run("named")) end)`,
	} {
		writeFile(t, filepath.Join(home, path), body)
	}
	demoSum, homeSum := treeSum(t, demo), treeSum(t, home)
	old := caseTimeout
	caseTimeout = 300 * time.Millisecond
	t.Cleanup(func() { caseTimeout = old })
	many := `local test = zemi.test
for i = 1, 300 do test("case " .. i, function() end) end
`
	// the plugin and test zemi-customize shows, as an agent copies them
	skill, err := fs.ReadFile(runtime.Skills, "skills/zemi-customize/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	example := map[string]string{}
	for path, heading := range map[string]string{"init.lua": "`init.lua`:\n", "lua/branch/init.lua": "`lua/branch/init.lua`:", "tests/branch_test.lua": "`tests/branch_test.lua` for the plugin above:"} {
		_, after, _ := strings.Cut(string(skill), heading)
		_, block, _ := strings.Cut(after, "```lua\n")
		example[path], _, _ = strings.Cut(block, "```")
	}

	for _, c := range []struct {
		name   string
		plugin map[string]string // a plugin to write; a value "-> x" is a link to x; none runs demo
		named  string            // its name, cp when empty
		args   []string          // <plugin> stands for its dir
		limit  uint64            // RLIMIT_NOFILE during the run
		cancel bool              // interrupted before it starts
		code   int
		want   string // (T) stands for the time a run took
	}{
		{
			name: "the passing cases pass", args: []string{"<plugin>", "pass_test"}, code: 0,
			want: "22 passed (2 files, (T))\n",
		},
		{name: "eq on a string", args: []string{"<plugin>", "fail_test.lua: eq on a string"}, code: 1, want: `FAIL tests/fail_test.lua:7: eq on a string
  tests/fail_test.lua:11: got "Oslo: 21C, Sunny", want "Oslo: 21C, sunny"
  print: asking wttr.in
0 passed, 1 failed (2 files, (T))
`},
		{name: "eq on a table", args: []string{"<plugin>", "eq on a table"}, code: 1, want: `FAIL tests/fail_test.lua:14: eq on a table
  tests/fail_test.lua:15: [1]: got "main", want "fix-login"
    got  { "main" }
    want { "fix-login" }
0 passed, 1 failed (2 files, (T))
`},
		{name: "an unstubbed call", args: []string{"<plugin>", "no stub"}, code: 1, want: `FAIL tests/fail_test.lua:18: a call no stub answers
  tests/fail_test.lua:19: got nil, want "Oslo: 21C, Sunny"
  test.tool at tests/fail_test.lua:19 returned: weather_now: lua/demo/init.lua:85: lua.http.get https://wttr.in/Oslo?format=j1: permission ask required; allow it in config with zemi.permission.rule.add{ subject = "lua.http.get", pattern = "https://wttr.in/Oslo?format=j1", decision = "allow", match = "https://wttr.in/Oslo?format=j1", principal = "plugin:demo" }
  no stub answered it; answer it with test.stub("lua.http.get", "https://wttr.in/*", { status = 200, body = "..." })
0 passed, 1 failed (2 files, (T))
`},
		{name: "a callback fails", args: []string{"<plugin>", "callback"}, code: 1, want: `FAIL tests/fail_test.lua:22: a callback that fails
  error in zemi.job on_exit during test.advance at tests/fail_test.lua:24:
    lua/demo/init.lua:10: attempt to index a non-table object(nil) with key 'sub'
0 passed, 1 failed (2 files, (T))
`},
		{name: "a render fails", args: []string{"<plugin>", "a render that fails"}, code: 1, want: `FAIL tests/fail_test.lua:27: a render that fails
  tests/fail_test.lua:28: render demo.cost: lua/demo/init.lua:36: cannot perform mul operation between nil and number
0 passed, 1 failed (2 files, (T))
`},
		{name: "a reply field nothing reads", args: []string{"<plugin>", "misspelled"}, code: 1, want: `FAIL tests/fail_test.lua:39: a reply function with a misspelled field
  tests/fail_test.lua:43: got nil, want "Oslo: 21C, Sunny"
  test.tool at tests/fail_test.lua:43 returned: weather_now: lua/demo/init.lua:85: test.stub reply: bdy is not a field of a http reply, which takes body, error, headers, status
  test.stub reply: bdy is not a field of a http reply, which takes body, error, headers, status
0 passed, 1 failed (2 files, (T))
`},
		{name: "eq on an error naming a file", args: []string{"<plugin>", "naming a file"}, code: 1, want: `FAIL tests/fail_test.lua:46: eq on an error naming a file
  tests/fail_test.lua:49: got "tests/fail_test.lua:47: lua.http.get htt...", want "permission ask required" (first difference at char 1)
  no stub answered it; answer it with test.stub("lua.http.get", "https://example.com/*", { status = 200, body = "..." })
0 passed, 1 failed (2 files, (T))
`},
		{name: "a case times out", args: []string{"<plugin>", "never settles"}, code: 1, want: `FAIL tests/fail_test.lua:31: a case that never settles
  timed out after 300ms in test.advance(1) at tests/fail_test.lua:36
0 passed, 1 failed (2 files, (T))
`},
		{name: "a driver still running at the deadline", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": `zemi.command.add("spin", function() while true do end end)`, "tests/a_test.lua": `zemi.test("spins", function() zemi.test.run("spin") end)`,
		}, want: "FAIL tests/a_test.lua:1: spins\n  timed out after 300ms in test.run(\"spin\") at tests/a_test.lua:1\n0 passed, 1 failed (1 file, (T))\n"},
		{
			name: "a pattern matching nothing", args: []string{"<plugin>", "wether"}, code: 1,
			want: "no case matches \"wether\" (30 cases in 2 files)\n",
		},
		{name: "a plugin that does not load", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": `require("cp")`, "lua/cp/init.lua": `require("cp.util")`, "tests/a_test.lua": `zemi.test("a", function() end)`,
		}, want: "FAIL init.lua:1: require: lua/cp/init.lua:1: require: module \"cp.util\" not found on runtimepath\n0 passed, 1 failed (1 file, (T))\n"},
		{name: "a plugin whose load never ends", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": "while true do end", "tests/a_test.lua": `zemi.test("a", function() end)`,
		}, want: "FAIL init.lua:1: timed out after 300ms\n0 passed, 1 failed (1 file, (T))\n"},
		{name: "a test file that does not parse", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": "", "tests/a_test.lua": "zemi.test(\"a\", function()\n  local x =\nend)\n",
		}, want: "FAIL tests/a_test.lua:3: syntax error near 'end'\n0 passed, 1 failed (1 file, (T))\n"},
		{name: "a case name twice", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": "", "tests/a_test.lua": "zemi.test(\"a\", function() end)\nzemi.test(\"a\", function() end)\n",
		}, want: "FAIL tests/a_test.lua:2: duplicate case name \"a\", first at line 1\n0 passed, 1 failed (1 file, (T))\n"},
		{name: "no cases", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{
			"init.lua": "", "tests/a_test.lua": "-- soon\n",
		}, want: "no cases (1 file)\n"},
		{
			name: "no test files", args: []string{"<plugin>"}, code: 1, plugin: map[string]string{"init.lua": ""},
			want: "no tests/*_test.lua in <plugin>\n",
		},
		{name: "the copy leaves out dot entries, node_modules and links out", args: []string{"<plugin>"}, code: 0, plugin: map[string]string{
			"init.lua": "", "lua/cp/real.lua": "return 1", "lua/cp/alias.lua": "-> real.lua", "lua/cp/out.lua": "-> ../../../outside.lua",
			".hidden/x.lua": "return 1", "node_modules/x.lua": "return 1",
			"tests/a_test.lua": `local test = zemi.test
test("copied", function()
  test.eq(require("cp.alias"), 1)
  for _, m in ipairs({ "cp.out", "./../.hidden/x", "./../node_modules/x" }) do
    assert(not pcall(require, m), m)
  end
end)`,
		}, want: "1 passed (1 file, (T))\n"},
		{name: "an answer runs a row's run when no callback takes it", args: []string{"<plugin>"}, code: 0, plugin: map[string]string{
			"init.lua": `zemi.command.add("menu", function()
  zemi.state.set("n", (zemi.state.get("n") or 0) + 1)
  zemi.ui.overlay.open({ id = "menu", items = { { text = "again", run = "menu" }, { text = "help", run = "help" }, "none" } })
end)`,
			"tests/a_test.lua": `local test = zemi.test
test("rows", function()
  assert(test.run("menu"))
  assert(test.answer("again"))
  test.eq(zemi.state.get("n"), 2)
  for _, c in ipairs({ { "help", "/help is not a command of plugin cp" }, { "none", "overlay menu has no on_select, and the row no run" } }) do
    local ok, err = pcall(test.answer, c[1])
    assert(not ok and err:find(c[2], 1, true), err)
  end
end)`,
		}, want: "1 passed (1 file, (T))\n"},
		{name: "a job's output arrives when it ends, the clock unmoved", args: []string{"<plugin>"}, code: 0, plugin: map[string]string{
			"init.lua": `zemi.command.add("log", function()
  zemi.job.start({ cmd = "git", args = { "log" }, on_stdout = function(lines) zemi.state.set("out", lines) end })
end)`,
			"tests/a_test.lua": `local test = zemi.test
test.stub("lua.job.start", "git log", { stdout = "a\nb" })
test("out", function() assert(test.run("log")) test.eq(zemi.state.get("out"), { "a", "b" }) end)`,
		}, want: "1 passed (1 file, (T))\n"},
		{name: "advance past what a duration holds", args: []string{"<plugin>"}, code: 0, plugin: map[string]string{
			"init.lua": "", "tests/a_test.lua": `zemi.test("far", function() zemi.test.advance(math.huge) zemi.test.eq(os.date("!%Y"), "2318") end)`,
		}, want: "1 passed (1 file, (T))\n"},
		{name: "the zemi-customize example passes", args: []string{"<plugin>"}, code: 0, plugin: example, named: "branch", want: "2 passed (1 file, (T))\n"},
		{name: "a name the plugins step would load", args: []string{"named"}, code: 0, want: "1 passed (1 file, (T))\n"},
		{
			name: "300 boots under 256 open files", args: []string{"<plugin>"}, limit: 256, code: 0,
			plugin: map[string]string{"init.lua": "", "tests/a_test.lua": many}, want: "300 passed (1 file, (T))\n",
		},
		{name: "an interrupt leaves no temp dir", args: []string{"<plugin>", "pass_test"}, cancel: true, code: 130, want: ""},
		{
			name: "no such plugin", args: []string{"<plugin>/missing"}, code: 1,
			want: "zemi: plugins test: lstat <plugin>/missing: no such file or directory\n",
		},
		{name: "usage", code: 2, want: "zemi: usage: zemi plugins test <name|dir> [pattern]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := demo
			if c.plugin != nil {
				dir = filepath.Join(t.TempDir(), "p", cmp.Or(c.named, "cp"))
				for path, body := range c.plugin {
					if target, ok := strings.CutPrefix(body, "-> "); ok {
						if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(target, filepath.Join(dir, path)); err != nil {
							t.Fatal(err)
						}
						continue
					}
					writeFile(t, filepath.Join(dir, path), body)
				}
				writeFile(t, filepath.Join(filepath.Dir(dir), "..", "outside.lua"), "return 2")
			}
			args := make([]string, len(c.args))
			for i, a := range c.args {
				args[i] = strings.ReplaceAll(a, "<plugin>", dir)
			}
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			if c.limit > 0 {
				var was syscall.Rlimit
				if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &was); err != nil {
					t.Fatal(err)
				}
				lower := was
				lower.Cur = c.limit
				if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lower); err != nil {
					t.Fatal(err)
				}
				defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &was)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if c.cancel {
				cancel()
			}
			defer cancel()
			var out strings.Builder
			code := testPlugins(ctx, args, &out, &out)
			got := regexp.MustCompile(`\d+\.\d\ds\)`).ReplaceAllString(out.String(), "(T))")
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				got = strings.ReplaceAll(got, real, "<plugin>")
			}
			if got = strings.ReplaceAll(got, dir, "<plugin>"); code != c.code || got != c.want {
				t.Errorf("exit %d, want %d; output:\n%s\nwant:\n%s", code, c.code, got, c.want)
			}
			if left, _ := os.ReadDir(tmp); len(left) > 0 {
				t.Errorf("the run left %s in its temp dir", left[0].Name())
			}
		})
	}
	if treeSum(t, demo) != demoSum {
		t.Error("the run changed the plugin's own files")
	}
	if treeSum(t, home) != homeSum {
		t.Error("the run changed HOME or the zemi dirs")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeSum hashes every path, mode and file under root.
func treeSum(t *testing.T, root string) string {
	t.Helper()
	sum := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sum.Write([]byte(p + d.Type().String()))
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			sum.Write(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}
