package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skill"
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
// itself, but for a builtin skill, whose file zemi rewrites on every start.
func TestSkillItemsShowTheBody(t *testing.T) {
	h, _ := newTestHostSkills(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	h.syncSkills()
	seen := map[string]bool{}
	for _, it := range h.skillItems() {
		m := it.(map[string]any)
		name, actions := m["text"].(string), fmt.Sprint(m["actions"])
		if name != "local-run" && name != "zemi" {
			continue
		}
		seen[name] = true
		if !strings.Contains(actions, "Read it") || strings.Contains(actions, "skills open "+name) != (name == "local-run") {
			t.Errorf("%s: actions %v", name, m["actions"])
		}
	}
	if !seen["local-run"] || !seen["zemi"] {
		t.Fatalf("listed %v", seen)
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.skillsCommand("open zemi"); !errors.Is(err, ErrUsage) {
		t.Fatalf("/skills open zemi: %v", err)
	}
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

	err := r.ExecuteChunk(t.Context(), `zemi.skills.enable("local-run")`, "project.lua", lua.OriginProject)
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
	if err := r.Boot(t.Context(), []lua.BootStep{
		h.NamespacesStep(), h.SkillsUserHooksStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if h.Commands.Lookup("booted-cmd") == nil {
		t.Fatal("user hook did not run in the boot window")
	}
}

// TestSkillCommands: the invocation fields decide whether the model sees a
// skill and whether it is a command, which yields to a builtin command and
// to one of its own origin, and replaces a less trusted one.
func TestSkillCommands(t *testing.T) {
	const asks = "d\nuser-invocable: true"
	file := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	userCmd := func(h *Host) { file(filepath.Join(h.Dirs.ConfigHome, "commands", "x.md"), "the user's x\n") }
	user := func(desc string) func(h *Host) {
		return func(h *Host) { writeSkill(t, h.userSkillsDir(), "x", desc, "") }
	}
	both := func(a, b func(h *Host)) func(h *Host) { return func(h *Host) { a(h); b(h) } }
	project := func(h *Host) { writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "skills"), "x", asks, "") }
	projectCmd := func(h *Host) {
		file(filepath.Join(h.Dirs.ProjectDir, ".agents", "commands", "x.md"), "the project's x\n")
	}
	const mine, other = "the model and /x", "the model; /x is another command"
	for _, c := range []struct {
		name      string
		skill     string // "x" when empty
		write     func(h *Host)
		toggle    []bool // after boot, each followed by the reload it asks for
		untrusted bool
		indexed   bool
		cmd       lua.Origin // of the skill's own command
		other     lua.Origin // of /<skill> when it is another command
		err       string
		used      string // its /skills detail's "used by"; "" when not listed
	}{
		{name: "plain", write: user("d"), indexed: true, used: "the model"},
		{name: "asks for a command", write: user(asks), indexed: true, cmd: lua.OriginUser, used: mine},
		{name: "hidden from the model is a command", write: user("d\ndisable-model-invocation: true"), cmd: lua.OriginUser, used: "/x"},
		{name: "hidden and no command", write: user("d\ndisable-model-invocation: true\nuser-invocable: false"), used: "/skills use only"},
		{name: "a builtin command stays", skill: "skills", write: func(h *Host) { writeSkill(t, h.userSkillsDir(), "skills", asks, "") }, indexed: true, other: lua.OriginBuiltin, err: "skills: /skills is already a builtin command", used: "the model; /skills is another command"},
		{name: "a markdown command of its origin stays", write: both(user(asks), userCmd), indexed: true, other: lua.OriginUser, err: "/x is already a user command", used: other},
		{name: "a lua command of its origin stays", write: both(user(asks), func(h *Host) {
			file(filepath.Join(h.Dirs.ConfigHome, "init.lua"), `zemi.command.add("x", function() end)`)
		}), indexed: true, other: lua.OriginUser, err: "/x is already a user command", used: other},
		{name: "a project command yields to a user skill", write: both(user(asks), projectCmd), indexed: true, cmd: lua.OriginUser, used: mine},
		{name: "a plugin skill yields to a user command", write: both(userCmd, func(h *Host) {
			writeSkill(t, filepath.Join(h.Dirs.DataHome, "plugins", "local", "p", "skills"), "x", asks, "")
		}), indexed: true, other: lua.OriginUser, err: "/x is already a user command", used: other},
		{name: "a trusted project skill", write: project, indexed: true, cmd: lua.OriginProject, used: mine},
		{name: "an untrusted project skill", write: project, untrusted: true},
		{name: "a recipe takes neither field", write: func(h *Host) {
			writeSkill(t, h.recipesDir(), "x", "d\ndisable-model-invocation: true\nuser-invocable: true", "")
		}, indexed: true, used: "the model"},
		{name: "off puts back the command it displaced", write: both(user(asks), projectCmd), toggle: []bool{false}, other: lua.OriginProject, used: "/skills use only"},
		{name: "on brings it back", write: user(asks), toggle: []bool{false, true}, indexed: true, cmd: lua.OriginUser, used: mine},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostSkillsNoBoot(t)
			c.write(h)
			h.SetProjectTrusted(!c.untrusted)
			steps := []lua.BootStep{
				h.NamespacesStep(), h.BuiltinCommandsStep(), h.PluginsStep(),
				lua.LoadFileStep("init", filepath.Join(h.Dirs.ConfigHome, "init.lua"), lua.OriginUser),
				h.SkillsUserHooksStep(), h.MarkdownCommandsStep(),
			}
			var reloads atomic.Int32
			h.SetReload(func() error {
				defer reloads.Add(1)
				return h.ForceReload(t.Context(), steps)
			}, nil)
			if err := r.Boot(t.Context(), steps); err != nil {
				t.Fatal(err)
			}
			h.SetUIHooks(&fakeUI{})
			for i, on := range c.toggle {
				if err := h.setSkillEnabled("x", on); err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() error {
					if n := reloads.Load(); n != int32(i+1) {
						return fmt.Errorf("reloads=%d", n)
					}
					return nil
				})
			}
			name := cmp.Or(c.skill, "x")
			indexed := slices.ContainsFunc(h.skillIndex(), func(s tools.SkillInfo) bool { return s.Name == name })
			mine, project := h.skillsIndexPrompt()
			if prompt := strings.Contains(mine+project, "- "+name+": "); indexed != c.indexed || prompt != c.indexed {
				t.Errorf("skill tool %v, prompt %v, want %v", indexed, prompt, c.indexed)
			}
			var cmd, other lua.Origin
			if got := h.Commands.Lookup(name); got != nil && strings.HasSuffix(got.Path, filepath.Join(name, "SKILL.md")) {
				cmd = got.Origin
			} else if got != nil {
				other = got.Origin
			}
			errs := fmt.Sprint(h.errs.entries)
			if cmd != c.cmd || other != c.other || c.err == "" && strings.Contains(errs, "skills") || !strings.Contains(errs, c.err) {
				t.Errorf("command origin %q, other %q, want %q, %q; errors %s", cmd, other, c.cmd, c.other, errs)
			}
			used := ""
			for _, it := range h.skillItems() {
				if m := it.(map[string]any); m["text"] == name && m["section"] != "Builtin" {
					used = fmt.Sprint(m["fields"])
				}
			}
			if c.used == "" && used != "" || c.used != "" && !strings.Contains(used, "[used by "+c.used+"]") {
				t.Errorf("fields %q, want used by %q", used, c.used)
			}
		})
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

	err := r.ExecuteChunk(t.Context(),
		`zemi.learn.decide("whatever", "accept")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("project-origin decide: %v", err)
	}
}

// TestSkillsUse: /skills use and a skill's own command send the command line
// as typed and the skill without frontmatter behind the marker, $ARGUMENTS
// filled, ahead of piped stdin; /skills use reaches a disabled skill too.
func TestSkillsUse(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, h.userSkillsDir(), "local-run", "run the local suite", "")
	for name, body := range map[string]string{
		"big":       "description: huge\n---\n\n" + strings.Repeat("step\n", tools.SkillMaxOut/4),
		"review-pr": "description: review a PR\nuser-invocable: true\n---\nReview PR $ARGUMENTS.\n",
	} {
		if err := os.MkdirAll(filepath.Join(h.userSkillsDir(), name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.userSkillsDir(), name, "SKILL.md"), []byte("---\nname: "+name+"\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	reflectStub(t, h, "ok")
	// a disabled skill is reviewed code: naming it is the request
	if err := h.setSkillEnabled("local-run", false); err != nil {
		t.Fatal(err)
	}
	stdin := agent.Attached("stdin", "the spec")
	for _, c := range []struct {
		name, args, typed string
		tail              []string // in order
	}{
		{"plain request", "skills use local-run make a landing page", "/skills use local-run make a landing page", []string{"<skill>", "Skill directory: ", "Body."}},
		{"line breaks survive", "skills use local-run first\n\nsecond", "/skills use local-run first\n\nsecond", []string{"Body."}},
		{"stdin after the skill", "skills use local-run fix it" + stdin, "/skills use local-run fix it", []string{"Body.", stdin}},
		{"no request", "skills use local-run", "/skills use local-run", []string{"Body."}},
		{"a big skill is cut", "skills use big", "/skills use big", []string{"Read the cut lines with the read tool"}},
		{"arguments filled", "skills use review-pr 12", "/skills use review-pr 12", []string{"Review PR 12."}},
		{"the skill's own command", "review-pr 12" + stdin, "/review-pr 12", []string{"Review PR 12.", stdin}},
	} {
		name, args, _ := strings.Cut(c.args, " ")
		if err := h.RunCommand(t.Context(), name, args); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		waitIdle(t, h)
		typed, tail := agent.CutAttached(lastUserText(t, h))
		if typed != c.typed || strings.Contains(tail, "run the local suite") {
			t.Errorf("%s: typed %q, tail:\n%s", c.name, typed, tail)
		}
		rest := tail
		for _, want := range c.tail {
			_, after, ok := strings.Cut(rest, want)
			if !ok {
				t.Errorf("%s: tail lacks %q after what came before:\n%s", c.name, want, tail)
				break
			}
			rest = after
		}
	}
	if err := h.skillsCommand("use nothing"); err == nil || !strings.Contains(err.Error(), "no skill") {
		t.Errorf("unknown skill: err = %v", err)
	}
}

// lastUserText is the live session's last stored user message, whole.
func lastUserText(t *testing.T, h *Host) string {
	t.Helper()
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range slices.Backward(rows) {
		var m fantasy.Message
		if r.Role == "user" && json.Unmarshal([]byte(r.PartsJSON), &m) == nil {
			return concatText(m.Content)
		}
	}
	t.Fatal("no user message stored")
	return ""
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

// TestSymlinkedSkillsDir: <config>/skills may be a link into a dotfiles
// tree. Its files must stay inside the read jail and classify as user
// origin, or every skill body asks and every hook is refused.
func TestSymlinkedSkillsDir(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
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

// TestAgentsHomeSkills: ~/.agents/skills adds user skills, body only, that
// lose a name clash to <config>/skills.
func TestAgentsHomeSkills(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	h.Dirs.AgentsHome = t.TempDir()
	agents := h.agentsSkillsDir()
	writeSkill(t, h.userSkillsDir(), "deploy", "the config deploy", "")
	writeSkill(t, agents, "shared", "from agents", `zemi.command.add("agents-hook", function() end)`)
	writeSkill(t, agents, "deploy", "the agents deploy", "")
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.SkillsUserHooksStep()}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	shared, _ := h.skillsReg.get("shared")
	deploy, _ := h.skillsReg.get("deploy")
	if shared.Source != skill.User || !h.skillsReg.isEnabled("shared") || deploy.Description != "the config deploy" {
		t.Errorf("shared %+v, deploy %q", shared, deploy.Description)
	}
	if h.HasUnseenErrors() {
		t.Errorf("errors %+v", h.errs.entries)
	}
	if h.Commands.Lookup("agents-hook") != nil {
		t.Error("a ~/.agents/skills hook ran")
	}
}

// TestRecipes: the steps share one host, in order. The pinned prompt lists
// the recipes it was built with; the skill tool resolves them live. A
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
		in, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, sub, "SKILL.md"), "content": "---\nname: " + name + "\ndescription: recipe " + name + "\n---\n\n" + body + "\n"}, json.Deterministic(true))
		return string(in)
	}
	// NAME=url is a command the entropy pass would cut
	tok := md("tok", "tok", "step one: TOKEN=ghp_0123456789abcdefghijklmnopqrstuv make rig\nAUTH_ISSUER=http://127.0.0.1:8099/auth/realms/local make run")
	edit, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "tok", "SKILL.md"), "old_string": "step one", "new_string": "step 1"}, json.Deterministic(true))
	late := []string{"late", "rig"}
	written := []string{"late", "rig", "tok"}
	pinned := func() string {
		toolset, _ := h.profileTools(nil)
		_, sys, ctx, _ := h.pinPrompt(nil, toolset)
		return sys + ctx
	}

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
		{name: "trusted lists it, a taken name is dropped", do: func() { h.SetProjectTrusted(true) }, prompt: []string{"rig"}, tool: []string{"rig"}},
		{name: "written mid-session loads, the prompt stays", do: func() { writeSkill(t, dir, "late", "recipe late", "") }, prompt: []string{"rig"}, tool: late},
		{name: "a reload's trust step keeps the pin", do: func() { h.SetProjectTrusted(true) }, prompt: []string{"rig"}, tool: late},
		{name: "compaction pins again", do: func() { h.compactDone(0, false) }, prompt: late, tool: late},
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
			if err := h.skillsCommand("delete late --yes"); err != nil {
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
			resp, err := all[i].Run(t.Context(), fantasy.ToolCall{ID: "t", Name: c.call, Input: c.input})
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
	if !strings.Contains(h.buildSystem(nil), "Recipes: "+dir+"\n") {
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
