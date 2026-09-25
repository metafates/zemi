package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/tools"
)

// newTestHostSkills boots a host with stores plus skill directories and
// the path class real boot installs (the config dir is user origin).
func newTestHostSkills(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	h, r := newTestHostSkillsNoBoot(t)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// newTestHostSkillsNoBoot is newTestHostSkills without the boot, for tests
// that need to lay down skill files first: every Boot builds a fresh
// generation, so all steps that must share state must pass in ONE call.
func newTestHostSkillsNoBoot(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := logging.New(t.TempDir())
	cfg, data, state, proj := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		ConfigHome: cfg,
		DataHome:   data,
		StateHome:  state,
		ProjectDir: proj,
	})
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

// TestSkillsEnabledDefaults covers §10.5: a skill starts enabled and the
// skill index the model sees lists enabled skills only.
func TestSkillsEnabledDefaults(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	writeSkill(t, h.userSkillsDir(), "rare", "used once a year", "")
	h.syncSkills()
	if err := h.setSkillEnabled("rare", false); err != nil {
		t.Fatal(err)
	}

	runUser(t, r, `
		local byName, n = {}, 0
		for _, s in ipairs(zemi.skills.list()) do
			if s.source ~= "builtin" then
				byName[s.name], n = s, n + 1
			end
		end
		assert(n == 2, "skills=" .. n)
		assert(byName["local-run"].enabled == true, "a user skill must be enabled")
		assert(byName["local-run"].source == "user", "source=" .. tostring(byName["local-run"].source))
		assert(byName["rare"].enabled == false, "disable did not stick")
	`)
	indexed := map[string]bool{}
	for _, s := range h.skillIndex() {
		indexed[s.Name] = true
	}
	if !indexed["local-run"] || indexed["rare"] {
		t.Fatalf("skillIndex must list enabled skills only: %v", indexed)
	}
}

// TestSkillItemsShowTheBody: /skills offers the instructions and the file
// itself.
func TestSkillItemsShowTheBody(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()
	for _, it := range h.skillItems() {
		m := it.(map[string]any)
		if m["text"] != "local-run" {
			continue
		}
		actions := fmt.Sprint(m["actions"])
		if !strings.Contains(actions, "Body.") || !strings.Contains(actions, "skills open local-run") {
			t.Fatalf("no read or open action: %v", m["actions"])
		}
		return
	}
	t.Fatal("the skill is not listed")
}

// TestSkillsEnableGate covers the origin gate: user origin flips the
// state, a project-origin chunk is refused, unknown names are refused.
func TestSkillsEnableGate(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()

	runUser(t, r, `zemi.skills.disable("local-run")`)
	runUser(t, r, `
		for _, s in ipairs(zemi.skills.list()) do
			if s.name == "local-run" then
				assert(s.enabled == false, "disable did not stick")
			end
		end
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.skills.enable("local-run")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("project-origin enable: %v", err)
	}
	runUser(t, r, `
		local ok, err = zemi.skills.disable("missing-skill")
		assert(ok == nil and err:find("no such skill"), tostring(err))
	`)
}

// TestSkillsUserHooksStep covers the boot-window hook run: an enabled
// user skill's zemi-skill.lua runs in the user hooks step. The
// skill must exist before boot (production lays down dirs before boot).
func TestSkillsUserHooksStep(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, h.userSkillsDir(), "booted", "user", `
		zemi.command.add("booted-cmd", function() end, { desc = "from user hook" })
	`)
	h.syncSkills()
	if err := r.Boot(context.Background(), []lua.BootStep{
		h.NamespacesStep(), h.SkillsUserHooksStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if h.Commands.Lookup("booted-cmd") == nil {
		t.Fatal("user hook did not run in the boot window")
	}
}

// TestLearnListAndDecide covers the §6.7 surface: list mirrors
// the memory queue, decide accepts or rejects, and only a user or builtin
// origin may decide.
func TestLearnListAndDecide(t *testing.T) {
	h, r := newTestHostSkills(t)
	seedSession(t, h)

	runUser(t, r, `zemi.memory.add("deploy script lives in bin/deploy")`)
	runUser(t, r, `
		local p = zemi.learn.list()
		assert(#p == 1, "proposals=" .. #p)
		assert(p[1].quote == "", "quote=" .. tostring(p[1].quote))
		assert(p[1].id ~= nil and p[1].id ~= "", "id missing")
	`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.list()[1].id, "reject")`)
	runUser(t, r, `
		assert(#zemi.learn.list() == 0, "rejected proposal still queued")
	`)
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("rejected proposal stored %d memories", len(rows))
	}

	runUser(t, r, `zemi.memory.add("the fts join needs m-qualified columns")`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.list()[1].id, "accept")`)
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("accepted proposal stored %d memories", len(rows))
	}

	runUser(t, r, `
		local ok, err = zemi.learn.decide("missing", "accept")
		assert(ok == nil and err:find("no such proposal"), tostring(err))
	`)
	runExpectErr(t, r, `zemi.learn.decide("x", "maybe")`, "accept or reject")

	err := r.ExecuteChunk(context.Background(),
		`zemi.learn.decide("whatever", "accept")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("project-origin decide: %v", err)
	}
}

// TestSkillsUse: /skills use sends the body without frontmatter ahead of
// the request, enabled or not.
func TestSkillsUse(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()
	s := h.skillsReg.list()[slices.IndexFunc(h.skillsReg.list(), func(s skills.Skill) bool { return s.Name == "local-run" })]
	msg, err := skillMessage(s, "make a landing page")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<loaded_skill name="local-run">`, "Skill directory: " + s.Dir, "\n\nBody.\n</loaded_skill>\n\nmake a landing page"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "description:") {
		t.Errorf("frontmatter leaked:\n%s", msg)
	}
	// a disabled skill is reviewed code: naming it is the request
	if err := h.setSkillEnabled("local-run", false); err != nil {
		t.Fatal(err)
	}
	if err := h.skillsCommand("use local-run"); err != nil && strings.Contains(err.Error(), "no skill") {
		t.Errorf("disabled user skill: err = %v", err)
	}
	if err := h.skillsCommand("use nothing"); err == nil || !strings.Contains(err.Error(), "no skill") {
		t.Errorf("unknown skill: err = %v", err)
	}
}

// TestProjectSkills: a trusted project adds its skills to the index, an
// user skill keeps its name, a skill written later waits for the next
// trust decision, and an untrusted project adds none.
func TestProjectSkills(t *testing.T) {
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
			if s.Source != skills.Builtin {
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
	for _, row := range h.healthChecks() {
		if row.name == "skills" && !strings.Contains(row.detail, "(2 from this project)") {
			t.Fatalf("skills row = %q", row.detail)
		}
	}
	h.SetProjectTrusted(false)
	if got := index(); !maps.Equal(got, map[string]string{"deploy": "the user's deploy"}) {
		t.Fatalf("untrusted: %v", got)
	}
}

// TestSymlinkedSkillsDir: <config>/skills may be a link into a dotfiles
// tree. Its files must stay inside the read jail and classify as user
// origin, or every skill body asks and every hook is refused.
func TestSymlinkedSkillsDir(t *testing.T) {
	log := logging.New(t.TempDir())
	cfg, real := t.TempDir(), t.TempDir()
	if err := os.Symlink(real, filepath.Join(cfg, "skills")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dirs := config.Dirs{ConfigHome: cfg, DataHome: t.TempDir(), StateHome: t.TempDir(), ProjectDir: t.TempDir()}
	classes := lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}
	for _, dir := range dirs.UserTrees() {
		classes = append(classes, lua.PathClass{Prefix: dir, Origin: lua.OriginUser})
	}
	h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes}), log, config.NewStore(), dirs)
	writeSkill(t, h.userSkillsDir(), "demo", "demo skill", "return {}")
	if !h.inJail(filepath.Join(h.userSkillsDir(), "demo", "SKILL.md"), h.readJails()...) {
		t.Error("a symlinked skills dir is outside the read jail")
	}
	hook, err := filepath.EvalSymlinks(filepath.Join(real, "demo", "zemi-skill.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if got := classes.Classify(hook); got != lua.OriginUser {
		t.Errorf("hook origin = %q, want user", got)
	}
}

// TestRecipes: the steps share one host, in order. The prompt lists the
// recipes pinned with the environment; the skill tool resolves them live. A
// step naming a tool runs it in auto mode, where the user is asked every time.
func TestRecipes(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "deploy", "the user's deploy", "")
	writeSkill(t, h.userSkillsDir(), "ship", "the user's ship", "")
	h.syncSkills()
	writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "skills"), "lint", "project lint", "")
	dir := h.recipesDir()
	for _, name := range []string{"rig", "deploy", "lint"} {
		writeSkill(t, dir, name, "recipe "+name, "")
	}
	ui := &fakeUI{askAllowed: true, askBy: "user:once"}
	h.SetUIHooks(ui)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, h)
	md := func(sub, name, body string) string {
		in, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, sub, "SKILL.md"), "content": "---\nname: " + name + "\ndescription: recipe " + name + "\n---\n\n" + body + "\n"})
		return string(in)
	}
	// NAME=url is a command the entropy pass would cut
	tok := md("tok", "tok", "step one: TOKEN=ghp_0123456789abcdefghijklmnopqrstuv make rig\nAUTH_ISSUER=http://127.0.0.1:8099/auth/realms/local make run")
	edit, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "tok", "SKILL.md"), "old_string": "step one", "new_string": "step 1"})
	late := []string{"late", "rig"}
	written := []string{"late", "rig", "tok"}

	for _, c := range []struct {
		name         string
		do           func()
		prompt, tool []string
		read         perm.Decision
		call, input  string // a tool the model runs, and what its result says
		out          string
		outside      bool // the ask names text from outside the machine
	}{
		{name: "untrusted project is inert", do: func() { h.SetProjectTrusted(false) }, read: perm.Ask},
		{name: "trusted lists it, a taken name is dropped", do: func() { h.SetProjectTrusted(true); h.environmentPrompt() }, prompt: []string{"rig"}, tool: []string{"rig"}},
		{name: "written mid-session loads, the prompt stays", do: func() { writeSkill(t, dir, "late", "recipe late", "") }, prompt: []string{"rig"}, tool: late},
		{name: "a reload's trust step keeps the pin", do: func() { h.SetProjectTrusted(true) }, prompt: []string{"rig"}, tool: late},
		{name: "compaction pins again", do: func() { h.compactDone(0, false); h.environmentPrompt() }, prompt: late, tool: late},
		{name: "a secret is redacted", prompt: late, tool: written, call: "write", input: tok, out: "redacted lines [6]"},
		{name: "any file there is scrubbed, whatever its name", prompt: late, tool: written, call: "write", input: strings.Replace(tok, "SKILL.md", "skill.md", 1), out: "redacted lines [6]"},
		{name: "the redacted file is not stale", prompt: late, tool: written, call: "edit", input: string(edit), out: "tok saved"},
		{name: "a name the directory does not have", prompt: late, tool: written, call: "write", input: md("bad", "other", "x"), out: "will not load: bad: frontmatter name"},
		{name: "a name a user skill has", prompt: late, tool: written, call: "write", input: md("ship", "ship", "x"), out: "another skill is named ship"},
		{name: "a session that fetched says so", do: func() { h.recordToolCall(sid, StreamTool{ID: "c1", Name: "fetch"}, true) }, prompt: late, tool: written, call: "write", input: md("bad", "other", "y"), out: "will not load", outside: true},
		{name: "only a recipe is deleted", do: func() {
			if h.skillsCommand("delete ship") == nil || h.skillsCommand("delete lint") == nil {
				t.Error("deleted a skill that is no recipe")
			}
		}, prompt: late, tool: written},
		{name: "delete removes it and pins again", do: func() {
			if err := h.skillsCommand("delete late"); err != nil {
				t.Error(err)
			}
		}, prompt: []string{"rig", "tok"}, tool: []string{"rig", "tok"}},
	} {
		if c.do != nil {
			c.do()
		}
		if c.call != "" {
			ui.asked = nil
			all := tools.All(h.PipelineFor())
			i := slices.IndexFunc(all, func(a fantasy.AgentTool) bool { return a.Info().Name == c.call })
			resp, err := all[i].Run(context.Background(), fantasy.ToolCall{ID: "t", Name: c.call, Input: c.input})
			if err != nil || resp.IsError || !strings.Contains(resp.Content, c.out) {
				t.Errorf("%s: %v %q, want %q", c.name, err, resp.Content, c.out)
			}
			// no mode settles a recipe and no rule could: Always stays empty
			if len(ui.asked) != 1 || ui.asked[0].Always != "" || !strings.Contains(ui.asked[0].Reason, "zemi's own files") ||
				strings.Contains(ui.asked[0].Reason, "outside the machine") != c.outside {
				t.Errorf("%s: asked %+v", c.name, ui.asked)
			}
		}
		var prompt, tool, items []string
		for line := range strings.Lines(h.skillsIndexPrompt()) {
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
			if m := it.(map[string]any); m["section"] == "Recipe" && m["run"] == nil {
				items = append(items, m["text"].(string))
			}
		}
		slices.Sort(prompt)
		slices.Sort(tool)
		slices.Sort(items)
		if !slices.Equal(prompt, c.prompt) || !slices.Equal(items, c.tool) || !slices.Equal(tool, c.tool) {
			t.Errorf("%s: prompt %v, /skills %v, tool %v", c.name, prompt, items, tool)
		}
		if got := h.fsDefault("fs.read", filepath.Join(dir, "rig", "SKILL.md")); got != cmp.Or(c.read, perm.Allow) {
			t.Errorf("%s: read = %v, want %v", c.name, got, cmp.Or(c.read, perm.Allow))
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "tok", "SKILL.md")); !strings.Contains(string(data), "step 1") || !strings.Contains(string(data), "realms/local") || strings.Contains(string(data), "ghp_") {
		t.Errorf("tok on disk: %s", data)
	}
	if !strings.Contains(h.environmentPrompt(), "Recipes: "+dir+"\n") {
		t.Error("the environment of a trusted project names the recipes dir")
	}
	for _, d := range []string{filepath.Join(dir, "late"), filepath.Join(dir, "ship"), filepath.Join(h.userSkillsDir(), "ship"), filepath.Join(h.Dirs.ProjectDir, ".agents", "skills", "lint")} {
		if _, err := os.Stat(d); (err == nil) == (d == filepath.Join(dir, "late")) {
			t.Errorf("after the deletes: stat %s: %v", d, err)
		}
	}
}

func TestListDesc(t *testing.T) {
	for in, want := range map[string]string{
		"Load before running the tests.": "load before running the tests",
		"LSP servers for Go":             "LSP servers for Go",
		"":                               "",
	} {
		if got := listDesc(in); got != want {
			t.Errorf("listDesc(%q) = %q, want %q", in, got, want)
		}
	}
}
