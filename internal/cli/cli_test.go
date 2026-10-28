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

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/store"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// TestCLI runs command lines through execute with a launch that records
// what it was handed; want nil means launch must not run.
func TestCLI(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	writeFile(t, filepath.Join(home, "config", "init.lua"), `zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "http://x", models = { { id = "m1" } } })`)
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
		{name: "prompt words join", args: []string{"-m", "a/b", "fix", "the", "test"}, want: &options{model: "a/b", prompt: "fix the test"}},
		{name: "flags after the prompt", args: []string{"hi", "-m", "x", "there"}, want: &options{model: "x", prompt: "hi there"}},
		{name: "dash dash keeps a dashed prompt", args: []string{"--", "-v", "means", "verbose?"}, want: &options{prompt: "-v means verbose?"}},
		{name: "dash dash keeps a command name", args: []string{"--", "plugin", "x"}, want: &options{prompt: "plugin x"}},
		{name: "one-word typo", args: []string{"helth"}, code: 2, stderr: "did you mean health"},
		{name: "continue with resume", args: []string{"-c", "-r", "s1"}, code: 2, stderr: "unknown shorthand flag: 'c'"},
		{name: "print flag is gone", args: []string{"-p", "hi"}, code: 2, stderr: "unknown shorthand flag: 'p'"},
		{name: "trust false", args: []string{"--trust=false"}, want: &options{trust: new(false)}},
		{name: "version", args: []string{"--version"}, stdout: "zemi "},
		{name: "health takes no args", args: []string{"health", "x"}, code: 2, stderr: "Run 'zemi health --help'"},
		{name: "a parent takes no args", args: []string{"models", "are", "slow"}, code: 2, stderr: `unknown command "are" for "zemi models"`},
		{name: "models lists what init.lua adds", args: []string{"models"}, stdout: "gw/m1\n"},
		{name: "-m parses before a subcommand", args: []string{"-m", "gw/m1", "models"}, stdout: "gw/m1\n"},
		{name: "-m without a slash refuses db", args: []string{"-m", "nope", "db", "select 1"}, code: 1, stderr: `zemi: db: -m "nope" is not a provider/model id`},
		{name: "-m with an empty half refuses models", args: []string{"-m", "nope/", "models"}, code: 1, stderr: `zemi: models: -m "nope/" is not a provider/model id`},
		{name: "-m a well-formed unknown provider stays db's no-op", args: []string{"-m", "nope/x", "db", "select 1"}, stdout: "1"},
		{name: "complete a model", args: []string{"__complete", "-m", ""}, stdout: "gw/m1\n"},
		{name: "a failure is 1", args: []string{"db", "select * from nope"}, code: 1, stderr: `zemi: db: SQL logic error: no such table: nope`},
		// the TUI's chat opens on the log's tail; the CLI keeps no session surface
		{name: "projects lists", args: []string{"projects"}, stdout: "  undecided     1  " + dirs.TrustRoot() + "\n"},
		{name: "projects as JSON", args: []string{"projects", "--json"}, stdout: `"trust":"undecided",`},
		{name: "plugins arity", args: []string{"plugin", "enable"}, code: 2, stderr: "zemi: plugin enable takes <name>\n"},
		{name: "a range of args", args: []string{"plugin", "install", "a", "b", "c"}, code: 2, stderr: "zemi: plugin install takes <name>[@<marketplace>]\n"},
		{name: "marketplace add takes a url", args: []string{"plugin", "marketplace", "add"}, code: 2, stderr: "zemi: plugin marketplace add takes <source>\n"},
		{name: "marketplace update takes a name", args: []string{"plugin", "marketplace", "update", "a", "b"}, code: 2, stderr: "zemi: plugin marketplace update takes [name]\n"},
		{name: "db takes sql", args: []string{"db"}, code: 2, stderr: "zemi: db takes <sql> [args...]\n"},
		{name: "marketplace list shows marketplaces, not entries", args: []string{"plugin", "marketplace", "list"}, stdout: "demo  owner/repo  2  ", notStdout: "the one"},
		{name: "marketplace list as JSON carries the entries", args: []string{"plugin", "marketplace", "list", "--json"}, stdout: `"entries":[{"name":"one","description":"the one"},{"name":"two","version":"0.1.0"}]`},
		{name: "completion help in zemi's words", args: []string{"completion", "--help"}, stdout: "print the bash completion script", notStdout: "Generate"},
		{name: "delete off a terminal", args: []string{"plugin", "delete", "zzq"}, code: 2, stderr: "pass --yes to delete zzq"},
		{name: "plugins lists", args: []string{"plugin"}, stdout: "rub  local  on  "},
		{name: "plugin list is the bare verb", args: []string{"plugin", "list"}, stdout: "rub  local  on  "},
		{name: "plugins as JSON", args: []string{"plugin", "--json"}, stdout: `{"name":"rub","version":"local","local":true,"enabled":true,"source":"`},
		{name: "the JSON carries the declared uses", args: []string{"plugin", "--json"}, stdout: `"uses":["job"]`},
		{name: "plugin list as JSON", args: []string{"plugin", "list", "--json"}, stdout: `{"name":"rub","version":"local","local":true,"enabled":true,"source":"`},
		{name: "a local plugin's manifest is the version it lists", args: []string{"plugin"}, stdout: "ver  1.2.0  on  "},
		{name: "delete with yes", args: []string{"plugin", "delete", "--yes", "zzq"}},
		{name: "the delete took", args: []string{"plugin"}, stdout: "rub", notStdout: "zzq"},
		{name: "completion script", args: []string{"completion", "fish"}, stdout: "complete -c zemi"},
		{name: "complete a plugin name", args: []string{"__complete", "plugin", "enable", ""}, stdout: "rub\n"},
		{name: "-C a missing dir", args: []string{"-C", "nope", "fix", "it"}, code: 1, stderr: "zemi: -C nope: no such file or directory"},
		{name: "-C a dir", args: []string{"-C", dir, "fix", "it"}, want: &options{prompt: "fix it", dir: dir}},
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
		prompt      string
		null        bool // null reads /dev/null, a character device
		err         string
		notErr      string
	}{
		{name: "a pipe is refused", stdin: "x", err: "stdin is piped; the TUI does not read it - give the prompt as an argument"},
		{name: "a pipe with the prompt given names no move", stdin: "x", prompt: "say hi", err: "stdin is piped; the TUI does not read it", notErr: "give the prompt"},
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
		o := options{prompt: c.prompt}
		err = o.readStdin(r)
		r.Close()
		errText := fmt.Sprint(err)
		if !strings.Contains(errText, c.err) || c.notErr != "" && strings.Contains(errText, c.notErr) || o.prompt != c.prompt {
			t.Errorf("%s: prompt %q, err %v", c.name, o.prompt, err)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "dev",
		"(devel)":                      "dev",
		"v0.0.0-20240101120000-abcdef": "dev",
		"v0.1.1-0.20260928120000-abcdef123456+dirty": "dev",
		"v1.2.3":       "v1.2.3",
		"v1.2.3+dirty": "v1.2.3+dirty",
	} {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTrustGate: --trust or an earlier "not now" wins over the saved
// decision, which wins over the prompt; only y and n are saved, the prompt
// opens at most once per process and not where the splash offers other
// projects, and only a trusted boot loads project commands.
func TestTrustGate(t *testing.T) {
	for _, c := range []struct {
		name, saved string
		session     *bool
		answer      string // "" means the prompt must not open
		trusted     bool
		wantSaved   string
		offers      bool // another project is known
		noSplash    bool // a prompt or a resume fills the first screen
	}{
		{"saved trusted", "trusted", nil, "", true, "trusted", false, false},
		{"saved untrusted", "untrusted", nil, "", false, "untrusted", false, false},
		{"flag over saved", "trusted", new(false), "", false, "trusted", false, false},
		{"flag trusts", "", new(true), "", true, "", false, false},
		{"prompt y", "", nil, "trust", true, "trusted", false, false},
		{"prompt n", "", nil, "untrust", false, "untrusted", false, false},
		{"prompt esc", "", nil, "later", false, "", false, false},
		{"projects offered", "", nil, "", false, "", true, false},
		{"projects offered, no splash", "", nil, "trust", true, "trusted", true, true},
	} {
		tg, h, r := newTrustGate(t)
		tg.splash = !c.noSplash
		if c.offers {
			g, p, err := store.OpenForDirs(tg.dirs.StateHome, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			g.Close()
			p.Close()
		}
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
		if want := map[bool]int{true: 1, false: 0}[c.answer != ""]; asked != want {
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

// TestHeadlessTrust: a headless run with no saved decision never trusts.
func TestHeadlessTrust(t *testing.T) {
	if got := headlessTrust()("/repo", "/repo", ""); got != "later" {
		t.Fatalf("answer %q", got)
	}
}

func newTrustGate(t *testing.T) (*trustGate, *hostapi.Host, *lua.Runtime) {
	t.Helper()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents", "commands", "x.md"), "---\ndescription: x\n---\nhello")
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: zemiruntime.FS})
	dirs := config.Dirs{ProjectDir: project, StateHome: t.TempDir()}
	h := hostapi.NewHost(r, log, config.NewStore(), dirs)
	return &trustGate{h: h, dirs: dirs}, h, r
}

// TestTrustGateInertWritesNothing: zemi models, zemi health and a completion
// boot the project trust step too, and an inert boot may not leave a
// .agents/worktrees/.gitignore in the user's repository.
func TestTrustGateInertWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name       string
		inert      bool
		wantIgnore bool
	}{
		{"an inert boot", true, false},
		{"a TUI boot", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tg, h, r := newTrustGate(t)
			h.Inert = c.inert
			wt := h.Dirs.WorktreesDir()
			if err := os.MkdirAll(wt, 0o755); err != nil {
				t.Fatal(err)
			}
			tg.session.Store(new(true))
			if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), tg.step()}); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(wt, ".gitignore"))
			if wrote := err == nil; wrote != c.wantIgnore {
				t.Errorf("wrote .gitignore = %v, want %v", wrote, c.wantIgnore)
			}
		})
	}
}

// TestSendPromptRunsCommands: a prompt on the command line that names a
// command runs it, the way the composer would, with attached text behind the
// marker; anything else is a message.
func TestSendPromptRunsCommands(t *testing.T) {
	_, h, _ := newTrustGate(t)
	got := ""
	if _, err := h.Commands.Add(&hostapi.Command{
		Name: "note", NArgs: "*", Desc: "d", Origin: lua.OriginUser,
		GoFn: func(_ *hostapi.Host, args string, confirm bool) error { got = args; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	attached := agent.Attached("stdin", "diff --git a/x b/x")
	for _, c := range []struct{ prompt, args string }{
		{"/note the args", "the args"},
		{"/note\nnext line", "next line"},
		{"/note" + attached, attached},
	} {
		got = "unset"
		if err := h.SendPrompt(c.prompt); err != nil || got != c.args {
			t.Errorf("%q: ran with %q, %v", c.prompt, got, err)
		}
	}
	// no such command: the text is the model's, and this host has no model
	got = "unset"
	if err := h.SendPrompt("/notes please"); err == nil || got != "unset" {
		t.Fatalf("an unknown command must reach the agent: %v, ran %q", err, got)
	}
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
		{"-m unlisted", "zemi: Model not listed: a/unlisted: not in the provider's models list; the provider decides whether it exists\n", func() error { return applyOptions(h, options{model: "a/unlisted"}) }},
		{"-m a target name", "", func() error {
			if err := applyOptions(h, options{model: "compact"}); err == nil || !strings.Contains(err.Error(), `provider "compact" is not registered`) {
				return fmt.Errorf("got %w", err)
			}
			return nil
		}},
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

// TestModelFlag: -m writes the conversation's slot, models.default, and
// leaves the other slots alone.
func TestModelFlag(t *testing.T) {
	_, h, r := newTrustGate(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	provider := `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "main" }, { id = "cheap" } } })`
	if err := r.ExecuteChunk(t.Context(), provider, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	if err := applyOptions(h, options{model: "a/main"}); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings.Str("models.default"); got != "a/main" {
		t.Fatalf("models.default = %q, want the flag's model", got)
	}
	// a bare /model <id> follows the slot the same way
	if err := h.SetModel("a/cheap"); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings.Str("models.default"); got != "a/cheap" {
		t.Fatalf("models.default = %q, want the bare pick", got)
	}
	if err := h.SetModel("compact a/main"); err != nil {
		t.Fatal(err)
	}
	if got, def := h.Settings.Str("models.compact"), h.Settings.Str("models.default"); got != "a/main" || def != "a/cheap" {
		t.Fatalf("a named slot wrote another: compact %q, default %q", got, def)
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

// TestTable: the table pads a column by the text it shows, not its escapes,
// holds a column at its floor, right-aligns a count, and a header is a
// terminal's alone.
func TestTable(t *testing.T) {
	build := func(p colorprofile.Profile, cell string) string {
		t := table{p: p, cols: []tableCol{
			{head: "name", role: "hint", width: 4},
			{head: "n", right: true},
		}}
		t.row(cell, "5")
		var b strings.Builder
		t.print(&b)
		return b.String()
	}
	if got := build(colorprofile.NoTTY, "on"); got != "on    5\n" {
		t.Errorf("plain table = %q, want the column at its floor", got)
	}
	// the escape in the cell pads by the text it shows, and the header rides bold
	if got := build(colorprofile.Ascii, "\x1b[1mon\x1b[m"); got != "\x1b[1mname\x1b[m  \x1b[1mn\x1b[m\n\x1b[1mon\x1b[m    5\n" {
		t.Errorf("styled table = %q", got)
	}
	// columns that name no head print none, even on a terminal: the checks
	// each say what they are
	anon := table{p: colorprofile.Ascii, cols: []tableCol{{width: 5}, {}}}
	anon.row("ok", "store")
	var b strings.Builder
	anon.print(&b)
	if got := b.String(); got != "ok     store\n" {
		t.Errorf("unnamed heads = %q, want no header row", got)
	}
}

// TestSessionsAcrossProjects: -a reaches the other projects' stores, grouped
// by project.
func TestSessionsAcrossProjects(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base, other := t.TempDir(), t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	if perm.CanonPath(dirs.TrustRoot()) != perm.CanonPath(base) {
		t.Fatalf("this project is %q, want %q", dirs.TrustRoot(), base)
	}
	for _, p := range []struct{ root, title string }{
		{dirs.TrustRoot(), "local notes"},
		{other, "deploy the kubelet"},
	} {
		global, db, err := store.OpenForDirs(dirs.StateHome, p.root)
		if err != nil {
			t.Fatal(err)
		}
		global.Close()
		if _, err := db.AppendLog("user", p.title, "", "", ""); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	// a store no SQLite reads, never opened so the file stays what it is
	garbage := t.TempDir()
	skipDir := filepath.Dir(store.ProjectDBPath(dirs.StateHome, garbage))
	if err := os.MkdirAll(skipDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"root": garbage + "\n", "zemi.db": "not a database"} {
		if err := os.WriteFile(filepath.Join(skipDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
		root           string // the project the run must start in, "" for any
	}{
		{name: "projects names the unreadable store", args: []string{"projects"}, stderr: "zemi: skipped 1 unreadable store: " + perm.TildePath(garbage) + ": "},
		{name: "projects JSON carries the stats error", args: []string{"projects", "--json"}, stdout: `"stats_error":"`},
	} {
		var out, errOut strings.Builder
		var got options
		code := execute(c.args, &out, &errOut, func(o options) int {
			got = o
			return 0
		})
		if code != c.code || !strings.Contains(out.String(), c.stdout) || !strings.Contains(errOut.String(), c.stderr) {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out.String(), errOut.String())
			continue
		}
		if c.root != "" && perm.CanonPath(got.dirs.TrustRoot()) != perm.CanonPath(c.root) {
			t.Errorf("%s: started in %q, want %q", c.name, got.dirs.TrustRoot(), c.root)
		}
	}
}

// TestProjectsAllUnreadable: when every store is unreadable, /projects names
// the skips instead of reading green over them.
func TestProjectsAllUnreadable(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// the root file names the trust root the hash must match: the symlinked
	// state home resolves to a different spelling, so hash the resolved root
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root"), []byte(resolved+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zemi.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := execute([]string{"projects"}, &out, &errOut, func(options) int { return 0 })
	for _, want := range []string{"zemi: skipped 1 unreadable store"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q, want %q", errOut.String(), want)
		}
	}
	if code != 0 {
		t.Errorf("code = %d, want 0; stdout %q", code, out.String())
	}
}

// TestModelsOverBrokenInit: an init.lua that fails after adding providers
// still boots soft, but the list it prints is incomplete, so the run is a
// failure, the way health reads the same state.
func TestModelsOverBrokenInit(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	writeFile(t, filepath.Join(home, "config", "init.lua"), `zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "http://x", models = { { id = "m1" } } })
error("boom after the providers")`)
	var out, errOut strings.Builder
	code := execute([]string{"models"}, &out, &errOut, func(options) int { return 0 })
	if code != 1 || !strings.Contains(out.String(), "gw/m1") {
		t.Errorf("code = %d, stdout %q, stderr %q; want 1 with the model list on stdout", code, out.String(), errOut.String())
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
