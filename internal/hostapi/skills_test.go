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
// the path classes real boot installs (authored skills are user origin,
// learned skills learned origin).
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
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "skills"), Origin: lua.OriginLearned},
	}
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes})
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

// TestSkillsQuarantineDefaults covers §10.5: authored skills start
// enabled, learned skills start disabled, and the skill index the model
// sees lists enabled skills only.
func TestSkillsQuarantineDefaults(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.authoredSkillsDir(), "local-run", "run the local suite", "")
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned from sessions", "")
	h.syncSkills()

	runUser(t, r, `
		local byName, n = {}, 0
		for _, s in ipairs(zemi.skills.list()) do
			if s.source ~= "builtin" then
				byName[s.name], n = s, n + 1
			end
		end
		assert(n == 2, "skills=" .. n)
		assert(byName["local-run"].enabled == true, "authored must be enabled")
		assert(byName["local-run"].source == "authored", "source=" .. tostring(byName["local-run"].source))
		assert(byName["auto-learned"].enabled == false, "learned must start disabled")
		assert(byName["auto-learned"].source == "learned", "source=" .. tostring(byName["auto-learned"].source))
	`)
	indexed := map[string]bool{}
	for _, s := range h.skillIndex() {
		indexed[s.Name] = true
	}
	if !indexed["local-run"] || indexed["auto-learned"] {
		t.Fatalf("skillIndex must list enabled skills only: %v", indexed)
	}
}

// TestSkillItemsShowTheBody: enabling a learned draft is the only review
// it gets, so /skills offers the instructions and the file itself.
func TestSkillItemsShowTheBody(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned from sessions", "")
	h.syncSkills()
	for _, it := range h.skillItems() {
		m := it.(map[string]any)
		if m["text"] != "auto-learned" {
			continue
		}
		actions := fmt.Sprint(m["actions"])
		if !strings.Contains(actions, "Body.") || !strings.Contains(actions, "skills open auto-learned") {
			t.Fatalf("no read or open action: %v", m["actions"])
		}
		return
	}
	t.Fatal("the learned skill is not listed")
}

// TestSkillsEnableGate covers the origin gate: user origin flips the
// state, a project-origin chunk is refused, unknown names are refused.
func TestSkillsEnableGate(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned", "")
	h.syncSkills()

	runUser(t, r, `zemi.skills.enable("auto-learned")`)
	runUser(t, r, `
		for _, s in ipairs(zemi.skills.list()) do
			if s.name == "auto-learned" then
				assert(s.enabled == true, "enable did not stick")
			end
		end
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.skills.enable("auto-learned")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("project-origin enable: %v", err)
	}
	runUser(t, r, `
		local ok, err = zemi.skills.disable("missing-skill")
		assert(ok == nil and err:find("no such skill"), tostring(err))
	`)
}

// TestSkillsLearnedHookOnEnable covers the learned enable hook: it runs
// under the learned origin on enable (commands and widgets allowed), and
// a tool registration inside it is refused.
func TestSkillsLearnedHookOnEnable(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.learnedSkillsDir(), "hooked", "learned with a hook", `
		zemi.command.add("hooked-cmd", function() end, { desc = "from skill hook" })
		local ok = pcall(zemi.tool.add, {
			name = "hooked-tool", description = "nope",
			execute = function() end })
		assert(not ok, "learned origin registered a tool")
	`)
	h.syncSkills()
	runUser(t, r, `zemi.skills.enable("hooked")`)
	if h.Commands.Lookup("hooked-cmd") == nil {
		t.Fatal("learned skill hook did not register its command")
	}

	writeSkill(t, h.learnedSkillsDir(), "hooked2", "go-side enable", `
		zemi.command.add("hooked2-cmd", function() end, { desc = "from skill hook" })
	`)
	h.syncSkills()
	if err := h.skillsCommand("enable hooked2"); err != nil {
		t.Fatalf(":skills enable: %v", err)
	}
	if h.Commands.Lookup("hooked2-cmd") == nil {
		t.Fatal("Go-side enable hook did not run")
	}
}

// TestSkillsAuthoredHooksStep covers the boot-window hook run: an enabled
// authored skill's zemi-skill.lua runs in the authored hooks step. The
// skill must exist before boot (production lays down dirs before boot).
func TestSkillsAuthoredHooksStep(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, h.authoredSkillsDir(), "booted", "authored", `
		zemi.command.add("booted-cmd", function() end, { desc = "from authored hook" })
	`)
	h.syncSkills()
	if err := r.Boot(context.Background(), []lua.BootStep{
		h.NamespacesStep(), h.SkillsAuthoredHooksStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if h.Commands.Lookup("booted-cmd") == nil {
		t.Fatal("authored hook did not run in the boot window")
	}
}

// TestLearnProposalsAndDecide covers the §6.7 surface: proposals mirrors
// the memory queue, decide accepts or rejects, and only a user or builtin
// origin may decide.
func TestLearnProposalsAndDecide(t *testing.T) {
	h, r := newTestHostSkills(t)
	seedSession(t, h)

	runUser(t, r, `zemi.memory.add({ kind = "fact", text = "deploy script lives in bin/deploy" })`)
	runUser(t, r, `
		local p = zemi.learn.proposals()
		assert(#p == 1, "proposals=" .. #p)
		assert(p[1].kind == "fact", "kind=" .. tostring(p[1].kind))
		assert(p[1].id ~= nil and p[1].id ~= "", "id missing")
	`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.proposals()[1].id, "reject")`)
	runUser(t, r, `
		assert(#zemi.learn.proposals() == 0, "rejected proposal still queued")
	`)
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("rejected proposal stored %d memories", len(rows))
	}

	runUser(t, r, `zemi.memory.add({ kind = "gotcha", text = "the fts join needs m-qualified columns" })`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.proposals()[1].id, "accept")`)
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
// the request, for any skill but an unreviewed learned draft.
func TestSkillsUse(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.authoredSkillsDir(), "local-run", "run the local suite", "")
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned from sessions", "")
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
	if err := h.skillsCommand("use auto-learned"); err == nil || !strings.Contains(err.Error(), "unreviewed draft") {
		t.Errorf("learned draft: err = %v", err)
	}
	// a disabled authored skill is reviewed code: naming it is the request
	if err := h.setSkillEnabled(nil, "local-run", false); err != nil {
		t.Fatal(err)
	}
	if err := h.skillsCommand("use local-run"); err != nil && strings.Contains(err.Error(), "no skill") {
		t.Errorf("disabled authored skill: err = %v", err)
	}
	if err := h.skillsCommand("use nothing"); err == nil || !strings.Contains(err.Error(), "no skill") {
		t.Errorf("unknown skill: err = %v", err)
	}
}

// TestProjectSkills: a trusted project adds its skills to the index, an
// authored skill keeps its name, a skill written later waits for the next
// trust decision, and an untrusted project adds none.
func TestProjectSkills(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.authoredSkillsDir(), "deploy", "the user's deploy", "")
	h.syncSkills()
	root := h.Dirs.ProjectDir
	writeSkill(t, filepath.Join(root, ".claude", "skills"), "deploy", "the project's deploy", "")
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "lint", "project lint", "")
	writeSkill(t, filepath.Join(root, ".zemi", "skills"), "release", "project release", "")

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
	writeSkill(t, filepath.Join(root, ".claude", "skills"), "late", "written mid-session", "")
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
	writeSkill(t, h.authoredSkillsDir(), "demo", "demo skill", "return {}")
	if !h.inJail(filepath.Join(h.authoredSkillsDir(), "demo", "SKILL.md"), h.readJails()...) {
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
	writeSkill(t, h.authoredSkillsDir(), "deploy", "the user's deploy", "")
	writeSkill(t, h.authoredSkillsDir(), "ship", "the user's ship", "")
	h.syncSkills()
	writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".zemi", "skills"), "lint", "project lint", "")
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
		{name: "a secret is redacted", prompt: late, tool: written, call: "write", input: md("tok", "tok", "step one: TOKEN=ghp_0123456789abcdefghijklmnopqrstuv make rig"), out: "redacted"},
		{name: "the redacted file is not stale", prompt: late, tool: written, call: "edit", input: string(edit), out: "tok saved"},
		{name: "a name the directory does not have", prompt: late, tool: written, call: "write", input: md("bad", "other", "x"), out: "will not load: bad: frontmatter name"},
		{name: "a name an authored skill has", prompt: late, tool: written, call: "write", input: md("ship", "ship", "x"), out: "another skill is named ship"},
		{name: "a session that fetched says so", do: func() { h.recordToolCall(sid, "c1", "fetch", "", true) }, prompt: late, tool: written, call: "write", input: md("bad", "other", "y"), out: "will not load", outside: true},
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
			if len(ui.asked) != 1 || ui.asked[0].Always != "" || !strings.Contains(ui.asked[0].Reason, "not settled") ||
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
		if !slices.Equal(prompt, c.prompt) || !slices.Equal(items, c.prompt) || !slices.Equal(tool, c.tool) {
			t.Errorf("%s: prompt %v, /skills %v, tool %v", c.name, prompt, items, tool)
		}
		if got := h.fsDefault("fs.read", filepath.Join(dir, "rig", "SKILL.md")); got != cmp.Or(c.read, perm.Allow) {
			t.Errorf("%s: read = %v, want %v", c.name, got, cmp.Or(c.read, perm.Allow))
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "tok", "SKILL.md")); !strings.Contains(string(data), "step 1") || strings.Contains(string(data), "ghp_") {
		t.Errorf("tok on disk: %s", data)
	}
	if !strings.Contains(h.environmentPrompt(), "Recipes: "+dir+"\n") {
		t.Error("the environment of a trusted project names the recipes dir")
	}
	for _, d := range []string{filepath.Join(dir, "late"), filepath.Join(dir, "ship"), filepath.Join(h.authoredSkillsDir(), "ship"), filepath.Join(h.Dirs.ProjectDir, ".zemi", "skills", "lint")} {
		if _, err := os.Stat(d); (err == nil) == (d == filepath.Join(dir, "late")) {
			t.Errorf("after the deletes: stat %s: %v", d, err)
		}
	}
	seedTranscript(t, h, sid)
	// or the reflector drafts a global skill for what the project already has
	if p, _, err := h.reflectPrompt(sid); err != nil || !strings.Contains(p, "- rig (enabled)") || !strings.Contains(p, "- lint (enabled)") {
		t.Errorf("reflector prompt: %v\n%s", err, p)
	}
}
