package hostapi

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// newTestHostSkills boots a host with stores plus skill directories and
// the path class real boot installs (the config dir is user origin).
func newTestHostSkills(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	h, r := newTestHostSkillsNoBoot(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// newTestHostSkillsNoBoot is newTestHostSkills without the boot, for tests
// that need to lay down skill files first: every Boot builds a fresh
// generation, so all steps that must share state must pass in ONE call.
func newTestHostSkillsNoBoot(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	cfg, data, state, proj := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		ConfigHome: cfg,
		DataHome:   data,
		StateHome:  state,
		ProjectDir: proj,
	})
	t.Cleanup(h.Close)
	h.ReleaseBuilder() // tests stub the model and boot no config
	return h, r
}

// writeSkill creates one skill directory with a SKILL.md and an optional
// zemi-skill.lua hook.
func writeSkill(t *testing.T, base, name, description, hook string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if hook != "" {
		if err := os.WriteFile(filepath.Join(dir, "zemi-skill.lua"), []byte(hook), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSkillItemsNameTheMarketplace: a plugin skill's source names the
// marketplace its plugin was installed from; a local plugin's does not.
func TestSkillItemsNameTheMarketplace(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	root := filepath.Join(h.Dirs.PluginsRoot(), "installed", "from-mkt")
	writeSkill(t, filepath.Join(root, "skills"), "marketed", "came from a marketplace", "")
	writeSkill(t, filepath.Join(h.Dirs.PluginsRoot(), "local", "handmade", "skills"), "handmade", "came from nowhere", "")
	reg := `{"installs":{"from-mkt":{"marketplace":"awesome-zemi","entry":"from-mkt","version":"v1.2","kind":"link","id":"i1"}}}`
	if err := os.WriteFile(filepath.Join(h.Dirs.PluginsRoot(), "marketplaces.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pluginGrant.Store(&pluginGrant{roots: []string{root, filepath.Join(h.Dirs.PluginsRoot(), "local", "handmade")}, names: []string{"from-mkt", "handmade"}})
	h.syncSkills()
	fields := map[string]string{}
	for _, it := range h.skillItems() {
		var b strings.Builder
		for _, f := range it.Fields {
			b.WriteString(f.Name + ": " + f.Value + "\n")
		}
		fields[it.Text] = b.String()
	}
	if src, ok := fields["marketed"]; !ok || !strings.Contains(src, "plugin:from-mkt, from the awesome-zemi marketplace") {
		t.Fatalf("marketed source: %q", src)
	}
	if src := fields["handmade"]; strings.Contains(src, "marketplace") {
		t.Fatalf("a local plugin names no marketplace: %q", src)
	}
}

// TestSkillItemsShowTheBody: a skill's view holds its text; only a skill
// with a file of its own offers to open it, since zemi rewrites a builtin's
// on every start.
func TestSkillItemsShowTheBody(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()
	seen := map[string]bool{}
	for _, it := range h.skillItems() {
		name, keys, body := it.Text, fmt.Sprint(it.Keys), it.Detail
		if name != "local-run" && name != "zemi" {
			continue
		}
		seen[name] = true
		if body == "" || strings.Contains(keys, "skills open "+name) != (name == "local-run") {
			t.Errorf("%s: keys %v, body %v", name, it.Keys, it.Detail)
		}
	}
	if !seen["local-run"] || !seen["zemi"] {
		t.Fatalf("listed %v", seen)
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.skillsCommand("open zemi", false); !errors.Is(err, ErrUsage) {
		t.Fatalf("/skills open zemi: %v", err)
	}
}

// TestSkillsEnableGate covers zemi.skills.enable: any origin flips the
// state, the on argument is required, unknown names are refused.
func TestSkillsEnableGate(t *testing.T) {
	t.Parallel()
	h, r := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()

	runUser(t, r, `zemi.skills.enable("local-run", false)`)
	runUser(t, r, `
		for _, s in ipairs(zemi.skills.list()) do
			if s.name == "local-run" then
				assert(s.enabled == false, "enable(name, false) did not stick")
			end
		end
		assert(not pcall(zemi.skills.enable, "local-run"), "on is required")
	`)

	if err := r.ExecuteChunk(t.Context(), `zemi.skills.enable("local-run", true)`, "project.lua", lua.OriginProject); err != nil {
		t.Fatalf("project-origin enable: %v", err)
	}
	runUser(t, r, `
		local ok, err = zemi.skills.enable("missing-skill", false)
		assert(ok == nil and err:find("no such skill"), tostring(err))
	`)
}

// TestSkillsUserHooksStep covers the boot-window hook run: an enabled
// user skill's zemi-skill.lua runs in the user hooks step. The
// skill must exist before boot (production lays down dirs before boot).
func TestSkillsUserHooksStep(t *testing.T) {
	t.Parallel()
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, h.userSkillsDir(), "booted", "user", `
		zemi.command.add("booted-cmd", function() end, { desc = "from user hook" })
	`)
	h.syncSkills()
	if err := r.Boot(t.Context(), []lua.BootStep{
		h.NamespacesStep(), h.SkillsUserHooksStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if h.Commands.Lookup("booted-cmd") == nil {
		t.Fatal("user hook did not run in the boot window")
	}
}

// TestSkillRows: what the model sees and what a /skills row says. No skill
// is a command of its own: the user invokes one with @name in a message.
func TestSkillRows(t *testing.T) {
	const asks = "d\nuser-invocable: true\nargument-hint: <n>"
	user := func(desc string) func(h *Host) {
		return func(h *Host) { writeSkill(t, h.userSkillsDir(), "x", desc, "") }
	}
	project := func(h *Host) { writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "skills"), "x", asks, "") }
	for _, c := range []struct {
		name      string
		write     func(h *Host)
		untrusted bool
		off       bool
		indexed   bool
		used      string
	}{
		{name: "plain", write: user("d"), indexed: true, used: "the model"},
		// the model loads every skill: Claude's key hides nothing
		{name: "the model-invocation key is ignored", write: user("d\ndisable-model-invocation: true"), indexed: true, used: "the model"},
		{name: "user-invocable stays metadata", write: user(asks), indexed: true, used: "the model"},
		{name: "a trusted project skill", write: project, indexed: true, used: "the model"},
		{name: "an untrusted project skill", write: project, untrusted: true},
		{name: "a recipe takes neither field", write: func(h *Host) {
			writeSkill(t, h.recipesDir(), "x", "d\nuser-invocable: true", "")
		}, indexed: true, used: "the model"},
		{name: "off leaves the @name token", write: user("d"), off: true, used: "@x only"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHostSkillsNoBoot(t)
			c.write(h)
			h.SetProjectTrusted(!c.untrusted)
			steps := []lua.BootStep{
				h.NamespacesStep(), h.BuiltinCommandsStep(), h.PluginsStep(),
				lua.LoadFileStep("init", filepath.Join(h.Dirs.ConfigHome, "init.lua"), lua.OriginUser),
				h.SkillsUserHooksStep(), h.MarkdownCommandsStep(),
			}
			if err := r.Boot(t.Context(), steps); err != nil {
				t.Fatal(err)
			}
			h.SetUIHooks(&fakeUI{})
			if c.off {
				if err := h.setSkillEnabled("x", false); err != nil {
					t.Fatal(err)
				}
			}
			indexed := slices.ContainsFunc(h.skillIndex(), func(s tools.SkillInfo) bool { return s.Name == "x" })
			mine, proj := h.skillsIndexPrompt()
			if prompt := strings.Contains(mine+proj, "- x: "); indexed != c.indexed || prompt != c.indexed {
				t.Errorf("skill tool %v, prompt %v, want %v", indexed, prompt, c.indexed)
			}
			if got := h.Commands.Lookup("x"); got != nil {
				t.Errorf("/x registered: %+v", got)
			}
			used := ""
			for _, it := range h.skillItems() {
				if it.Text == "x" && it.Section != "Builtin" {
					used = dumpFields(it.Fields)
				}
			}
			if c.used == "" && used != "" || c.used != "" && !strings.Contains(used, "[used by "+c.used+"]") {
				t.Errorf("fields %q, want used by %q", used, c.used)
			}
			if err := h.skillsCommand("use x", false); !errors.Is(err, ErrUsage) {
				t.Errorf("/skills use x: %v, want a usage error", err)
			}
		})
	}
}

// TestProjectSkills: a trusted project adds its skills to the index, an
// user skill keeps its name, a skill written later waits for the next
// trust decision, and an untrusted project adds none.
func TestProjectSkills(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "deploy", "the user's deploy", "")
	h.syncSkills()
	root := h.Dirs.ProjectDir
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "deploy", "the project's deploy", "")
	writeSkill(t, filepath.Join(root, ".claude", "skills"), "claudely", "not read", "")
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "lint", "project lint", "")
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "release", "project release", "")

	index := func() map[string]string {
		got := map[string]string{}
		for _, s := range h.enabledSkills() {
			if s.Source != skill.Builtin {
				got[s.Name] = s.Description
			}
		}
		return got
	}
	h.SetProjectTrusted(true)
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "late", "written mid-session", "")
	want := map[string]string{"deploy": "the user's deploy", "lint": "project lint", "release": "project release"}
	if got := index(); !maps.Equal(got, want) {
		t.Fatalf("trusted: %v", got)
	}
	// /health explains the prompt, so it counts the project's skills too
	for _, row := range h.HealthRows() {
		if row.Name == "skills" && !strings.Contains(row.Detail, "(2 from this project)") {
			t.Fatalf("skills row = %q", row.Detail)
		}
	}
	h.SetProjectTrusted(false)
	if got := index(); !maps.Equal(got, map[string]string{"deploy": "the user's deploy"}) {
		t.Fatalf("untrusted: %v", got)
	}
}

// TestDescriptionCutWarnsOnly: a description over the cap still loads, and
// the cut is a log line, not an /errors row on every boot.
func TestDescriptionCutWarnsOnly(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "big", strings.Repeat("d", skill.MaxDescriptionRunes+1), "")
	h.syncSkills()
	s, ok := h.skillsReg.get("big")
	if !ok || len([]rune(s.Description)) != skill.MaxDescriptionRunes {
		t.Fatalf("the cut skill did not load: %+v", s)
	}
	for _, e := range h.errs.entries {
		if strings.Contains(e.msg, "description cut") {
			t.Fatalf("recorded %+v", h.errs.entries)
		}
	}
}

// TestClaudeCommandSkillClash: a plugin shipping skills/x/ and commands/x.md
// loads both: the skills row keeps the bare name and the command row falls
// back to the prefixed one.
func TestClaudeCommandSkillClash(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	root := filepath.Join(h.pluginsRoot(), "installed", "clashy")
	writeSkill(t, filepath.Join(root, "skills"), "x", "the skill one", "")
	cmd := filepath.Join(root, "commands", "x.md")
	if err := os.MkdirAll(filepath.Dir(cmd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cmd, []byte("---\ndescription: the command one\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.pluginGrant.Store(&pluginGrant{roots: []string{root}, uses: map[string][]string{}, claude: set.Of(root)})
	h.syncSkills()
	s, ok := h.skillsReg.get("x")
	if !ok || s.Dir != filepath.Join(root, "skills", "x") {
		t.Fatalf("skills row: %+v", s)
	}
	if c, ok := h.skillsReg.get("clashy-x"); !ok || c.File != cmd {
		t.Fatalf("command row: %+v", c)
	}
	if slices.ContainsFunc(h.errs.entries, func(e errEntry) bool { return strings.Contains(e.msg, "commands"+string(filepath.Separator)+"x.md") }) {
		t.Fatalf("recorded %+v", h.errs.entries)
	}
}

// TestRecipes: the steps share one host, in order. The pinned prompt lists
// the recipes it was built with; the skill tool resolves them live.
func TestRecipes(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "deploy", "the user's deploy", "")
	writeSkill(t, h.userSkillsDir(), "ship", "the user's ship", "")
	h.syncSkills()
	writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "skills"), "lint", "project lint", "")
	dir := h.recipesDir()
	for _, name := range []string{"rig", "deploy", "lint"} {
		writeSkill(t, dir, name, "recipe "+name, "")
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	md := func(sub, name, body string) string {
		in, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, sub, "SKILL.md"), "text": "---\nname: " + name + "\ndescription: recipe " + name + "\n---\n\n" + body + "\n"}, json.Deterministic(true))
		return string(in)
	}
	// NAME=url is a command the entropy pass would cut
	tok := md("tok", "tok", "step one: TOKEN=ghp_0123456789abcdefghijklmnopqrstuv make rig\nAUTH_ISSUER=http://127.0.0.1:8099/auth/realms/local make run")
	edit, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "tok", "SKILL.md"), "search": "step one", "replace": "step 1"}, json.Deterministic(true))
	late := []string{"late", "rig"}
	written := []string{"late", "rig", "tok"}
	pinned := func() string {
		toolset := h.mainTools()
		_, sys, ctx, _ := h.pinPrompt(toolset)
		return sys + ctx
	}

	// check asserts the pinned prompt, the /skills rows and the skill tool
	// index all name want, and nothing else from the recipes dir.
	check := func(t *testing.T, wantPrompt, wantTool []string) {
		t.Helper()
		var prompt, tool, items []string
		for line := range strings.Lines(pinned()) {
			if name, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": recipe "); ok {
				prompt = append(prompt, name)
			}
		}
		for _, s := range h.skillIndex() {
			if filepath.Dir(s.Path) == dir {
				tool = append(tool, s.Name)
			}
		}
		for _, it := range h.skillItems() {
			// a recipe has no stored state, so its row must not toggle
			if it.Section == "Recipe" && it.Run == "" {
				items = append(items, it.Text)
			}
		}
		slices.Sort(prompt)
		slices.Sort(tool)
		slices.Sort(items)
		if !slices.Equal(prompt, wantPrompt) || !slices.Equal(items, wantTool) || !slices.Equal(tool, wantTool) {
			t.Errorf("prompt %v, /skills %v, tool %v", prompt, items, tool)
		}
	}
	// call runs a tool the model would, asserting its result says want.
	call := func(t *testing.T, name, input, want string) {
		t.Helper()
		ui.asked = nil
		all := tools.All(h.PipelineFor())
		i := slices.IndexFunc(all, func(a fantasy.AgentTool) bool { return a.Info().Name == name })
		resp, err := all[i].Run(t.Context(), fantasy.ToolCall{ID: "t", Name: name, Input: input})
		if err != nil || resp.IsError || !strings.Contains(resp.Content, want) {
			t.Errorf("%v %q, want %q", err, resp.Content, want)
		}
	}

	// the steps build on each other in this order: each sees what the ones
	// before it did
	t.Run("untrusted project is inert", func(t *testing.T) {
		h.SetProjectTrusted(false)
		check(t, nil, nil)
	})
	t.Run("trusted lists it, a taken name is dropped", func(t *testing.T) {
		h.SetProjectTrusted(true)
		check(t, []string{"rig"}, []string{"rig"})
	})
	t.Run("written mid-session loads, the prompt stays", func(t *testing.T) {
		writeSkill(t, dir, "late", "recipe late", "")
		check(t, []string{"rig"}, late)
	})
	t.Run("a reload's trust step keeps the pin", func(t *testing.T) {
		h.SetProjectTrusted(true)
		check(t, []string{"rig"}, late)
	})
	t.Run("a prompt drop pins again", func(t *testing.T) {
		h.agents.mu.Lock()
		h.agents.dropPin()
		h.agents.mu.Unlock()
		check(t, late, late)
	})
	t.Run("a secret is redacted", func(t *testing.T) {
		call(t, "write", tok, "redacted lines [6]")
		check(t, late, written)
	})
	t.Run("any file there is scrubbed, whatever its name", func(t *testing.T) {
		call(t, "write", strings.Replace(tok, "SKILL.md", "skill.md", 1), "redacted lines [6]")
		check(t, late, written)
	})
	t.Run("the redacted file is not stale", func(t *testing.T) {
		call(t, "edit", string(edit), "tok saved")
		check(t, late, written)
	})
	t.Run("a name the directory does not have", func(t *testing.T) {
		call(t, "write", md("bad", "other", "x"), "will not load: bad: frontmatter name")
		check(t, late, written)
	})
	t.Run("a name a user skill has", func(t *testing.T) {
		call(t, "write", md("ship", "ship", "x"), "another skill is named ship")
		check(t, late, written)
	})
	t.Run("a session that fetched says so", func(t *testing.T) {
		h.recordToolCall(store.MainSessionID, StreamTool{ID: "c1", Name: "fetch"}, true)
		call(t, "write", md("bad", "other", "y"), "will not load")
		check(t, late, written)
	})
	t.Run("only a recipe is deleted", func(t *testing.T) {
		if h.skillsCommand("delete ship", false) == nil || h.skillsCommand("delete lint", false) == nil {
			t.Error("deleted a skill that is no recipe")
		}
		check(t, late, written)
	})
	t.Run("delete removes it and pins again", func(t *testing.T) {
		if err := h.skillsCommand("delete late --yes", false); err != nil {
			t.Error(err)
		}
		check(t, []string{"rig", "tok"}, []string{"rig", "tok"})
	})
	if data, _ := os.ReadFile(filepath.Join(dir, "tok", "SKILL.md")); !strings.Contains(string(data), "step 1") || !strings.Contains(string(data), "realms/local") || strings.Contains(string(data), "ghp_") {
		t.Errorf("tok on disk: %s", data)
	}
	if !strings.Contains(h.buildSystem(), "Recipes: "+dir+"\n") {
		t.Error("the environment of a trusted project names the recipes dir")
	}
	for _, d := range []string{filepath.Join(dir, "late"), filepath.Join(dir, "ship"), filepath.Join(h.userSkillsDir(), "ship"), filepath.Join(h.Dirs.ProjectDir, ".agents", "skills", "lint")} {
		if _, err := os.Stat(d); (err == nil) == (d == filepath.Join(dir, "late")) {
			t.Errorf("after the deletes: stat %s: %v", d, err)
		}
	}
}
