package cli

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/runtime"
)

// TestCLI runs command lines through execute with a launch that records
// what it was handed; want nil means launch must not run.
func TestCLI(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	writeFile(t, filepath.Join(home, "config", "init.lua"), `zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "http://x", models = { { id = "m1" } } })`)
	writeFile(t, filepath.Join(home, "data", "builtin-skills", "zemi", "SKILL.md"), "# zemi\n")
	for _, name := range []string{"rub", "ver", "zzq"} {
		if err := os.MkdirAll(filepath.Join(home, "data", "plugins", "local", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(home, "data", "plugins", "local", "ver", ".zemi", "plugin.json"), `{"version": "1.2.0", "uses": ["job"]}`)
	writeFile(t, filepath.Join(home, "data", "plugins", "marketplaces.json"), `{"marketplaces": {"demo": {"source": "owner/repo", "added": "2026-01-02T00:00:00Z"}}}`)
	writeFile(t, filepath.Join(home, "data", "plugins", "marketplaces", "demo", plugin.ZemiMarketplaceFile), `{"name": "demo", "plugins": [
		{"name": "one", "source": "./p", "description": "the one"},
		{"name": "two", "source": "./q", "version": "0.1.0"}]}`)
	// stdin is no terminal, as for an agent's command
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer func(was *os.File) { os.Stdin = was }(os.Stdin)
	os.Stdin = stdin
	base, dir := t.TempDir(), t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	path := store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot())
	global, db, err := store.OpenForDirs(dirs.StateHome, dirs.TrustRoot())
	if err != nil {
		t.Fatal(err)
	}
	global.Close()
	if _, err := db.AppendLog("user", "fix the failing test", "", "", ""); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// a running zemi holds the write lock: a look at sessions waits on none
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.Begin()
	if err == nil {
		_, err = tx.Exec(`DELETE FROM kv`)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, c := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
		notStdout      string
		want           *options
	}{
		{name: "tui", want: &options{}},
		{name: "one-word typo", args: []string{"helth"}, code: 2, stderr: "unknown command"},
		{name: "a lone word is no command", args: []string{"fix the test"}, code: 2, stderr: `unknown command "fix the test" for "zemi"`},
		{name: "continue with resume", args: []string{"-c", "-r", "s1"}, code: 2, stderr: "unknown shorthand flag: 'c'"},
		{name: "print flag is gone", args: []string{"-p", "hi"}, code: 2, stderr: "unknown shorthand flag: 'p'"},
		{name: "trust false", args: []string{"--trust=false"}, want: &options{trust: new(false)}},
		{name: "version", args: []string{"--version"}, stdout: "zemi "},
		{name: "a failure is 1", args: []string{"db", "select * from nope"}, code: 1, stderr: `zemi: db: SQL logic error: no such table: nope`},
		{name: "plugins arity", args: []string{"plugin", "enable"}, code: 2, stderr: "zemi: plugin enable takes <name>\n"},
		{name: "a range of args", args: []string{"plugin", "install", "a", "b", "c"}, code: 2, stderr: "zemi: plugin install takes <name>[@<marketplace>]\n"},
		{name: "marketplace add takes a url", args: []string{"plugin", "marketplace", "add"}, code: 2, stderr: "zemi: plugin marketplace add takes <source>\n"},
		{name: "marketplace update takes a name", args: []string{"plugin", "marketplace", "update", "a", "b"}, code: 2, stderr: "zemi: plugin marketplace update takes [name]\n"},
		{name: "db takes sql", args: []string{"db"}, code: 2, stderr: "zemi: db takes <sql> [args...]\n"},
		{name: "marketplace list shows marketplaces, not entries", args: []string{"plugin", "marketplace", "list"}, stdout: "demo  owner/repo  2  ", notStdout: "the one"},
		{name: "marketplace list as JSON carries the entries", args: []string{"plugin", "marketplace", "list", "--json"}, stdout: `"entries":[{"name":"one","description":"the one"},{"name":"two","version":"0.1.0"}]`},
		{name: "completion help in zemi's words", args: []string{"completion", "--help"}, stdout: "print the bash completion script", notStdout: "Generate"},
		{name: "delete off a terminal", args: []string{"plugin", "delete", "zzq"}, code: 2, stderr: "pass --yes to delete zzq"},
		{name: "plugins lists", args: []string{"plugin", "list"}, stdout: "rub  local  on  "},
		{name: "bare plugin shows its help", args: []string{"plugin"}, stdout: "Available Commands:"},
		{name: "docs setup names the skill path", args: []string{"docs", "setup"}, stdout: filepath.Join(dirs.DataHome, "builtin-skills", "zemi", "SKILL.md"), notStdout: "start zemi once"},
		{name: "bare docs shows its help", args: []string{"docs"}, stdout: "Available Commands:"},
		{name: "plugins as JSON", args: []string{"plugin", "list", "--json"}, stdout: `{"name":"rub","version":"local","local":true,"enabled":true,"source":"`},
		{name: "the JSON carries the declared uses", args: []string{"plugin", "list", "--json"}, stdout: `"uses":["job"]`},
		{name: "a local plugin's manifest is the version it lists", args: []string{"plugin", "list"}, stdout: "ver  1.2.0  on  "},
		{name: "delete with yes", args: []string{"plugin", "delete", "--yes", "zzq"}},
		{name: "the delete took", args: []string{"plugin", "list"}, stdout: "rub", notStdout: "zzq"},
		{name: "completion script", args: []string{"completion", "fish"}, stdout: "complete -c zemi"},
		{name: "complete a plugin name", args: []string{"__complete", "plugin", "enable", ""}, stdout: "rub\n"},
		{name: "-C a missing dir", args: []string{"-C", "nope", "db", "select 1"}, code: 1, stderr: "zemi: db: -C nope: no such file or directory"},
		{name: "-C a dir", args: []string{"-C", dir}, want: &options{dir: dir}},
		{name: "scratch is trusted for the run", args: []string{"--scratch"}, want: &options{scratch: true, trust: new(true)}},
		{name: "scratch and -C collide", args: []string{"--scratch", "-C", dir}, code: 1, stderr: "none of the others can be"},
	} {
		if err := os.Chdir(base); err != nil {
			t.Fatal(err)
		}
		var out, errOut strings.Builder
		var got *options
		code := execute(c.args, &out, &errOut, func(o options) int {
			got = &o
			return 0
		})
		if code != c.code || !strings.Contains(out.String(), c.stdout) || !strings.Contains(errOut.String(), c.stderr) ||
			c.notStdout != "" && strings.Contains(out.String(), c.notStdout) || c.code == 1 && strings.Contains(errOut.String(), "--help") {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out.String(), errOut.String())
		}
		if (got == nil) != (c.want == nil) {
			t.Errorf("%s: launched %+v", c.name, got)
			continue
		}
		if got == nil {
			continue
		}
		sameTrust := (got.trust == nil) == (c.want.trust == nil) && (got.trust == nil || *got.trust == *c.want.trust)
		got.trust, c.want.trust = nil, nil
		got.dirs, c.want.dirs = config.Dirs{}, config.Dirs{} // the environment, not the flags
		if *got != *c.want || !sameTrust {
			t.Errorf("%s: launched %+v", c.name, *got)
		}
		if wd, _ := os.Getwd(); c.want.dir != "" && (wd != c.want.dir || os.Getenv("PWD") != c.want.dir) {
			t.Errorf("%s: cwd %q, PWD %q", c.name, wd, os.Getenv("PWD"))
		}
	}
	// a look at sessions, completion included, creates no store
	if stores, _ := os.ReadDir(filepath.Join(home, "state", "projects")); len(stores) != 1 {
		t.Errorf("%d project stores, want the seeded one", len(stores))
	}
}

// TestReadStdin: the TUI takes a terminal or /dev/null on stdin and
// refuses a pipe.
func TestReadStdin(t *testing.T) {
	for _, c := range []struct {
		name, stdin string
		null        bool // null reads /dev/null, a character device
		err         string
	}{
		{name: "a pipe is refused", stdin: "x", err: "stdin is piped; the TUI does not read it"},
		{name: "/dev/null is taken", null: true},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			w.WriteString(c.stdin)
			w.Close()
		}()
		if c.null {
			r.Close()
			if r, err = os.Open(os.DevNull); err != nil {
				t.Fatal(err)
			}
		}
		o := options{}
		err = o.readStdin(r)
		r.Close()
		if !strings.Contains(fmt.Sprint(err), c.err) {
			t.Errorf("%s: err %v", c.name, err)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	for _, c := range []struct {
		name  string
		v     string
		rev   string
		dirty bool
		want  string
	}{
		{"no version", "", "", false, "dev"},
		{"devel", "(devel)", "", false, "dev"},
		{"no vcs info", "v0.1.1-0.20260928120000-abcdef123456", "", false, "dev"},
		{"past a tag", "v0.1.1-0.20260928120000-abcdef123456", "abcdef1234567890", false, "v0.1.0+abcdef1"},
		{"past a tag, dirty", "v0.1.1-0.20260928120000-abcdef123456", "abcdef1234567890", true, "v0.1.0+abcdef1.dirty"},
		{"no tag reachable", "v0.0.0-20240101120000-abcdef", "abcdef1234567890", false, "dev+abcdef1"},
		{"exact tag", "v1.2.3", "abcdef1234567890", false, "v1.2.3+abcdef1"},
		{"exact tag, dirty as go stamps it", "v1.2.3+dirty", "abcdef1234567890", true, "v1.2.3+abcdef1.dirty"},
		{"exact tag, dirty, no vcs", "v1.2.3+dirty", "", true, "v1.2.3.dirty"},
		{"release ldflags", "v1.2.3", "", false, "v1.2.3"},
	} {
		if got := normalizeVersion(c.v, c.rev, c.dirty); got != c.want {
			t.Errorf("%s: normalizeVersion(%q, %q, %v) = %q, want %q", c.name, c.v, c.rev, c.dirty, got, c.want)
		}
	}
}

// TestTrustGate: --trust or an earlier "not now" wins over the saved
// decision, which wins over the prompt; only y and n are saved, the prompt
// opens at most once per process, and only a trusted boot loads project commands.
func TestTrustGate(t *testing.T) {
	for _, c := range []struct {
		name, saved string
		session     *bool
		answer      string // "" means the prompt must not open
		trusted     bool
		wantSaved   string
	}{
		{"saved trusted", "trusted", nil, "", true, "trusted"},
		{"saved untrusted", "untrusted", nil, "", false, "untrusted"},
		{"flag over saved", "trusted", new(false), "", false, "trusted"},
		{"flag trusts", "", new(true), "", true, ""},
		{"prompt y", "", nil, "trust", true, "trusted"},
		{"prompt n", "", nil, "untrust", false, "untrusted"},
		{"prompt esc", "", nil, "later", false, ""},
	} {
		tg, h, r := newTrustGate(t)
		root := tg.dirs.TrustRoot()
		if err := os.MkdirAll(tg.dirs.WorktreesDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if c.saved != "" {
			if err := tg.dirs.WriteTrust(map[string]string{root: c.saved}); err != nil {
				t.Fatal(err)
			}
		}
		tg.session.Store(c.session)
		asked := 0
		tg.ask = func(string, string, string) string {
			asked++
			return c.answer
		}
		if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), tg.step(), h.MarkdownCommandsStep()}); err != nil {
			t.Fatal(err)
		}
		loaded := h.Commands.Lookup("x") != nil
		again, _ := tg.decide()
		_, err := os.Stat(filepath.Join(tg.dirs.WorktreesDir(), ".gitignore"))
		if ignored := err == nil; h.ProjectTrusted() != c.trusted || loaded != c.trusted || again != c.trusted || ignored != c.trusted {
			t.Errorf("%s: trusted=%v loaded=%v again=%v worktrees ignored=%v", c.name, h.ProjectTrusted(), loaded, again, ignored)
		}
		want := 0
		if c.answer != "" {
			want = 1
		}
		if asked != want {
			t.Errorf("%s: prompt opened %d times", c.name, asked)
		}
		if got := tg.dirs.ReadTrust()[root]; got != c.wantSaved {
			t.Errorf("%s: saved %q", c.name, got)
		}
	}
}

// TestTrustCommand: /trust saves the answer and reloads only when the
// decision changed; not now changes nothing.
func TestTrustCommand(t *testing.T) {
	for _, c := range []struct {
		answer, wantSaved string
		reloads           bool
	}{
		{"trust", "trusted", true},
		{"untrust", "untrusted", false},
		{"later", "", false},
	} {
		tg, _, _ := newTrustGate(t) // outside the bubble, which waits for every goroutine in it: a host's never exit
		synctest.Test(t, func(t *testing.T) {
			tg.ask = func(_, _, state string) string {
				if state != config.Untrusted {
					t.Errorf("/trust shows state %q", state)
				}
				return c.answer
			}
			reloaded := make(chan struct{}, 1)
			tg.h.SetReload(func() error { reloaded <- struct{}{}; return nil }, nil)
			tg.command()()
			select {
			case <-reloaded:
				if !c.reloads {
					t.Errorf("%s: reloaded", c.answer)
				}
			case <-time.After(500 * time.Millisecond):
				if c.reloads {
					t.Errorf("%s: no reload", c.answer)
				}
			}
			if got := tg.dirs.ReadTrust()[tg.dirs.TrustRoot()]; got != c.wantSaved {
				t.Errorf("%s: saved %q", c.answer, got)
			}
		})
	}
}

func newTrustGate(t *testing.T) (*trustGate, *hostapi.Host, *lua.Runtime) {
	t.Helper()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents", "commands", "x.md"), "---\ndescription: x\n---\nhello")
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: runtime.FS})
	dirs := config.Dirs{ProjectDir: project, StateHome: t.TempDir()}
	h := hostapi.NewHost(r, log, config.NewStore(), dirs)
	t.Cleanup(h.Close)
	return &trustGate{h: h, dirs: dirs}, h, r
}

// TestHeadlessNotices: with no UI a warning or an error reaches stderr, the
// -m warning included, info stays quiet, and a command that opens a screen
// names itself.
func TestHeadlessNotices(t *testing.T) {
	_, h, r := newTrustGate(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })`, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Commands.Add(&hostapi.Command{
		Name: "screen", Desc: "d", Origin: lua.OriginUser,
		GoFn: func(h *hostapi.Host, _ string, confirm bool) error { _, err := h.Screen(); return err },
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, want string
		do         func() error
	}{
		{"info", "", func() error { h.Notify(slog.LevelInfo, "Copied", "3 chars"); return nil }},
		{"untitled error", "zemi: Error: first\n  second\n", func() error { h.Notify(slog.LevelError, "", "first\nsecond"); return nil }},
		{"zemi.notify.warn", "zemi: Warning: w\n", func() error {
			return r.ExecuteChunk(t.Context(), `zemi.notify.warn("w")`, "init.lua", lua.OriginUser)
		}},
		{"screen command", "", func() error {
			if err := h.SendPrompt("/screen"); err == nil || err.Error() != "/screen needs the TUI" {
				return fmt.Errorf("got %w", err)
			}
			return nil
		}},
	} {
		var out strings.Builder
		h.Stderr = &out
		if err := c.do(); err != nil || out.String() != c.want {
			t.Errorf("%s: err %v, stderr %q", c.name, err, out.String())
		}
	}
}

// BenchmarkConfigFingerprint walks a local plugin with a dependency tree
// in it, the shape that made the one-second watch cost.
func BenchmarkConfigFingerprint(b *testing.B) {
	dir := b.TempDir()
	deps := filepath.Join(dir, "local", "y", "node_modules")
	for i := range 2000 {
		p := filepath.Join(deps, fmt.Sprintf("m%d", i%100), fmt.Sprintf("f%d.js", i))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "local", "y", ".zemi", "init.lua"), []byte("-- one"), 0o644)
	for b.Loop() {
		configFingerprint("", pluginRoots(dir)...)
	}
}

// TestCompleteEntries: zemi plugin install offers a registered marketplace's
// entries as the name@marketplace it types, and nothing past the first
// argument.
func TestCompleteEntries(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, env))
	}
	t.Setenv("ZEMI_DATA_HOME", filepath.Join(home, "data"))
	writeFile(t, filepath.Join(home, "data", "plugins", "marketplaces.json"), `{"marketplaces": {"demo": {"source": "owner/repo", "added": "2026-01-02T00:00:00Z"}}}`)
	writeFile(t, filepath.Join(home, "data", "plugins", "marketplaces", "demo", plugin.ZemiMarketplaceFile), `{"name": "demo", "plugins": [
		{"name": "one", "source": "./p"},
		{"name": "two", "source": "./q"}]}`)
	c := newRoot(&options{}, func(options) int { return 0 })
	install, _, err := c.Find([]string{"plugin", "install"})
	if err != nil {
		t.Fatal(err)
	}
	got, directive := install.ValidArgsFunction(install, nil, "")
	if !slices.Equal(got, []cobra.Completion{"one@demo", "two@demo"}) || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("install completes %v (%v)", got, directive)
	}
	if got, _ := install.ValidArgsFunction(install, []string{"one@demo"}, ""); got != nil {
		t.Errorf("install completes past its argument: %v", got)
	}
}

// wireProbe is not a wire type: it pins the walk's pointer, slice and map
// cases, and that a value leaves writeJSON as it entered.
type wireProbe struct {
	Ptr   *string
	Slice []string
	Map   map[string]string
}

// TestWriteJSONSanitizes: every string the wire carries, a nested row's
// included, passes the same sanitize pass at writeJSON - plant a control
// byte in each field class and none may reach a reader of the line.
func TestWriteJSONSanitizes(t *testing.T) {
	const esc = "\x1b]0;pwned\a\x7f"
	probe := wireProbe{Ptr: new(string), Slice: []string{esc}, Map: map[string]string{"k\x07\x7f": esc}}
	*probe.Ptr = esc
	var out strings.Builder
	writeJSON(&out, probe)
	line := out.String()
	for _, b := range []byte{0x1b, 0x07, 0x7f} {
		if strings.IndexByte(line, b) >= 0 {
			t.Errorf("raw byte %#x on the line: %q", b, line)
		}
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if strings.ContainsAny(x, "\x1b\x07\x7f") {
				t.Errorf("%s carries a control byte: %q", path, x)
			}
		case []any:
			for _, e := range x {
				walk(path, e)
			}
		case map[string]any:
			for k, e := range x {
				if strings.ContainsAny(k, "\x1b\x07\x7f") {
					t.Errorf("%s key carries a control byte: %q", path, k)
				}
				walk(path+"."+k, e)
			}
		}
	}
	for k, e := range row {
		walk("."+k, e)
	}
	if *probe.Ptr != esc || probe.Map["k\x07\x7f"] != esc {
		t.Error("writeJSON rewrote the caller's value")
	}
}
